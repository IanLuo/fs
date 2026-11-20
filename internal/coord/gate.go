package coord

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/flagship-dev/flagship/internal/memory"
)

// DoneInput is a reviewer asking to complete a review.
type DoneInput struct {
	Project string
	Session string
	Task    string
	Note    string
	// Dir is the directory the reviewer signs from: its working directory at the
	// moment it asks. A review is a judgement made in its own tree, so the gate
	// refuses a signature made in the developer's checkout. Empty means the caller
	// could not say which directory it is in, and then there is no fact to compare.
	Dir string
}

// CheckResult is one required check as the completion record keeps it: the exact
// command, its exit result, and the session that ran it.
type CheckResult struct {
	Command string `json:"command"`
	Exit    int    `json:"exit"`
	Session string `json:"session"`
}

// DoneResult names both tasks a passing review closes.
type DoneResult struct {
	Review string        `json:"review"`
	Dev    string        `json:"dev"`
	Checks []CheckResult `json:"checks"`
	// Slots are the three review questions as the close answered them. Empty on a
	// review opened before the questions were the rule, which is asked and answered
	// under the rule it was opened with.
	Slots []SlotResult `json:"slots,omitempty"`
}

// SlotResult is one review question as the close answered it: which question,
// what the reviewer recorded at the developer's tree, and the command behind it
// when a command answered it.
type SlotResult struct {
	Slot    string `json:"slot"`
	Verdict string `json:"verdict"`
	Command string `json:"command,omitempty"`
	Exit    *int   `json:"exit,omitempty"`
	Note    string `json:"note,omitempty"`
}

// The three review questions a close answers, in the order a reader asks them:
// are the specs honoured, are the code and tests good, and did this change break
// anything.
const (
	slotSpec       = "spec"
	slotTests      = "tests"
	slotRegression = "regression"

	// stateNone is the record of a spec slot no spec governs: the gap is written
	// down, with its reason, instead of being left invisible. No other slot accepts
	// it, because "no test answers this" and "nothing broke" are not questions a
	// missing artefact can answer.
	stateNone = "none"
)

// reviewSlots is the closed set of questions, and the order the refusal names them.
var reviewSlots = []string{slotSpec, slotTests, slotRegression}

// isReviewSlot reports whether s names one of the three questions.
func isReviewSlot(s string) bool {
	for _, slot := range reviewSlots {
		if slot == s {
			return true
		}
	}
	return false
}

// slotCutoff is when the three review questions became the close rule. A review
// task created at or after it answers all three; one created before it closes
// under the rule it was opened under — its declared checks and the digest — so a
// review an earlier rule opened is not made unclosable by a question it was never
// asked. Creation is the fact the round is decided by because it is written once,
// when the review is added, and cannot move afterwards: the claim, the verdict and
// the close all happen later, and a rule that keyed on any of them would change
// under a reviewer that is already working.
var slotCutoff = time.Date(2026, 11, 20, 8, 40, 0, 0, time.UTC)

// Done is the completion gate.
//
// Completion is requested on a review task, by the reviewer, and only a passing
// independent check set closes anything. All of these leave the task incomplete:
// no review task (a worker cannot complete its own work), a development task
// already closed, no developer to be independent from, a signature made in the
// developer's own checkout, a required check that is missing or did not pass, and
// — on a review opened at or after slotCutoff — a review question with no passing
// answer at the developer's tree. The evidence refusals are recorded; the ones
// about the shape of the round are refused without a row, because there is no
// round to record anything about.
//
// The developer's own report of completion is never enough on its own: it is one
// input, and this gate is the other.
func Done(s *memory.Store, in DoneInput) (DoneResult, error) {
	if err := memory.ValidProject(in.Project); err != nil {
		return DoneResult{}, err
	}
	if err := memory.ValidID("session", in.Session); err != nil {
		return DoneResult{}, err
	}
	if err := memory.ValidID("task", in.Task); err != nil {
		return DoneResult{}, err
	}

	scope := memory.Scope(in.Project)
	rows, err := s.Rows(scope)
	if err != nil {
		return DoneResult{}, err
	}
	now := s.Now()
	ix := memory.NewIndex(rows)
	if err := hold(ix, in.Session, in.Task, now); err != nil {
		return DoneResult{}, err
	}

	review, err := loadTask(rows, in.Task)
	if err != nil {
		return DoneResult{}, err
	}
	if review.ParentDev == "" {
		return DoneResult{}, fmt.Errorf(
			"%s is development work: completion requires a review task, claimed by a fresh session, that runs the checks — a worker cannot complete its own work", review.Tid)
	}
	dev, err := loadTask(rows, review.ParentDev)
	if err != nil {
		return DoneResult{}, err
	}
	if dev.done {
		return DoneResult{}, fmt.Errorf("%s is already closed; a completed task is never reopened", dev.Tid)
	}
	developer, ok := ix.Claim(dev.Tid)
	if !ok {
		return DoneResult{}, fmt.Errorf("%s was never claimed, so there is no developer to be independent from", dev.Tid)
	}
	if developer.Session == in.Session {
		return DoneResult{}, fmt.Errorf("session %s holds %s; a reviewer must be a different session from the developer", in.Session, dev.Tid)
	}
	// Where the reviewer signs from is part of the review, not a detail of its
	// shell: a round completed in the developer's own checkout is one context, which
	// is the one thing a review cannot be. Both paths are resolved before they are
	// compared — one directory has many spellings, and a textual comparison refuses
	// the wrong signature as often as it lets the right one through. A developer row
	// with no recorded path is a round from before paths were recorded: there is no
	// fact to compare, so nothing is refused.
	if in.Dir != "" && dev.ProjectPath != "" {
		signed, developed := sameDir(in.Dir), sameDir(dev.ProjectPath)
		if signed == developed {
			return DoneResult{}, fmt.Errorf(
				"completion refused: this signature is from %s, and %s was developed in %s: both resolve to %s, one directory, so the review is not independent — sign from the review worktree; the task stays incomplete",
				in.Dir, dev.Tid, dev.ProjectPath, signed)
		}
	}

	// Every check the development task declared, against the newest run of that
	// exact command by this reviewer. A later run is the current truth, so a
	// reviewer that fixed and re-ran reports the rerun.
	events := eventsFor(ix, review.Tid, now)
	var (
		passed  []CheckResult
		missing []string
		failed  []string
	)
	for _, check := range dev.Checks {
		ev, ok := newestCheck(events, in.Session, check)
		if !ok || ev.Exit == nil {
			missing = append(missing, check)
			continue
		}
		if *ev.Exit != 0 {
			failed = append(failed, fmt.Sprintf("%s (exit %d)", check, *ev.Exit))
			continue
		}
		passed = append(passed, CheckResult{Command: check, Exit: *ev.Exit, Session: ev.Session})
	}
	// A verdict names the tree it judged, and it is the tree the developer
	// finished: the newest dev-done names that tree, and the reviewer's newest
	// passing run names the tree it ran against. Two different trees are two
	// different rounds, and the gate will not bless one with the other's name.
	// Compared exactly as recorded — a git abbreviation is a different string, and
	// guessing which one was meant is how a gate stops saying what it checked.
	devDigest := newestDevDoneDigest(eventsFor(ix, dev.Tid, now))
	reviewDigest := newestPassDigest(events, in.Session)
	var digestReason string
	switch {
	case devDigest == "":
		digestReason = fmt.Sprintf("%s has no dev-done digest: a verdict must name the tree it judged, so the developer reports dev-done again with --digest", dev.Tid)
	case reviewDigest == "":
		digestReason = fmt.Sprintf("the reviewer's pass names no digest: report the passing run with --digest %s", devDigest)
	case reviewDigest != devDigest:
		digestReason = fmt.Sprintf("the reviewer passed %s but the developer finished %s: a verdict names a different tree", reviewDigest, devDigest)
	}
	// The three review questions, for a review this rule governs. They are answered
	// against the developer's tree, so there is no question to ask until that tree
	// is named: the refusal above already says what to do about it.
	var (
		slotPassed   []SlotResult
		slotsMissing []string
		slotsFailed  []string
	)
	if !review.at.Before(slotCutoff) && devDigest != "" {
		slotPassed, slotsMissing, slotsFailed = slotsAt(events, in.Session, devDigest)
	}
	refused := refusal{
		missing: missing, failed: failed, digestWhy: digestReason,
		slotsMissing: slotsMissing, slotsFailed: slotsFailed,
	}
	if len(slotsMissing) > 0 || len(slotsFailed) > 0 {
		refused.fix = fmt.Sprintf("answer each question with fs report --project %s --session %s --task %s --slot <slot> --state pass --command '<the check that answered it>' --exit <its exit code> --digest %s, or --slot spec --none '<why>' when no spec governs the change", in.Project, in.Session, review.Tid, devDigest)
	}
	if len(missing) > 0 || len(failed) > 0 || digestReason != "" || len(slotsMissing) > 0 || len(slotsFailed) > 0 {
		reason := refused.reason()
		// The task stays incomplete and the review records the reason: a refusal
		// with no trace would make the next reader re-derive it from nothing. It is
		// recorded as `refused`, not `fail`: the gate declined to close, which is
		// not a reviewer's verdict that a check failed.
		extra, err := json.Marshal(eventExtra{
			State: "refused", Role: "review", Note: clip(reason, maxNote),
			Round: roundOf(ix, review.Tid, "refused"),
		})
		if err != nil {
			return DoneResult{}, err
		}
		if err := s.PutEvent(scope, review.Tid, in.Session,
			ix.MaxEvent(review.Tid, in.Session)+1,
			reason, string(extra)); err != nil {
			return DoneResult{}, err
		}
		return DoneResult{}, errors.New(reason)
	}

	// A passing review is the one step that used to leave no row of its own, so a
	// round's duration and a review's latency were unmeasurable from the records: the
	// task row closed and nothing said when. Record the close on every task it closes,
	// before the rows are closed — a failure between the two is recoverable, because
	// the retry closes and records again, where closing first would leave a closed
	// round that no retry could ever record.
	// The review may have run in a checkout fs made for it. fs removes only what it
	// made, and only while it is clean: a dirty worktree holds someone's work, so it
	// stays on disk and the close names it. There is no --force anywhere — deleting
	// a directory with uncommitted work is not a cleanup. This happens before the
	// close is written, so the note can say what was left. Every refusal above
	// happens before it, so a refused round keeps its checkout to continue in.
	worktreeNote := settleWorktree(rows, dev, review, in.Session)
	facts := closeFacts{Session: in.Session, Dir: in.Dir, Digest: devDigest, Worktree: worktreeNote}
	for _, tid := range []string{review.Tid, dev.Tid} {
		if err := writeClose(s, scope, ix, tid, facts); err != nil {
			return DoneResult{}, err
		}
	}

	// Passing. Close the review first, then the development task it reviewed. That
	// order is the recoverable one: a failure between the two leaves the
	// development task open, and a rerun closes it — while closing the development
	// task first would leave a review task that can never be completed, because
	// the gate refuses a closed development task.
	if err := s.Retire(review.rowID); err != nil {
		return DoneResult{}, err
	}
	if err := s.Retire(dev.rowID); err != nil {
		return DoneResult{}, err
	}
	return DoneResult{Review: review.Tid, Dev: dev.Tid, Checks: passed, Slots: slotPassed}, nil
}

// closeFacts is everything one close records about the round it ends: who signed,
// from where, at which tree, and what became of the checkout fs made.
type closeFacts struct {
	Session  string
	Dir      string
	Digest   string
	Worktree string
}

// note is the close's one line: where the signature was made, and the checkout fs
// left behind when it did not remove one.
func (f closeFacts) note() string {
	var parts []string
	if f.Dir != "" {
		parts = append(parts, "signed from "+f.Dir)
	}
	if f.Worktree != "" {
		parts = append(parts, f.Worktree)
	}
	return strings.Join(parts, "; ")
}

// settleWorktree deals with the review checkout fs made, and answers what the
// close should say about it: nothing when fs made none or removed it, and the
// reason it stayed when it did. A directory fs cannot read is left for the same
// reason a dirty one is — fs would be deleting on a guess.
func settleWorktree(rows []memory.Row, dev, review Task, sid string) string {
	wt, ok := launchedWorktree(rows, review.Tid, sid)
	if !ok || !exists(wt.Dir) {
		return ""
	}
	clean, err := worktreeClean(wt.Dir)
	if err != nil {
		return fmt.Sprintf("checkout %s left in place: %v", wt.Dir, err)
	}
	if !clean {
		return fmt.Sprintf("checkout %s kept: it has uncommitted work", wt.Dir)
	}
	if err := removeWorktree(dev.ProjectPath, wt.Dir); err != nil {
		return fmt.Sprintf("checkout %s left in place: %v", wt.Dir, err)
	}
	return ""
}

// writeClose records one close: the round ended, in this session, from this
// directory, at the tree the signature blessed. It is one append-only row of the
// ordinary report shape, one per closed task, so a later reader can measure a round
// — claim to close, dev-done to close — without a chat log and without a second
// store.
func writeClose(s *memory.Store, scope string, ix *memory.Index, tid string, f closeFacts) error {
	note := clip(f.note(), maxNote)
	text := "closed"
	if note != "" {
		text = "closed: " + note
	}
	extra, err := json.Marshal(eventExtra{
		State: stateClosed, Role: "review", Digest: f.Digest, Note: note,
	})
	if err != nil {
		return err
	}
	return s.PutEvent(scope, tid, f.Session, ix.MaxEvent(tid, f.Session)+1, text, string(extra))
}

// sameDir is the string two directories are compared as: symlinks followed where
// the path exists, cleaned otherwise. A path that is gone still compares as itself,
// because whether it exists is a different question from whether it is the
// developer's.
func sameDir(dir string) string {
	if dir == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Clean(real)
	}
	return filepath.Clean(dir)
}

// newestCheck returns the newest review-run result for one exact command by the
// session that must have run it. A run by anyone else is not evidence of an
// independent check.
func newestCheck(events []Evidence, sid, command string) (Evidence, bool) {
	for _, ev := range events {
		if ev.Role != "review" || ev.Session != sid || ev.Command != command {
			continue
		}
		return ev, true
	}
	return Evidence{}, false
}

// newestDevDoneDigest is the tree the developer last reported finished. The
// newest dev-done wins: a developer that reworked and reported again names the
// later tree, and the earlier one is history.
func newestDevDoneDigest(events []Evidence) string {
	for _, ev := range events {
		if ev.Role == "dev" && ev.State == "dev-done" {
			return strings.TrimSpace(ev.Digest)
		}
	}
	return ""
}

// newestPassDigest is the tree this reviewer's newest passing run names. A pass is
// the reviewer saying "this tree runs the checks green", so the newest one is the
// tree it is asking to close.
func newestPassDigest(events []Evidence, sid string) string {
	for _, ev := range events {
		if ev.Role == "review" && ev.Session == sid && ev.State == "pass" {
			return strings.TrimSpace(ev.Digest)
		}
	}
	return ""
}

// refusal is everything a reader needs to act on a declined close: what was never
// run, what failed, the tree a verdict names, and the review questions left
// unanswered — plus the verb that answers a question.
type refusal struct {
	missing      []string
	failed       []string
	digestWhy    string
	slotsMissing []string
	slotsFailed  []string
	fix          string
}

// reason says exactly what is missing or failing, in the order a reader can act
// on it.
func (r refusal) reason() string {
	var parts []string
	if len(r.missing) > 0 {
		parts = append(parts, "no recorded reviewer run of: "+strings.Join(r.missing, "; "))
	}
	if len(r.failed) > 0 {
		parts = append(parts, "failed: "+strings.Join(r.failed, "; "))
	}
	if len(r.slotsMissing) > 0 {
		parts = append(parts, "no passing answer to the review question: "+strings.Join(r.slotsMissing, ", "))
	}
	if len(r.slotsFailed) > 0 {
		parts = append(parts, "the review question failed: "+strings.Join(r.slotsFailed, ", "))
	}
	if r.digestWhy != "" {
		parts = append(parts, r.digestWhy)
	}
	reason := "completion refused: " + strings.Join(parts, " — ") + "; the task stays incomplete"
	if r.fix != "" {
		reason += " — " + r.fix
	}
	return reason
}

// slotsAt answers the three review questions from this reviewer's newest record of
// each, at the tree the developer finished. A newer record is the current truth,
// so a question asked twice is answered by the second answer.
func slotsAt(events []Evidence, sid, tree string) (passed []SlotResult, missing, failed []string) {
	for _, slot := range reviewSlots {
		ev, ok := newestSlot(events, sid, slot)
		switch {
		case !ok:
			missing = append(missing, slot)
		case ev.Digest != tree:
			missing = append(missing, fmt.Sprintf("%s (recorded for %s, not %s)", slot, ev.Digest, tree))
		case ev.State == stateNone && slot == slotSpec:
			passed = append(passed, SlotResult{Slot: slot, Verdict: stateNone, Note: ev.Note})
		case ev.State == "pass":
			passed = append(passed, SlotResult{Slot: slot, Verdict: ev.State, Command: ev.Command, Exit: ev.Exit, Note: ev.Note})
		case ev.State == "fail":
			failed = append(failed, slotFail(slot, ev))
		default:
			failed = append(failed, fmt.Sprintf("%s (recorded as %s)", slot, ev.State))
		}
	}
	return passed, missing, failed
}

// newestSlot returns this reviewer's newest record of one review question. The
// role and the session are checked: a question answered by the developer, or by
// another session, is not an answer from the review.
func newestSlot(events []Evidence, sid, slot string) (Evidence, bool) {
	for _, ev := range events {
		if ev.Role != "review" || ev.Session != sid || ev.Slot != slot {
			continue
		}
		return ev, true
	}
	return Evidence{}, false
}

// slotFail names a question the reviewer recorded as failing, with the exit code
// that failed it when a command produced one.
func slotFail(slot string, ev Evidence) string {
	if ev.Exit != nil {
		return fmt.Sprintf("%s (exit %d)", slot, *ev.Exit)
	}
	return slot
}

// clip bounds a stored string. Evidence is bounded by design: no transcript, no
// unbounded tool output, nothing a secret could hide in.
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
