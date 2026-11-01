package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/flagship-dev/flagship/internal/coord"
)

// envelope is what every fs invocation prints on stdout. A refusal is a normal
// outcome rather than a crash, so it is JSON too, and the exit code says which
// kind of answer it was.
type envelope struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// usageErr is a malformed command line: the caller asked for something fs does
// not understand, which is a different answer from "fs understood and refuses".
// It exits 2 so a script can tell the two apart.
type usageErr struct{ msg string }

func (e usageErr) Error() string { return e.msg }

// helpErr is a verb asked to describe itself: `fs <verb> --help`. It is not a
// refusal — the caller asked a question and fs answers it — so it exits 0.
// A wrapper that reads the exit code must not see help as a failure.
type helpErr struct{ verb string }

func (e helpErr) Error() string { return e.verb + ": help requested" }

// tableOutput is a report already rendered for a human, by an explicit
// --format table. run prints it as text instead of wrapping it in the JSON
// envelope: asking for a table is asking not to be given JSON. Nothing else
// bypasses the envelope, so a script still gets one answer shape.
type tableOutput string

// emit prints the envelope and returns the exit code: 0 for a result, 2 for a
// malformed command line, 1 for everything else. Every failure path is a
// refusal — there is no fallback store, no cached answer, and no partial success.
func emit(stdout, stderr io.Writer, data any, err error, usage string) int {
	if err == nil {
		if t, ok := data.(tableOutput); ok {
			fmt.Fprint(stdout, string(t))
			return 0
		}
		write(stdout, envelope{OK: true, Data: data})
		return 0
	}
	var u usageErr
	if errors.As(err, &u) {
		if usage != "" {
			fmt.Fprint(stderr, usage)
		}
		write(stdout, envelope{OK: false, Error: err.Error()})
		return 2
	}
	write(stdout, envelope{OK: false, Error: err.Error()})
	return 1
}

func write(w io.Writer, e envelope) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(e)
}

// The table's column widths. A task id is `t-` plus 16 hex characters, so it
// fits whole: a reader who then runs `fs claim --task` needs the real id.
const (
	colTask  = 18
	colKind  = 6
	colState = 7
	colHold  = 16
	colRound = 5
	colLease = 8
	colAgent = 9
	colGoal  = 60
)

// renderStatus formats a status report for a human: a count, then one block per
// project and one line per task. It renders the same report the JSON envelope
// carries, so the two views cannot disagree — there is no second read, no
// rendering state, and nothing stored.
func renderStatus(r coord.StatusReport, now time.Time) string {
	var b strings.Builder
	tasks, byState := 0, map[string]int{}
	for _, p := range r.Projects {
		tasks += len(p.Tasks)
		for _, t := range p.Tasks {
			byState[t.State]++
		}
	}
	fmt.Fprintf(&b, "%s, %s", plural(len(r.Projects), "project"), plural(tasks, "task"))
	for _, state := range []string{"open", "claimed", "done"} {
		if byState[state] > 0 {
			fmt.Fprintf(&b, " · %d %s", byState[state], state)
		}
	}
	b.WriteString("\n")

	for _, p := range r.Projects {
		if len(p.Tasks) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s\n", p.Project)
		fmt.Fprintf(&b, "  %-*s  %-*s  %-*s  %-*s  %-*s  %-*s  %-*s  %s\n",
			colTask, "TASK", colKind, "KIND", colState, "STATE", colHold, "HOLDER",
			colRound, "ROUND", colLease, "LEASE", colAgent, "AGENT", "GOAL")
		for _, t := range p.Tasks {
			fmt.Fprintf(&b, "  %-*s  %-*s  %-*s  %-*s  %-*s  %-*s  %-*s  %s\n",
				colTask, cell(t.Task, colTask),
				colKind, cell(t.Kind, colKind),
				colState, cell(t.State, colState),
				colHold, tail(holder(t), colHold),
				colRound, cell(round(t), colRound),
				colLease, cell(lease(t, now), colLease),
				colAgent, cell(t.Agent.State, colAgent),
				cell(oneLine(t.Goal), colGoal))
		}
	}

	// A scope keeps a name for as long as it holds any fs: row, including the
	// retired projection's, so empty projects are common and say nothing about
	// current work. One line for all of them, rather than a block each: the table
	// is for a human reading what is happening now. The JSON view still carries
	// every project, because that is the answer a query asked for.
	var empty []string
	for _, p := range r.Projects {
		if len(p.Tasks) == 0 {
			empty = append(empty, p.Project)
		}
	}
	if len(empty) > 0 {
		fmt.Fprintf(&b, "\n%s with no tasks: %s\n", plural(len(empty), "project"), strings.Join(empty, ", "))
	}
	return b.String()
}

// holder is who holds the task, or "-" when nobody does.
func holder(t coord.TaskStatus) string {
	if t.Session == "" {
		return "-"
	}
	return t.Session
}

// round is the claim attempt that holds the task. It is how a reader tells two
// sessions that held the same task apart.
func round(t coord.TaskStatus) string {
	if t.Round == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", t.Round)
}

// lease is what is left before the holder must write again to keep the task.
// A lapsed lease says so rather than showing a negative number: it frees the task
// for a new session, and it never means the work is done.
func lease(t coord.TaskStatus, now time.Time) string {
	if t.LeaseExpires == nil {
		return "-"
	}
	if !t.LeaseLive {
		return "lapsed"
	}
	left := t.LeaseExpires.Sub(now).Truncate(time.Second)
	if left < 0 {
		left = 0
	}
	return left.String()
}

// cell fits a value in one column: padded to width, or cut with an ellipsis so a
// long goal cannot push the table out of shape.
func cell(s string, width int) string {
	if len(s) <= width {
		return s
	}
	if width <= 1 {
		return s[:width]
	}
	return s[:width-1] + "…"
}

// tail fits an identifier in one column without hiding what tells two of them
// apart. A session name leads with when it started, so the end is the part that
// distinguishes it: cutting the head is right where cutting the goal's tail is not.
func tail(s string, width int) string {
	if len(s) <= width {
		return s
	}
	if width <= 1 {
		return s[len(s)-width:]
	}
	return "…" + s[len(s)-(width-1):]
}

// oneLine keeps a field on one line: a goal may contain newlines, and a table row
// may not.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// plural writes "1 task" rather than "1 tasks".
func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}
