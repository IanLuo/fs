package coord_test

import (
	"strings"
	"testing"
	"time"

	"github.com/flagship-dev/flagship/internal/coord"
	"github.com/flagship-dev/flagship/internal/memory"
)

// fakeLauncher records what fs asked it to start and answers Status from a map,
// so a test can say "this agent is still working" without a terminal.
type fakeLauncher struct {
	started  []coord.LaunchSpec
	status   map[string]string // pane -> working|idle|gone
	closed   []string
	startErr error
}

func (f *fakeLauncher) Start(spec coord.LaunchSpec) (coord.Handle, error) {
	if f.startErr != nil {
		return coord.Handle{}, f.startErr
	}
	f.started = append(f.started, spec)
	pane := "pane-" + spec.Name
	if f.status == nil {
		f.status = map[string]string{}
	}
	if _, ok := f.status[pane]; !ok {
		f.status[pane] = coord.StatusWorking
	}
	return coord.Handle{Pane: pane, Name: spec.Name, Dir: spec.Dir}, nil
}

func (f *fakeLauncher) Status(h coord.Handle) (string, error) {
	if s, ok := f.status[h.Pane]; ok {
		return s, nil
	}
	return coord.StatusGone, nil
}

func (f *fakeLauncher) Close(h coord.Handle) error {
	f.closed = append(f.closed, h.Pane)
	delete(f.status, h.Pane)
	return nil
}

// launchRows counts the launch records in a project.
func launchRows(t *testing.T, s *memory.Store, project string) []memory.Row {
	t.Helper()
	var out []memory.Row
	for _, r := range rows(t, s, project) {
		if r.Kind == memory.KindLaunch {
			out = append(out, r)
		}
	}
	return out
}

// TestDispatchStartsOneAgentAndRecordsIt: fs chooses the identity, hands it over in
// the prompt, and records what it started — because closing a pane is only safe
// when there is a record that fs opened it.
func TestDispatchStartsOneAgentAndRecordsIt(t *testing.T) {
	s, _ := open(t)
	task := add(t, s, "p")
	fake := &fakeLauncher{}

	got, err := coord.Dispatch(s, coord.DispatchInput{Project: "p", Task: task.Tid, Kind: "pi", Dir: t.TempDir()}, fake)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(fake.started) != 1 {
		t.Fatalf("started %d agents, want 1", len(fake.started))
	}
	if !strings.Contains(fake.started[0].Prompt, "--session "+got.Session+" --task "+task.Tid) {
		t.Errorf("prompt does not hand over the identity:\n%s", fake.started[0].Prompt)
	}
	if !strings.Contains(fake.started[0].Prompt, "fs claim") || !strings.Contains(fake.started[0].Prompt, "fs report") {
		t.Errorf("prompt does not say how to claim and report:\n%s", fake.started[0].Prompt)
	}
	recs := launchRows(t, s, "p")
	if len(recs) != 1 {
		t.Fatalf("launch records = %d, want 1", len(recs))
	}
	if want := memory.LaunchKey(task.Tid, got.Session); recs[0].OriginID != want {
		t.Errorf("launch key = %q, want %q", recs[0].OriginID, want)
	}
	if !strings.Contains(recs[0].Extra, got.Pane) {
		t.Errorf("launch record does not name the pane: %s", recs[0].Extra)
	}
}

// TestDispatchRefusesWhatItMustNotStart: every refusal happens before a process
// exists, so a mistake cannot leave an orphan pane behind.
func TestDispatchRefusesWhatItMustNotStart(t *testing.T) {
	s, _ := open(t)
	task := add(t, s, "p")
	claim(t, s, "p", "s-1", task.Tid)

	if _, err := coord.Dispatch(s, coord.DispatchInput{Project: "p", Task: task.Tid}, &fakeLauncher{}); err == nil {
		t.Error("dispatched a task that is already held")
	} else if !strings.Contains(err.Error(), "already being worked on") {
		t.Errorf("refusal = %v, want the holder named", err)
	}

	other := add(t, s, "p")
	if _, err := coord.Dispatch(s, coord.DispatchInput{Project: "p", Task: other.Tid}, &fakeLauncher{}); err == nil {
		t.Error("dispatched a task that records no path")
	} else if !strings.Contains(err.Error(), "records no path") {
		t.Errorf("refusal = %v, want the missing path named", err)
	}
}

// TestDispatchRefusalLeavesNoRecord: a launcher that cannot start must not be
// retried and must not leave a launch record claiming it did.
func TestDispatchRefusalLeavesNoRecord(t *testing.T) {
	s, now := open(t)
	task, err := coord.Add(s, coord.AddInput{Project: "p", Goal: "g", Checks: []string{"true"}, Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_ = now
	fake := &fakeLauncher{startErr: errLauncher{}}
	if _, err := coord.Dispatch(s, coord.DispatchInput{Project: "p", Task: task.Tid}, fake); err == nil {
		t.Fatal("a failing launcher was not reported")
	}
	if len(launchRows(t, s, "p")) != 0 {
		t.Error("a failed launch wrote a record")
	}
	if len(fake.started) != 0 {
		t.Error("the launcher was asked twice")
	}
}

type errLauncher struct{}

func (errLauncher) Error() string { return "no such launcher" }

// TestDispatchPinsTheStore: a pane inherits the launcher's environment, not the
// caller's, so the prompt must name the store fs is reading. Without it fs starts an
// agent that claims in a different store and the round never begins.
func TestDispatchPinsTheStore(t *testing.T) {
	s, _ := open(t)
	task := add(t, s, "p")
	fake := &fakeLauncher{}
	if _, err := coord.Dispatch(s, coord.DispatchInput{Project: "p", Task: task.Tid, Dir: t.TempDir()}, fake); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !strings.Contains(fake.started[0].Prompt, "export MEMSTORE_DB="+s.Path()) {
		t.Errorf("prompt does not pin the store %q:\n%s", s.Path(), fake.started[0].Prompt)
	}
}

// TestDispatchRefusesASecondLaunch: a dispatch that runs twice — a double click, a
// retried command — must not put two agents on one task. The first agent may not
// have claimed yet, so the launch record, not the claim, is what says so.
func TestDispatchRefusesASecondLaunch(t *testing.T) {
	s, now := open(t)
	task := add(t, s, "p")
	fake := &fakeLauncher{}
	if _, err := coord.Dispatch(s, coord.DispatchInput{Project: "p", Task: task.Tid, Dir: t.TempDir()}, fake); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := coord.Dispatch(s, coord.DispatchInput{Project: "p", Task: task.Tid, Dir: t.TempDir()}, fake); err == nil {
		t.Fatal("dispatched the same task twice while the first agent is young")
	}
	if len(fake.started) != 1 {
		t.Fatalf("started %d agents, want 1", len(fake.started))
	}
	// After the lease the recovery is a new dispatch, which is how a dead round is
	// replaced.
	*now = now.Add(2 * memory.DefaultTTL)
	if _, err := coord.Dispatch(s, coord.DispatchInput{Project: "p", Task: task.Tid, Dir: t.TempDir()}, fake); err != nil {
		t.Fatalf("re-dispatch after the lease lapsed: %v", err)
	}
}

// TestDispatchAsksAReviewerForChecks: a review's prompt names every declared check
// and requires the exit code, because fs's gate accepts nothing less.
func TestDispatchAsksAReviewerForChecks(t *testing.T) {
	s, _ := open(t)
	dev := add(t, s, "p")
	review := addReview(t, s, "p", dev.Tid)
	fake := &fakeLauncher{}
	if _, err := coord.Dispatch(s, coord.DispatchInput{Project: "p", Task: review.Tid, Dir: t.TempDir()}, fake); err != nil {
		t.Fatalf("dispatch review: %v", err)
	}
	prompt := fake.started[0].Prompt
	for _, want := range []string{"independent reviewer", dev.Checks[0], "--role review", "--exit", "not trust the task text"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("reviewer prompt is missing %q:\n%s", want, prompt)
		}
	}
}

// TestReapClosesOnlyFinishedRounds: the round is over when the task closed, the
// reviewer recorded its verdict, or the lease lapsed. A working agent is never
// closed, and a launch fs cannot see is kept rather than killed.
func TestReapClosesOnlyFinishedRounds(t *testing.T) {
	s, _ := open(t)
	fake := &fakeLauncher{}

	// fs starts a reviewer; it passes its checks and closes the task.
	dev := add(t, s, "p")
	review := addReview(t, s, "p", dev.Tid)
	claim(t, s, "p", "s-dev", dev.Tid)
	started, err := coord.Dispatch(s, coord.DispatchInput{Project: "p", Task: review.Tid, Dir: t.TempDir()}, fake)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := coord.Claim(s, coord.ClaimInput{Project: "p", Session: started.Session, Task: review.Tid}); err != nil {
		t.Fatal(err)
	}
	runCheck(t, s, "p", started.Session, review.Tid, dev.Checks[0], 0)
	if _, err := coord.Done(s, coord.DoneInput{Project: "p", Session: started.Session, Task: review.Tid}); err != nil {
		t.Fatal(err)
	}
	fake.status[started.Pane] = coord.StatusIdle

	// A second launch, on a fresh task, whose agent is still working.
	busy := add(t, s, "p")
	busyLaunched, err := coord.Dispatch(s, coord.DispatchInput{Project: "p", Task: busy.Tid, Dir: t.TempDir()}, fake)
	if err != nil {
		t.Fatalf("dispatch busy: %v", err)
	}
	fake.status[busyLaunched.Pane] = coord.StatusWorking

	res, err := coord.Reap(s, coord.ReapInput{Project: "p"}, fake)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	closed := map[string]string{}
	for _, c := range res.Closed {
		closed[c.Task] = c.Reason
	}
	if _, ok := closed[review.Tid]; !ok {
		t.Errorf("a closed task's agent was not closed: %+v", res)
	}
	if _, ok := closed[busy.Tid]; ok {
		t.Errorf("a working agent was closed: %+v", res)
	}
	kept := false
	for _, k := range res.Kept {
		if k.Task == busy.Tid && k.Reason == "still working" {
			kept = true
		}
	}
	if !kept {
		t.Errorf("the working agent was not reported as kept: %+v", res)
	}
}

// TestReapClosesALapsedRound: a session that stopped writing left a pane and no
// verdict; reap ends it, so a dead round does not sit in the pane list forever.
func TestReapClosesALapsedRound(t *testing.T) {
	s, now := open(t)
	task, err := coord.Add(s, coord.AddInput{Project: "p", Goal: "g", Checks: []string{"true"}, Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeLauncher{}
	got, err := coord.Dispatch(s, coord.DispatchInput{Project: "p", Task: task.Tid}, fake)
	if err != nil {
		t.Fatal(err)
	}
	// The agent claims, then goes quiet past the lease without a verdict.
	if _, err := coord.Claim(s, coord.ClaimInput{Project: "p", Session: got.Session, Task: task.Tid}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * memory.DefaultTTL)
	// The agent is idle: it stopped, which is what an expired lease plus an idle
	// pane means.
	fake.status[got.Pane] = coord.StatusIdle

	res, err := coord.Reap(s, coord.ReapInput{Project: "p"}, fake)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Closed) != 1 || res.Closed[0].Reason != "lease lapsed" {
		t.Fatalf("reap = %+v, want one close for a lapsed lease", res)
	}
	if len(fake.closed) != 1 {
		t.Errorf("launcher closed %d panes, want 1", len(fake.closed))
	}
}

// TestReapNeverTouchesAPaneFsDidNotOpen: no launch record, no close. The panes a
// human opened are the human's.
func TestReapNeverTouchesAPaneFsDidNotOpen(t *testing.T) {
	s, _ := open(t)
	task := add(t, s, "p")
	claim(t, s, "p", "s-human", task.Tid) // someone is working, by hand
	fake := &fakeLauncher{}
	res, err := coord.Reap(s, coord.ReapInput{Project: "p"}, fake)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Closed) != 0 || len(fake.closed) != 0 {
		t.Fatalf("reap closed a pane with no launch record: %+v", res)
	}
}

// TestReapClosesAReviewThatEndedInFeedback: a fail verb ends the round, and the
// pane goes with it. The next round is a new dispatch, not the same pane.
func TestReapClosesAReviewThatEndedInFeedback(t *testing.T) {
	s, _ := open(t)
	dev := add(t, s, "p")
	review := addReview(t, s, "p", dev.Tid)
	claim(t, s, "p", "s-dev", dev.Tid)
	fake := &fakeLauncher{}
	got, err := coord.Dispatch(s, coord.DispatchInput{Project: "p", Task: review.Tid, Dir: t.TempDir()}, fake)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Claim(s, coord.ClaimInput{Project: "p", Session: got.Session, Task: review.Tid}); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Report(s, coord.ReportInput{
		Project: "p", Session: got.Session, Task: review.Tid, Role: "review",
		State: "fail", Note: "the parser misses empty input",
	}); err != nil {
		t.Fatal(err)
	}
	fake.status[got.Pane] = coord.StatusIdle

	res, err := coord.Reap(s, coord.ReapInput{Project: "p"}, fake)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Closed) != 1 || !strings.Contains(res.Closed[0].Reason, "fail") {
		t.Fatalf("reap = %+v, want one close with the verdict named", res)
	}
}

var _ = time.Now
