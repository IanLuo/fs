package coord_test

import (
	"strings"
	"testing"

	"github.com/flagship-dev/flagship/internal/coord"
	"github.com/flagship-dev/flagship/internal/memory"
)

// reviewFixture is a development task, its review task, and both held: the state
// every gate case starts from.
func reviewFixture(t *testing.T) (*memory.Store, coord.Task, coord.Task) {
	s, _ := open(t)
	dev := add(t, s, "p")
	review := addReview(t, s, "p", dev.Tid)
	claim(t, s, "p", "s-dev", dev.Tid)
	claim(t, s, "p", "s-rev", review.Tid)
	return s, dev, review
}

// TestDevDoneNeedsAReviewTask is P0-1's acceptance test: a development task
// reports dev-done only once its review task exists. The refusal names the task
// that is missing and the verb that creates it, and the same report is accepted
// after that task is added.
func TestDevDoneNeedsAReviewTask(t *testing.T) {
	s, _ := open(t)
	dev := add(t, s, "p")
	claim(t, s, "p", "s-dev", dev.Tid)

	_, err := coord.Report(s, coord.ReportInput{Project: "p", Session: "s-dev", Task: dev.Tid, State: "dev-done"})
	if err == nil {
		t.Fatal("dev-done was recorded with no review task")
	}
	for _, want := range []string{"no review task", dev.Tid, "--review-of"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
	// The refused report left nothing behind: the round has not moved.
	st, err := coord.Status(s, coord.StatusInput{Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Projects[0].Tasks[0]; got.RoundState != "" || len(got.Evidence) != 0 {
		t.Fatalf("task = %+v, want no round state and no evidence after a refusal", got)
	}

	// With the review task in place the same report is accepted.
	addReview(t, s, "p", dev.Tid)
	report(t, s, coord.ReportInput{Project: "p", Session: "s-dev", Task: dev.Tid, State: "dev-done"})
	st, err = coord.Status(s, coord.StatusInput{Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range st.Projects[0].Tasks {
		if task.Task == dev.Tid && task.RoundState != coord.RoundReviewOpen {
			t.Fatalf("round_state = %q, want %q once the review task exists", task.RoundState, coord.RoundReviewOpen)
		}
	}
}

// TestRoundStateMarksAStalledRound: a dev-done written before the refusal above
// existed still stalls a round, and append-only records cannot rewrite it. Status
// derives the stall so a reader sees it without knowing to look.
func TestRoundStateMarksAStalledRound(t *testing.T) {
	s, _ := open(t)
	dev := add(t, s, "p")
	claim(t, s, "p", "s-dev", dev.Tid)

	// The row an older fs wrote: dev-done with no review task in existence.
	extra := `{"state":"dev-done","role":"dev","digest":"abc123"}`
	if err := s.PutEvent(memory.Scope("p"), dev.Tid, "s-dev", 1, "dev-done", extra); err != nil {
		t.Fatalf("forge the older dev-done: %v", err)
	}

	round := func() map[string]string {
		t.Helper()
		st, err := coord.Status(s, coord.StatusInput{Project: "p"})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, task := range st.Projects[0].Tasks {
			out[task.Task] = task.RoundState
		}
		return out
	}

	if got := round()[dev.Tid]; got != coord.RoundAwaitingReview {
		t.Fatalf("round_state = %q, want %q for a dev-done with no review task", got, coord.RoundAwaitingReview)
	}

	review := addReview(t, s, "p", dev.Tid)
	got := round()
	if got[dev.Tid] != coord.RoundReviewOpen || got[review.Tid] != coord.RoundReviewOpen {
		t.Fatalf("round_state = %v, want review-open on both the development task and its review", got)
	}

	// A verdict ends the wait; the round is no longer between two steps.
	claim(t, s, "p", "s-rev", review.Tid)
	report(t, s, coord.ReportInput{Project: "p", Session: "s-rev", Task: review.Tid, Role: "review", State: "fail", Note: "parser misses empty input"})
	if got := round()[review.Tid]; got != "" {
		t.Fatalf("round_state = %q, want empty once a verdict is recorded", got)
	}
}

// TestDoneRefusesWithoutReviewerRunChecks is the "missing required check" case: the
// developer's own report is not evidence of an independent check.
func TestDoneRefusesWithoutReviewerRunChecks(t *testing.T) {
	s, dev, review := reviewFixture(t)
	// The developer runs the check and reports it. It is not evidence: the run has
	// to come from the reviewer, on the review task.
	report(t, s, coord.ReportInput{
		Project: "p", Session: "s-dev", Task: dev.Tid, State: "pass",
		Command: dev.Checks[0], Exit: exit(0),
	})

	_, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid})
	if err == nil {
		t.Fatal("completion passed with no reviewer-run check")
	}
	if !strings.Contains(err.Error(), dev.Checks[0]) {
		t.Fatalf("refusal %q does not name the missing check %q", err, dev.Checks[0])
	}
	assertOpen(t, s, dev.Tid)
	assertOpen(t, s, review.Tid)
}

// TestDoneRefusesAFailingCheck: a non-zero result leaves the task incomplete and the
// record keeps the exact command and exit code that refused it.
func TestDoneRefusesAFailingCheck(t *testing.T) {
	s, dev, review := reviewFixture(t)
	runCheck(t, s, "p", "s-rev", review.Tid, dev.Checks[0], 1)

	_, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid})
	if err == nil {
		t.Fatal("completion passed with a failing check")
	}
	if !strings.Contains(err.Error(), "exit 1") {
		t.Fatalf("refusal %q does not record the exit code", err)
	}
	assertOpen(t, s, dev.Tid)

	// The refusal is recorded: the next reader does not have to re-derive why.
	st, err := coord.Status(s, coord.StatusInput{Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, task := range st.Projects[0].Tasks {
		if task.Task != review.Tid {
			continue
		}
		for _, ev := range task.Evidence {
			if ev.State == "refused" && ev.Role == "review" && ev.Command == "" && strings.Contains(ev.Note, "completion refused") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the refusal was not recorded on the review task")
	}
}

// TestCompletionRefusalIsNotAVerdict: the gate declining to close is not a
// reviewer's judgement that a check failed. It is recorded as `refused`, the task
// stays open, and a later recorded run still closes it as pass.
func TestCompletionRefusalIsNotAVerdict(t *testing.T) {
	s, dev, review := reviewFixture(t)

	// No reviewer run of the declared check: the gate declines to close.
	if _, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid}); err == nil {
		t.Fatal("completion passed with no reviewer-run check")
	}
	assertOpen(t, s, dev.Tid)
	assertOpen(t, s, review.Tid)

	// A reader must see a refusal, not a verdict: the newest word on the review is
	// `refused`, with the reviewer role and the reason.
	st, err := coord.Status(s, coord.StatusInput{Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	var got coord.TaskStatus
	for _, task := range st.Projects[0].Tasks {
		if task.Task == review.Tid {
			got = task
		}
	}
	if got.Agent.State != "refused" || got.Agent.Role != "review" {
		t.Fatalf("agent = %+v, want the reviewer's refusal, not a verdict", got.Agent)
	}
	if len(got.Evidence) == 0 || got.Evidence[0].State != "refused" {
		t.Fatalf("evidence = %+v, want the refusal as the newest word", got.Evidence)
	}

	// The same reviewer runs the check and completes: the round closes as pass.
	runCheck(t, s, "p", "s-rev", review.Tid, dev.Checks[0], 0)
	result, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid})
	if err != nil {
		t.Fatalf("a recorded run did not let completion through: %v", err)
	}
	if result.Dev != dev.Tid || result.Review != review.Tid {
		t.Fatalf("result = %+v, want both tasks closed", result)
	}
	for _, r := range rows(t, s, "p") {
		if r.Kind == memory.KindTask && (r.Task() == dev.Tid || r.Task() == review.Tid) && !r.Closed {
			t.Fatalf("task %s is not closed after the recorded run", r.Task())
		}
	}
}

// TestReviewFailIsStillAVerdict is the mirror of the refusal case: a genuine
// reviewer failure keeps its own state, reads as a verdict, and leaves the
// development task open.
func TestReviewFailIsStillAVerdict(t *testing.T) {
	s, dev, review := reviewFixture(t)
	report(t, s, coord.ReportInput{
		Project: "p", Session: "s-rev", Task: review.Tid, Role: "review",
		State: "fail", Note: "the parser misses empty input",
	})
	assertOpen(t, s, dev.Tid)
	assertOpen(t, s, review.Tid)

	st, err := coord.Status(s, coord.StatusInput{Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	var got coord.TaskStatus
	for _, task := range st.Projects[0].Tasks {
		if task.Task == review.Tid {
			got = task
		}
	}
	if got.Agent.State != "fail" || got.Agent.Role != "review" {
		t.Fatalf("agent = %+v, want the reviewer's fail still read as a verdict", got.Agent)
	}

	// A real fail still refuses completion and leaves the development task open.
	if _, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid}); err == nil {
		t.Fatal("completion passed on a failed review")
	}
	assertOpen(t, s, dev.Tid)
	assertOpen(t, s, review.Tid)
}

// TestDoneRefusesAPartialCheckSet: every declared check is required, not one of
// them. A task that declared two cannot be completed on the strength of one.
func TestDoneRefusesAPartialCheckSet(t *testing.T) {
	s, _ := open(t)
	dev, err := coord.Add(s, coord.AddInput{
		Project: "p", Goal: "two checks", Checks: []string{"go test ./...", "go vet ./..."},
	})
	if err != nil {
		t.Fatal(err)
	}
	review := addReview(t, s, "p", dev.Tid)
	claim(t, s, "p", "s-dev", dev.Tid)
	claim(t, s, "p", "s-rev", review.Tid)
	runCheck(t, s, "p", "s-rev", review.Tid, dev.Checks[0], 0)

	if _, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid}); err == nil {
		t.Fatalf("completion passed with %q unrun", dev.Checks[1])
	} else if !strings.Contains(err.Error(), dev.Checks[1]) {
		t.Fatalf("refusal %q does not name the unrun check %q", err, dev.Checks[1])
	}
}

// TestDoneRequiresADifferentSession checks independence at the gate rather than
// trusting the binding layer for it. Through fs this state cannot arise — one
// identity holds one task — so the row is written directly: a harness that reused an
// identity must be refused here, not three layers down.
func TestDoneRequiresADifferentSession(t *testing.T) {
	s, _ := open(t)
	dev := add(t, s, "p")
	review := addReview(t, s, "p", dev.Tid)
	claim(t, s, "p", "s-same", review.Tid)
	if err := s.PutClaim(memory.Scope("p"), dev.Tid, "s-same", 1); err != nil {
		t.Fatalf("forge the reused identity's claim: %v", err)
	}
	runCheck(t, s, "p", "s-same", review.Tid, dev.Checks[0], 0)

	_, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-same", Task: review.Tid})
	if err == nil {
		t.Fatal("one session developed and reviewed the same work")
	}
	if !strings.Contains(err.Error(), "different session") {
		t.Fatalf("refusal %q does not say the reviewer must be a different session", err)
	}
}

// TestDonePassesOnAPassingIndependentCheckSet is the only case that completes
// anything: the reviewer ran every declared check, each exited zero, and the record
// keeps the command, the exit code and the session.
func TestDonePassesOnAPassingIndependentCheckSet(t *testing.T) {
	s, dev, review := reviewFixture(t)
	runCheck(t, s, "p", "s-rev", review.Tid, dev.Checks[0], 0)

	result, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid})
	if err != nil {
		t.Fatalf("done: %v", err)
	}
	if result.Dev != dev.Tid || result.Review != review.Tid {
		t.Fatalf("result = %+v, want both tasks named", result)
	}
	if len(result.Checks) != 1 || result.Checks[0].Command != dev.Checks[0] ||
		result.Checks[0].Exit != 0 || result.Checks[0].Session != "s-rev" {
		t.Fatalf("checks = %+v, want the exact command, exit and reviewer session", result.Checks)
	}
	// Both rows are closed, and stay closed: completion is terminal.
	rs := rows(t, s, "p")
	for _, r := range rs {
		if r.Kind == memory.KindTask && (r.Task() == dev.Tid || r.Task() == review.Tid) && !r.Closed {
			t.Fatalf("task %s is not closed after completion", r.Task())
		}
	}
	if _, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid}); err == nil {
		t.Fatal("a completed task was completed again; it must never be reopened")
	}
	if _, err := coord.Claim(s, coord.ClaimInput{Project: "p", Session: "s-fresh", Task: dev.Tid}); err == nil {
		t.Fatal("a closed task was claimable")
	}
}

// TestDoneRerunsTheNewestResult: a reviewer that ran a check, fixed the cause and
// re-ran it reports the rerun, not the first failure.
func TestDoneRerunsTheNewestResult(t *testing.T) {
	s, _ := open(t)
	dev, err := coord.Add(s, coord.AddInput{Project: "p", Goal: "rerun", Checks: []string{"flake"}})
	if err != nil {
		t.Fatal(err)
	}
	review2 := addReview(t, s, "p", dev.Tid)
	claim(t, s, "p", "s-dev2", dev.Tid)
	claim(t, s, "p", "s-rev2", review2.Tid)
	runCheck(t, s, "p", "s-rev2", review2.Tid, dev.Checks[0], 1)
	runCheck(t, s, "p", "s-rev2", review2.Tid, dev.Checks[0], 0)

	if _, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev2", Task: review2.Tid}); err != nil {
		t.Fatalf("the newest run was not the one the gate read: %v", err)
	}
}

// assertOpen fails unless a task is still open.
func assertOpen(t *testing.T, s *memory.Store, tid string) {
	t.Helper()
	for _, r := range rows(t, s, "p") {
		if r.Kind == memory.KindTask && r.Task() == tid {
			if r.Closed {
				t.Fatalf("task %s was closed by a refusal", tid)
			}
			return
		}
	}
	t.Fatalf("no task row for %s", tid)
}
