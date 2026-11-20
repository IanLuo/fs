package coord_test

import (
	"strings"
	"testing"
	"time"

	"github.com/flagship-dev/flagship/internal/coord"
	"github.com/flagship-dev/flagship/internal/memory"
)

// TestDoneRefusesWithoutTheThreeAnswers is item A's acceptance test: a review that
// ran the declared checks but answered none of the three questions does not close,
// and the refusal names each unanswered slot and the verb that answers it.
func TestDoneRefusesWithoutTheThreeAnswers(t *testing.T) {
	s, dev, review := reviewFixture(t)
	runCheck(t, s, "p", "s-rev", review.Tid, dev.Checks[0], 0)

	_, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid})
	if err == nil {
		t.Fatal("completion passed with none of the three questions answered")
	}
	for _, want := range []string{"spec", "tests", "regression", "--slot", "fs report"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
	assertOpen(t, s, dev.Tid)
	assertOpen(t, s, review.Tid)

	// The refusal is on the record, so the next reader does not re-derive it.
	ev := newestEvidence(t, s, "p", review.Tid)
	if ev.State != "refused" || !strings.Contains(ev.Note, "spec") {
		t.Fatalf("newest evidence = %+v, want the gate's refusal naming a slot", ev)
	}
}

// TestDoneClosesOnThreeAnswers: one passing answer to each question at the
// developer's tree closes the round, and the close reports the three answers.
func TestDoneClosesOnThreeAnswers(t *testing.T) {
	s, dev, review := reviewFixture(t)
	runCheck(t, s, "p", "s-rev", review.Tid, dev.Checks[0], 0)
	slotPass(t, s, "p", "s-rev", review.Tid, fixtureDigest)

	result, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid})
	if err != nil {
		t.Fatalf("three answers did not close the round: %v", err)
	}
	if len(result.Slots) != 3 {
		t.Fatalf("slots = %+v, want one answer per question", result.Slots)
	}
	for i, want := range []string{"spec", "tests", "regression"} {
		if result.Slots[i].Slot != want || result.Slots[i].Verdict != "pass" || result.Slots[i].Command != "check "+want {
			t.Fatalf("slots[%d] = %+v, want %s passed by its own command", i, result.Slots[i], want)
		}
	}

	// The answers stay readable on the closed round: a reader answers all three
	// questions from the records alone.
	seen := map[string]bool{}
	for _, ev := range newestTask(t, s, "p", review.Tid).Evidence {
		if ev.Slot != "" {
			seen[ev.Slot] = true
		}
	}
	for _, want := range []string{"spec", "tests", "regression"} {
		if !seen[want] {
			t.Fatalf("the closed round carries no %s answer: %+v", want, seen)
		}
	}
}

// TestSpecSlotMayBeNone: a spec slot no spec governs is recorded as a gap, with its
// reason, and that is an answer the gate accepts — the gap is visible, not invisible.
func TestSpecSlotMayBeNone(t *testing.T) {
	s, _, review := reviewFixture(t)
	runCheck(t, s, "p", "s-rev", review.Tid, "go test ./...", 0)
	report(t, s, coord.ReportInput{
		Project: "p", Session: "s-rev", Task: review.Tid, Slot: "spec",
		None: "no locked spec governs this change", Digest: fixtureDigest,
	})
	for _, slot := range []string{"tests", "regression"} {
		report(t, s, coord.ReportInput{
			Project: "p", Session: "s-rev", Task: review.Tid, Role: "review",
			State: "pass", Slot: slot, Command: "check " + slot, Exit: exit(0), Digest: fixtureDigest,
		})
	}

	result, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid})
	if err != nil {
		t.Fatalf("a recorded spec gap did not close the round: %v", err)
	}
	if len(result.Slots) != 3 || result.Slots[0].Verdict != "none" {
		t.Fatalf("slots = %+v, want the spec question answered as none", result.Slots)
	}
	if result.Slots[0].Note != "no locked spec governs this change" {
		t.Fatalf("spec answer note = %q, want the recorded reason", result.Slots[0].Note)
	}

	// The reason is a report row: a reader sees the gap and why, beside the answers.
	var none coord.Evidence
	for _, ev := range newestTask(t, s, "p", review.Tid).Evidence {
		if ev.Slot == "spec" {
			none = ev
		}
	}
	if none.State != "none" || none.Note == "" {
		t.Fatalf("spec answer = %+v, want state none with its reason", none)
	}
}

// TestNoneBelongsToTheSpecSlotOnly: "no test answers this" and "nothing broke" are
// not questions a gap can answer, so --none is refused for the other two slots.
func TestNoneBelongsToTheSpecSlotOnly(t *testing.T) {
	s, _, review := reviewFixture(t)
	for _, slot := range []string{"tests", "regression"} {
		_, err := coord.Report(s, coord.ReportInput{
			Project: "p", Session: "s-rev", Task: review.Tid, Slot: slot,
			None: "nothing to run", Digest: fixtureDigest,
		})
		if err == nil {
			t.Fatalf("--none answered the %s slot", slot)
		}
		if !strings.Contains(err.Error(), "--slot spec") {
			t.Fatalf("refusal %q does not say which slot --none belongs to", err)
		}
	}
}

// TestAnswersAtAnotherTreeDoNotAnswer: a question is about a change, so an answer
// recorded for another commit is not an answer about this one.
func TestAnswersAtAnotherTreeDoNotAnswer(t *testing.T) {
	s, dev, review := reviewFixture(t)
	runCheck(t, s, "p", "s-rev", review.Tid, dev.Checks[0], 0)
	slotPass(t, s, "p", "s-rev", review.Tid, "def5678")

	_, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid})
	if err == nil {
		t.Fatal("answers recorded for another tree closed the round")
	}
	for _, want := range []string{"def5678", fixtureDigest, "spec"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
	assertOpen(t, s, review.Tid)

	// Answered at the developer's tree, the same three questions close it.
	slotPass(t, s, "p", "s-rev", review.Tid, fixtureDigest)
	if _, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid}); err != nil {
		t.Fatalf("answers at the developer's tree did not close the round: %v", err)
	}
}

// TestAFailedAnswerIsNamed: the reason a slot may be recorded on a fail is that a
// failing question is named — not silently left in the unanswered list.
func TestAFailedAnswerIsNamed(t *testing.T) {
	s, _, review := reviewFixture(t)
	runCheck(t, s, "p", "s-rev", review.Tid, "go test ./...", 0)
	for _, slot := range []string{"spec", "tests"} {
		report(t, s, coord.ReportInput{
			Project: "p", Session: "s-rev", Task: review.Tid, Role: "review",
			State: "pass", Slot: slot, Command: "check " + slot, Exit: exit(0), Digest: fixtureDigest,
		})
	}
	report(t, s, coord.ReportInput{
		Project: "p", Session: "s-rev", Task: review.Tid, Role: "review",
		State: "fail", Slot: "regression", Command: "go test -race ./...", Exit: exit(1),
		Note:   "the new test fails without the change and passes with it, but the race run hangs",
		Digest: fixtureDigest,
	})

	_, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid})
	if err == nil {
		t.Fatal("a failed question closed the round")
	}
	if !strings.Contains(err.Error(), "regression (exit 1)") {
		t.Fatalf("refusal %q does not name the failed question and its exit code", err)
	}
	assertOpen(t, s, review.Tid)
}

// TestANewerAnswerWins: a reviewer that recorded a fail, fixed the cause and
// answered again reports the second answer, which is the current truth.
func TestANewerAnswerWins(t *testing.T) {
	s, dev, review := reviewFixture(t)
	runCheck(t, s, "p", "s-rev", review.Tid, dev.Checks[0], 0)
	report(t, s, coord.ReportInput{
		Project: "p", Session: "s-rev", Task: review.Tid, Role: "review",
		State: "fail", Slot: "tests", Command: "scripts/changed-needs-test.sh HEAD~1", Exit: exit(1), Digest: fixtureDigest,
	})
	slotPass(t, s, "p", "s-rev", review.Tid, fixtureDigest)

	if _, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid}); err != nil {
		t.Fatalf("the newest answer was not the one the gate read: %v", err)
	}
}

// TestGrandfatheredReviewClosesUnderItsOwnRule: the three questions are the rule a
// review is opened under. A review created before the change closes on its declared
// checks and digest, even though the close happens after the change — otherwise a
// review that was already working would be made unclosable by a question it was
// never asked.
func TestGrandfatheredReviewClosesUnderItsOwnRule(t *testing.T) {
	s, now := open(t)
	beforeSlots(now)
	dev := add(t, s, "p")
	review := addReview(t, s, "p", dev.Tid)
	claim(t, s, "p", "s-dev", dev.Tid)
	devDone(t, s, "p", "s-dev", dev.Tid)
	claim(t, s, "p", "s-rev", review.Tid)

	// The close happens under the new rule; the review was opened under the old one.
	// The move lapses the review's lease, so the same session takes the next round —
	// recovery, not a new reviewer.
	*now = time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	claim(t, s, "p", "s-rev", review.Tid)
	runCheck(t, s, "p", "s-rev", review.Tid, dev.Checks[0], 0)
	result, err := coord.Done(s, coord.DoneInput{Project: "p", Session: "s-rev", Task: review.Tid})
	if err != nil {
		t.Fatalf("a review opened before the rule was made unclosable by it: %v", err)
	}
	if len(result.Slots) != 0 {
		t.Fatalf("slots = %+v, want none: the review was never asked the questions", result.Slots)
	}
}

// TestASlotIsAReviewQuestion: a slot answers a review's question about a change, so
// it is refused on development work, on anything but a pass, fail or none, without a
// tree, and without the review role. Each refusal names what to do instead.
func TestASlotIsAReviewQuestion(t *testing.T) {
	s, now := open(t)
	dev := add(t, s, "p")
	review := addReview(t, s, "p", dev.Tid)
	claim(t, s, "p", "s-dev", dev.Tid)
	devDone(t, s, "p", "s-dev", dev.Tid)
	claim(t, s, "p", "s-rev", review.Tid)

	cases := []struct {
		name string
		in   coord.ReportInput
		want string
	}{
		{
			"on development work",
			coord.ReportInput{Project: "p", Session: "s-dev", Task: dev.Tid, State: "pass", Slot: "spec", Digest: fixtureDigest},
			"review task",
		},
		{
			"on a state that answers nothing",
			coord.ReportInput{Project: "p", Session: "s-rev", Task: review.Tid, State: "progress", Slot: "spec", Digest: fixtureDigest},
			"--state pass or --state fail",
		},
		{
			"with no tree",
			coord.ReportInput{Project: "p", Session: "s-rev", Task: review.Tid, State: "pass", Slot: "tests"},
			"--digest",
		},
		{
			"under an unknown name",
			coord.ReportInput{Project: "p", Session: "s-rev", Task: review.Tid, State: "pass", Slot: "coverage", Digest: fixtureDigest},
			"unknown slot",
		},
		{
			"as a developer's word",
			coord.ReportInput{Project: "p", Session: "s-rev", Task: review.Tid, State: "pass", Slot: "spec", Role: "dev", Digest: fixtureDigest},
			"--role review",
		},
		{
			"as a bare state",
			coord.ReportInput{Project: "p", Session: "s-rev", Task: review.Tid, State: "none", Digest: fixtureDigest},
			"--slot spec --none",
		},
		{
			"with a state it is not",
			coord.ReportInput{Project: "p", Session: "s-rev", Task: review.Tid, State: "pass", Slot: "spec", None: "nothing governs it", Digest: fixtureDigest},
			"drop --state",
		},
		{
			"with a note beside its reason",
			coord.ReportInput{Project: "p", Session: "s-rev", Task: review.Tid, Slot: "spec", None: "nothing governs it", Note: "also this", Digest: fixtureDigest},
			"drop --note",
		},
		{
			"with a check beside its reason",
			coord.ReportInput{Project: "p", Session: "s-rev", Task: review.Tid, Slot: "spec", None: "nothing governs it", Command: "true", Exit: exit(0), Digest: fixtureDigest},
			"drop --command",
		},
	}
	for _, c := range cases {
		_, err := coord.Report(s, c.in)
		if err == nil {
			t.Fatalf("a slot %s was recorded", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("refusal for a slot %s = %q, want it to name %q", c.name, err, c.want)
		}
	}
	*now = now.Add(time.Second)
}

// TestAnAnswerIsNotTheVerdict: answering one question does not make the review a
// finished one, so a second review task is still refused while the first has no
// verdict on the declared checks.
func TestAnAnswerIsNotTheVerdict(t *testing.T) {
	s, _, review := reviewFixture(t)
	slotPass(t, s, "p", "s-rev", review.Tid, fixtureDigest)

	if _, err := coord.Add(s, coord.AddInput{Project: "p", Goal: "second review", ReviewOf: review.ParentDev}); err == nil {
		t.Fatal("an answer to one question opened a second review of the same round")
	}
}

// newestTask is one task as status reports it, after the round closed.
func newestTask(t *testing.T, s *memory.Store, project, tid string) coord.TaskStatus {
	t.Helper()
	st, err := coord.Status(s, coord.StatusInput{Project: project})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, task := range st.Projects[0].Tasks {
		if task.Task == tid {
			return task
		}
	}
	t.Fatalf("no task %s in status", tid)
	return coord.TaskStatus{}
}
