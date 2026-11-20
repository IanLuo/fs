package coord

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

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
}

// Done is the completion gate.
//
// Completion is requested on a review task, by the reviewer, and only a passing
// independent check set closes anything. All of these leave the task incomplete:
// no review task (a worker cannot complete its own work), a development task
// already closed, no developer to be independent from, a signature made in the
// developer's own checkout, and a required check that is missing or did not pass.
// The evidence refusals are recorded; the ones about the shape of the round are
// refused without a row, because there is no round to record anything about.
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
	if len(missing) > 0 || len(failed) > 0 || digestReason != "" {
		reason := refusalReason(missing, failed, digestReason)
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
	for _, tid := range []string{review.Tid, dev.Tid} {
		if err := writeClose(s, scope, ix, tid, in.Session, in.Dir, devDigest); err != nil {
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
	return DoneResult{Review: review.Tid, Dev: dev.Tid, Checks: passed}, nil
}

// writeClose records one close: the round ended, in this session, from this
// directory, at the tree the signature blessed. It is one append-only row of the
// ordinary report shape, one per closed task, so a later reader can measure a round
// — claim to close, dev-done to close — without a chat log and without a second
// store.
func writeClose(s *memory.Store, scope string, ix *memory.Index, tid, sid, dir, digest string) error {
	note, text := "", "closed"
	if dir != "" {
		// The directory travels in the note because the record has one bounded
		// free-text line and no field to spare: the close is an ordinary report.
		note = "signed from " + dir
		text = "closed: " + note
	}
	extra, err := json.Marshal(eventExtra{
		State: stateClosed, Role: "review", Digest: digest, Note: clip(note, maxNote),
	})
	if err != nil {
		return err
	}
	return s.PutEvent(scope, tid, sid, ix.MaxEvent(tid, sid)+1, text, string(extra))
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

// refusalReason says exactly what is missing or failing, in the order a reader
// can act on it.
func refusalReason(missing, failed []string, digest string) string {
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "no recorded reviewer run of: "+strings.Join(missing, "; "))
	}
	if len(failed) > 0 {
		parts = append(parts, "failed: "+strings.Join(failed, "; "))
	}
	if digest != "" {
		parts = append(parts, digest)
	}
	return "completion refused: " + strings.Join(parts, " — ") + "; the task stays incomplete"
}

// clip bounds a stored string. Evidence is bounded by design: no transcript, no
// unbounded tool output, nothing a secret could hide in.
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
