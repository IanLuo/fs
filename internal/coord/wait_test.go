package coord_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/flagship-dev/flagship/internal/coord"
	"github.com/flagship-dev/flagship/internal/memory"
)

// sessionFromPrompt reads the identity fs minted for the agent out of the prompt.
// It is how the fake agent knows what to claim as.
func sessionFromPrompt(t *testing.T, prompt string) string {
	t.Helper()
	fields := strings.Fields(prompt)
	for i, f := range fields {
		if f == "session" && i+1 < len(fields) {
			return strings.TrimSuffix(fields[i+1], ".")
		}
	}
	t.Fatalf("prompt names no session:\n%s", prompt)
	return ""
}

// launchOutcome reads the outcome recorded on the newest launch row for a task.
func launchOutcome(t *testing.T, s *memory.Store, project, tid string) string {
	t.Helper()
	var newest string
	var at time.Time
	for _, r := range launchRows(t, s, project) {
		if r.Task() != tid {
			continue
		}
		var ex struct {
			Outcome string    `json:"outcome"`
			At      time.Time `json:"at"`
		}
		if err := json.Unmarshal([]byte(r.Extra), &ex); err != nil {
			t.Fatalf("launch extra: %v", err)
		}
		if ex.At.After(at) {
			at, newest = ex.At, ex.Outcome
		}
	}
	return newest
}

// TestDispatchWaitsForTheAgentToClaim: a started pane is not evidence that the
// agent took the prompt. The dispatch succeeds because a row says the agent
// claimed, and the pane is left alone.
func TestDispatchWaitsForTheAgentToClaim(t *testing.T) {
	s, _ := open(t)
	task := add(t, s, "p")
	fake := &fakeLauncher{}
	fake.onStart = func(spec coord.LaunchSpec) {
		sid := sessionFromPrompt(t, spec.Prompt)
		if _, err := coord.Claim(s, coord.ClaimInput{Project: "p", Session: sid, Task: task.Tid}); err != nil {
			t.Fatalf("agent claim: %v", err)
		}
	}

	got, err := coord.Dispatch(s, coord.DispatchInput{
		Project: "p", Task: task.Tid, Dir: t.TempDir(), Wait: 2 * time.Second,
	}, fake)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got.Unconfirmed {
		t.Error("a claimed task was reported unconfirmed")
	}
	if len(fake.started) != 1 {
		t.Fatalf("started %d agents, want 1", len(fake.started))
	}
	if got := launchOutcome(t, s, "p", task.Tid); got != coord.OutcomeStarted {
		t.Errorf("launch outcome = %q, want %q", got, coord.OutcomeStarted)
	}
	if len(fake.closed) != 0 {
		t.Errorf("dispatch closed a pane it should have left open: %v", fake.closed)
	}
}

// TestDispatchAcceptsEvidenceWithoutAClaim: the agent may write a signal before
// it claims. Any evidence from the dispatched session is proof it woke.
func TestDispatchAcceptsEvidenceWithoutAClaim(t *testing.T) {
	s, _ := open(t)
	task := add(t, s, "p")
	fake := &fakeLauncher{}
	fake.onStart = func(spec coord.LaunchSpec) {
		sid := sessionFromPrompt(t, spec.Prompt)
		if err := s.PutEvent(memory.Scope("p"), task.Tid, sid, 1, "progress", `{"state":"progress","role":"dev"}`); err != nil {
			t.Fatalf("agent evidence: %v", err)
		}
	}

	got, err := coord.Dispatch(s, coord.DispatchInput{
		Project: "p", Task: task.Tid, Dir: t.TempDir(), Wait: 2 * time.Second,
	}, fake)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got.Unconfirmed {
		t.Error("a session that wrote evidence was reported unconfirmed")
	}
}

// TestDispatchRefusesAnAgentThatNeverWakes: nothing claimed and nothing written
// inside the window is a refusal. The launch row says unconfirmed, the pane stays
// open because a slow start must not be killed, and one line names the pane and
// the session.
func TestDispatchRefusesAnAgentThatNeverWakes(t *testing.T) {
	s, _ := open(t)
	task := add(t, s, "p")
	fake := &fakeLauncher{}

	_, err := coord.Dispatch(s, coord.DispatchInput{
		Project: "p", Task: task.Tid, Dir: t.TempDir(), Wait: 20 * time.Millisecond,
	}, fake)
	if err == nil {
		t.Fatal("an agent that never woke was reported as started")
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("refusal is more than one line: %q", err)
	}
	if !strings.Contains(err.Error(), "pane-fs-") || !strings.Contains(err.Error(), "session d-") {
		t.Errorf("refusal does not name the pane and the session: %v", err)
	}
	if got := launchOutcome(t, s, "p", task.Tid); got != coord.OutcomeUnconfirmed {
		t.Errorf("launch outcome = %q, want %q", got, coord.OutcomeUnconfirmed)
	}
	if len(fake.closed) != 0 {
		t.Errorf("dispatch closed the slow pane: %v", fake.closed)
	}
	if len(fake.status) != 1 {
		t.Errorf("the pane is not still open: %v", fake.status)
	}
}

// TestDispatchNoWaitMarksUnconfirmed: fire-and-forget returns without waiting and
// says so, so a reader never mistakes a started process for a working agent.
func TestDispatchNoWaitMarksUnconfirmed(t *testing.T) {
	s, _ := open(t)
	task := add(t, s, "p")
	fake := &fakeLauncher{}

	started := time.Now()
	got, err := coord.Dispatch(s, coord.DispatchInput{
		Project: "p", Task: task.Tid, Dir: t.TempDir(), NoWait: true,
	}, fake)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("--no-wait took %s", elapsed)
	}
	if !got.Unconfirmed {
		t.Error("--no-wait did not mark the launch unconfirmed")
	}
	if got := launchOutcome(t, s, "p", task.Tid); got != coord.OutcomeStarted {
		t.Errorf("launch outcome = %q, want %q: --no-wait keeps the accepted launch", got, coord.OutcomeStarted)
	}
}

// TestDispatchAllowsRedispatchAfterAnUnconfirmedLaunch: the no-retry rule is
// about a launcher that fails, not an agent that never woke. The second dispatch
// names the attempt it replaces.
func TestDispatchAllowsRedispatchAfterAnUnconfirmedLaunch(t *testing.T) {
	s, now := open(t)
	task := add(t, s, "p")
	fake := &fakeLauncher{}

	_, err := coord.Dispatch(s, coord.DispatchInput{
		Project: "p", Task: task.Tid, Dir: t.TempDir(), Wait: 20 * time.Millisecond,
	}, fake)
	if err == nil {
		t.Fatal("the first agent never woke but dispatch succeeded")
	}
	if len(fake.started) != 1 {
		t.Fatalf("started %d agents, want 1", len(fake.started))
	}

	*now = now.Add(time.Second) // a re-dispatch happens in a later second
	fake.onStart = func(spec coord.LaunchSpec) {
		sid := sessionFromPrompt(t, spec.Prompt)
		if _, err := coord.Claim(s, coord.ClaimInput{Project: "p", Session: sid, Task: task.Tid}); err != nil {
			t.Fatalf("second agent claim: %v", err)
		}
	}
	second, err := coord.Dispatch(s, coord.DispatchInput{
		Project: "p", Task: task.Tid, Dir: t.TempDir(), Wait: 2 * time.Second,
	}, fake)
	if err != nil {
		t.Fatalf("re-dispatch after an unconfirmed launch: %v", err)
	}
	if second.Previous == nil {
		t.Fatal("the re-dispatch did not name the earlier attempt")
	}
	if !strings.Contains(second.Previous.Session, "d-") || second.Previous.Pane == "" {
		t.Errorf("previous attempt = %+v, want the session and pane", second.Previous)
	}
	if want := "pane-" + fake.started[0].Name; second.Previous.Pane != want {
		t.Errorf("previous pane = %q, want %q", second.Previous.Pane, want)
	}
	if second.Session == second.Previous.Session {
		t.Errorf("re-dispatch reused session %s", second.Session)
	}
}
