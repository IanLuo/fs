package coord_test

import (
	"strings"
	"testing"
	"time"

	"github.com/flagship-dev/flagship/internal/coord"
	"github.com/flagship-dev/flagship/internal/memory"
)

// TestStatusReportsStateHolderAndProvenance: a reader can tell what a task is, who
// holds it, and where each status came from and when.
func TestStatusReportsStateHolderAndProvenance(t *testing.T) {
	s, now := open(t)
	task := add(t, s, "p")
	claim(t, s, "p", "s-1", task.Tid)
	report(t, s, coord.ReportInput{Project: "p", Session: "s-1", Task: task.Tid, State: "progress", Note: "half way"})

	st, err := coord.Status(s, coord.StatusInput{Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Projects) != 1 || st.Projects[0].Project != "p" || len(st.Projects[0].Tasks) != 1 {
		t.Fatalf("status = %+v, want one project with one task", st)
	}
	got := st.Projects[0].Tasks[0]
	if got.State != "claimed" || got.Session != "s-1" || got.Round != 1 || !got.LeaseLive {
		t.Fatalf("task = %+v, want claimed by s-1 at round 1", got)
	}
	if got.LeaseExpires == nil || !got.LeaseExpires.Equal(now.Add(memory.DefaultTTL)) {
		t.Fatalf("lease_expires = %v, want %v", got.LeaseExpires, now.Add(memory.DefaultTTL))
	}
	if got.Agent.State != "progress" || got.Agent.Source != "fs.event" || !got.Agent.Fresh {
		t.Fatalf("agent = %+v, want the live report from fs.event", got.Agent)
	}
	if got.Agent.At == nil || got.Agent.Age == nil {
		t.Fatalf("agent = %+v, want a timestamp and an age", got.Agent)
	}
	// Nothing observed runtime health, and absence reads as unknown — never alive.
	if got.Runtime.State != "unknown" || got.Runtime.Source != "none" || got.Runtime.Fresh {
		t.Fatalf("runtime = %+v, want unknown from no source", got.Runtime)
	}
}

// TestExpiredHeartbeatIsUnknown is AC4's sharp edge: an agent that stopped writing is
// not idle, not done and not alive. It is unknown, and expiry never advanced the task.
func TestExpiredHeartbeatIsUnknown(t *testing.T) {
	s, now := open(t)
	task := add(t, s, "p")
	claim(t, s, "p", "s-1", task.Tid)
	report(t, s, coord.ReportInput{Project: "p", Session: "s-1", Task: task.Tid, State: "pass", Note: "looks done"})

	*now = now.Add(memory.DefaultTTL + time.Second)
	st, err := coord.Status(s, coord.StatusInput{Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	got := st.Projects[0].Tasks[0]
	if got.Agent.State != "unknown" {
		t.Fatalf("an expired heartbeat read as %q; it must be unknown", got.Agent.State)
	}
	if got.Agent.Fresh {
		t.Fatal("an expired heartbeat was reported fresh")
	}
	if got.State != "open" || got.LeaseLive {
		t.Fatalf("task = %+v, want open with a lapsed lease, not done", got)
	}
	if got.Agent.At == nil || got.Agent.Age == nil {
		t.Fatal("the expired observation lost its timestamp; staleness must stay visible")
	}
	// The verdict is unknown, but the fact it was computed from survives: otherwise a
	// finished agent and a vanished one look identical.
	if got.Agent.Reported != "pass" {
		t.Fatalf("reported = %q, want the last state reported (pass)", got.Agent.Reported)
	}
}

// TestExpiredRuntimeIsUnknown: a harness report that stopped is unknown too.
func TestExpiredRuntimeIsUnknown(t *testing.T) {
	s, now := open(t)
	task := add(t, s, "p")
	claim(t, s, "p", "s-1", task.Tid)
	report(t, s, coord.ReportInput{Project: "p", Session: "s-1", Task: task.Tid, State: "progress"})
	report(t, s, coord.ReportInput{Project: "p", Session: "s-1", Task: task.Tid, State: "progress", Role: "runtime"})

	st, err := coord.Status(s, coord.StatusInput{Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Projects[0].Tasks[0].Runtime; got.State != "progress" || !got.Fresh {
		t.Fatalf("runtime = %+v, want the live harness report", got)
	}

	*now = now.Add(memory.DefaultTTL + time.Second)
	st, err = coord.Status(s, coord.StatusInput{Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Projects[0].Tasks[0].Runtime; got.State != "unknown" || got.Fresh {
		t.Fatalf("runtime = %+v, want unknown once the harness stops reporting", got)
	}
}

// TestAggregateCrossesProjectsAndASessionDoesNot: the user asks about everything; an
// agent asks about what its binding authorizes, and nothing else.
func TestAggregateCrossesProjectsAndASessionDoesNot(t *testing.T) {
	s, _ := open(t)
	a := add(t, s, "alpha")
	add(t, s, "beta")
	claim(t, s, "alpha", "s-1", a.Tid)

	all, err := coord.Status(s, coord.StatusInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Projects) != 2 || all.Projects[0].Project != "alpha" || all.Projects[1].Project != "beta" {
		t.Fatalf("aggregate = %+v, want alpha and beta", all)
	}

	// An agent bound in alpha sees its own task only.
	own, err := coord.Status(s, coord.StatusInput{Session: "s-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(own.Projects) != 1 || own.Projects[0].Project != "alpha" || len(own.Projects[0].Tasks) != 1 {
		t.Fatalf("a bound session saw %+v, want only its own task in alpha", own)
	}
	if _, err := coord.Status(s, coord.StatusInput{Project: "beta", Session: "s-1"}); err == nil {
		t.Fatal("a session bound in alpha read beta")
	} else if !strings.Contains(err.Error(), "alpha") {
		t.Fatalf("refusal %q does not say where the session is bound", err)
	}

	// An unbound session may look for work in a named project, but not aggregate.
	if _, err := coord.Status(s, coord.StatusInput{Session: "s-new"}); err == nil {
		t.Fatal("an unbound session read every project")
	}
	free, err := coord.Status(s, coord.StatusInput{Session: "s-new", Project: "beta"})
	if err != nil {
		t.Fatalf("unbound discovery of one project: %v", err)
	}
	if len(free.Projects) != 1 || len(free.Projects[0].Tasks) != 1 {
		t.Fatalf("discovery = %+v, want beta's one task", free)
	}
}

// TestStatusShowsDoneAndEvidence: a completed task reads done, and the checks that
// closed it stay visible with their exact commands and exits.
func TestStatusShowsDoneAndEvidence(t *testing.T) {
	s, _ := open(t)
	dev := add(t, s, "p")
	review := addReview(t, s, "p", dev.Tid)
	claim(t, s, "p", "s-dev", dev.Tid)
	devDone(t, s, "p", "s-dev", dev.Tid)
	claim(t, s, "p", "s-rev", review.Tid)
	runCheck(t, s, "p", "s-rev", review.Tid, dev.Checks[0], 0)
	if _, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid}); err != nil {
		t.Fatal(err)
	}

	st, err := coord.Status(s, coord.StatusInput{Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]coord.TaskStatus{}
	for _, task := range st.Projects[0].Tasks {
		states[task.Task] = task
	}
	if states[dev.Tid].State != "done" || states[review.Tid].State != "done" {
		t.Fatalf("states = %v, want both done", states)
	}
	// The reviewer's run stays visible after the close: closing adds a row to the
	// evidence, it does not replace what the verdict rests on.
	ev := states[review.Tid].Evidence
	var passing bool
	for _, got := range ev {
		if got.State == "pass" && got.Command == dev.Checks[0] && got.Exit != nil && *got.Exit == 0 && got.Session == "s-rev" {
			passing = true
		}
	}
	if !passing {
		t.Fatalf("evidence = %+v, want the reviewer's run with its command, exit and session", ev)
	}
}

// TestAVerdictNamesItsRound: a verdict is a fact about one round, and the claim rows
// already carry it, so the verdict row does too. Without it a round that needed four
// attempts and a round that needed one read the same, and a spinning round looks like
// a slow one. The task's round is the claim's, live or lapsed, and empty on a task
// nobody ever claimed.
func TestAVerdictNamesItsRound(t *testing.T) {
	s, now := open(t)
	dev := add(t, s, "p")
	review := addReview(t, s, "p", dev.Tid)
	claim(t, s, "p", "s-dev", dev.Tid)
	devDone(t, s, "p", "s-dev", dev.Tid)
	claim(t, s, "p", "s-rev", review.Tid)
	// The lease lapses before the review finishes; the same session re-claims for
	// round 2, which is the recovery the prompt names.
	*now = now.Add(2 * memory.DefaultTTL)
	if got := claim(t, s, "p", "s-rev", review.Tid); got.Round != 2 {
		t.Fatalf("round = %d, want 2", got.Round)
	}
	runCheck(t, s, "p", "s-rev", review.Tid, dev.Checks[0], 0)

	byTask := statusByTask(t, s, "p")
	ev := byTask[review.Tid].Evidence
	if len(ev) == 0 || ev[0].State != "pass" || ev[0].Round != 2 {
		t.Fatalf("evidence = %+v, want the newest verdict to carry round 2", ev)
	}
	if byTask[review.Tid].Round != 2 {
		t.Errorf("round = %d, want 2", byTask[review.Tid].Round)
	}

	// A closed task keeps the round of its last claim: the claim row outlives the
	// close, and nothing rewrites it. The lease lapses and the round stays readable.
	if _, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid}); err != nil {
		t.Fatalf("done: %v", err)
	}
	*now = now.Add(2 * memory.DefaultTTL)
	byTask = statusByTask(t, s, "p")
	if got := byTask[review.Tid]; got.State != "done" || got.Round != 2 || got.LeaseLive {
		t.Errorf("closed review = %s at round %d (lease live %v), want done at round 2 with no live lease", got.State, got.Round, got.LeaseLive)
	}

	// A task nobody claimed has no round to name.
	hand := add(t, s, "p")
	if got := statusByTask(t, s, "p")[hand.Tid].Round; got != 0 {
		t.Errorf("unclaimed task round = %d, want none", got)
	}
}

// TestARefusalNamesItsRound: the gate declining to close writes `refused`, not a
// verdict, and it names the round it declined — otherwise a round refused three
// times reads as one refusal.
func TestARefusalNamesItsRound(t *testing.T) {
	s, _ := open(t)
	dev := add(t, s, "p")
	review := addReview(t, s, "p", dev.Tid)
	claim(t, s, "p", "s-dev", dev.Tid)
	claim(t, s, "p", "s-rev", review.Tid)
	if _, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid}); err == nil {
		t.Fatal("completion passed with no reviewer-run check")
	}
	ev := statusByTask(t, s, "p")[review.Tid].Evidence
	if len(ev) == 0 || ev[0].State != "refused" || ev[0].Round != 1 {
		t.Fatalf("evidence = %+v, want the refusal to carry round 1", ev)
	}
}

// statusByTask reads one project's status and indexes it by task.
func statusByTask(t *testing.T, s *memory.Store, project string) map[string]coord.TaskStatus {
	t.Helper()
	st, err := coord.Status(s, coord.StatusInput{Project: project})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	out := map[string]coord.TaskStatus{}
	for _, task := range st.Projects[0].Tasks {
		out[task.Task] = task
	}
	return out
}

// TestStatusCarriesTheDevelopmentReport: when a session dies its context is gone,
// so the next session reads what it left. The note and digest are the only record
// of where the work stopped, and they have to be in status, newest first, with the
// session that wrote them.
func TestStatusCarriesTheDevelopmentReport(t *testing.T) {
	s, _ := open(t)
	task := add(t, s, "p")
	// A review task exists because the round cannot reach dev-done without one.
	addReview(t, s, "p", task.Tid)
	claim(t, s, "p", "s-dev", task.Tid)
	report(t, s, coord.ReportInput{Project: "p", Session: "s-dev", Task: task.Tid, State: "progress",
		Note: "ast.rs next", Digest: "1 test red: TestParseUnary"})
	report(t, s, coord.ReportInput{Project: "p", Session: "s-dev", Task: task.Tid, State: "dev-done",
		Note: "parser wired", Digest: "all green"})

	st, err := coord.Status(s, coord.StatusInput{Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	var ev []coord.Evidence
	for _, got := range st.Projects[0].Tasks {
		if got.Task == task.Tid {
			ev = got.Evidence
		}
	}
	if len(ev) != 2 {
		t.Fatalf("evidence = %+v, want both development reports", ev)
	}
	if ev[0].State != "dev-done" || ev[0].Note != "parser wired" || ev[0].Digest != "all green" {
		t.Errorf("newest report = %+v, want the stopping point", ev[0])
	}
	if ev[1].Note != "ast.rs next" || ev[1].Digest != "1 test red: TestParseUnary" {
		t.Errorf("earlier report = %+v, want the progress one", ev[1])
	}
	if ev[0].Role != "dev" || ev[0].Session != "s-dev" {
		t.Errorf("provenance = %+v, want the development role and the session that wrote it", ev[0])
	}
}

// TestStatusIsStableAcrossRuns: two identical questions get byte-identical answers,
// so a reader can diff them.
func TestStatusIsStableAcrossRuns(t *testing.T) {
	s, _ := open(t)
	add(t, s, "zeta")
	add(t, s, "alpha")
	first, err := coord.Status(s, coord.StatusInput{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := coord.Status(s, coord.StatusInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Projects) != len(second.Projects) {
		t.Fatalf("two runs saw %d and %d projects", len(first.Projects), len(second.Projects))
	}
	for i := range first.Projects {
		if first.Projects[i].Project != second.Projects[i].Project {
			t.Fatalf("project order moved between runs: %q then %q", first.Projects[i].Project, second.Projects[i].Project)
		}
	}
}
