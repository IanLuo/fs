package coord

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/flagship-dev/flagship/internal/memory"
)

// Launcher is the whole of what fs knows about processes: start an agent, ask
// which agents exist, close one. There is one implementation (herdr) and no
// fallback. A launcher that cannot start is a misconfiguration and a refusal —
// never a retry, because a broken launcher retried is a room full of agents
// burning tokens.
//
// List returns every live agent by pane, in one call. One call matters because a
// status listing and a reap pass each ask about every agent they know: a claim
// lease says whether an agent is *writing*, and silence is not death, so the only
// way to tell a dead agent from a slow one is to ask the launcher — once per
// listing, not once per row.
type Launcher interface {
	Start(LaunchSpec) (Handle, error)
	List() (map[string]string, error)
	Close(Handle) error
}

// LaunchSpec is one agent to start: who it claims as, where it works, and what it
// is told.
type LaunchSpec struct {
	Name   string
	Kind   string
	Dir    string
	Prompt string
}

// Handle is what a launcher returns and what a later Status or Close needs.
type Handle struct {
	Pane string `json:"pane,omitempty"`
	Name string `json:"name,omitempty"`
	Dir  string `json:"dir,omitempty"`
}

// Agent statuses a launcher may report.
const (
	StatusWorking = "working"
	StatusIdle    = "idle"
	StatusGone    = "gone"
)

// DispatchInput is one request to start an agent for a task.
type DispatchInput struct {
	Project string
	Task    string
	Kind    string // default pi
	Dir     string // default: the task's recorded project_path
}

// DispatchResult is what was started, and as whom.
type DispatchResult struct {
	Task    string `json:"task"`
	Kind    string `json:"kind"`
	Session string `json:"session"`
	Pane    string `json:"pane,omitempty"`
	Name    string `json:"name,omitempty"`
	Dir     string `json:"dir"`
	Prompt  string `json:"prompt"`
}

// Dispatch starts one agent for one task. fs chooses the session id, hands it to
// the agent in the prompt, and records what it started.
//
// The order matters and is the reason the record is worth having:
//
//  1. Read only: does the task exist, is it closed, does it have a live holder, is
//     there a directory to work in? All refusals happen here, before anything is
//     started, so a typo cannot leave an orphan pane behind.
//  2. Start the agent. A failure here is reported and not retried.
//  3. Record `launch:<tid>:<sid>` with the kind, the pane and the directory.
//
// The agent claims itself, under the id in the prompt, so the bind is still the
// agent's own write: fs starts the process, it does not claim on the agent's
// behalf. An agent that never claims leaves a launch record and no claim, which is
// exactly what `reap` needs to find it.
func Dispatch(s *memory.Store, in DispatchInput, l Launcher) (DispatchResult, error) {
	if err := memory.ValidProject(in.Project); err != nil {
		return DispatchResult{}, err
	}
	if err := memory.ValidID("task", in.Task); err != nil {
		return DispatchResult{}, err
	}
	if in.Kind == "" {
		in.Kind = "pi"
	}
	if err := memory.ValidID("kind", in.Kind); err != nil {
		return DispatchResult{}, err
	}
	scope := memory.Scope(in.Project)
	rows, err := s.Rows(scope)
	if err != nil {
		return DispatchResult{}, err
	}
	task, err := loadTask(rows, in.Task)
	if err != nil {
		return DispatchResult{}, err
	}
	if task.done {
		return DispatchResult{}, fmt.Errorf("%s is closed; a completed task is never reopened, so there is nothing to start", task.Tid)
	}
	now := s.Now()
	ix := memory.NewIndex(rows)
	if held, ok := ix.Claim(task.Tid); ok && held.Live(now) {
		return DispatchResult{}, fmt.Errorf("task %s is held by session %s until %s: it is already being worked on",
			task.Tid, held.Session, held.Expires().UTC().Format(time.RFC3339))
	}
	if prev, ok := latestLaunch(rows, task.Tid); ok {
		// A row and a clock cannot tell a dead agent from a slow one: a lapsed lease
		// means the session stopped writing, and silence is not death. Ask.
		live, err := l.List()
		if err != nil {
			return DispatchResult{}, fmt.Errorf("cannot see the agents already running (%w); refusing to start a second one for %s", err, task.Tid)
		}
		if status, alive := live[prev.Pane]; alive {
			return DispatchResult{}, fmt.Errorf("task %s is already being worked on by session %s (agent %s, pane %s, %s); it is alive even if its lease lapsed — run fs reap or wait for it to finish",
				task.Tid, prev.Session, prev.Name, prev.Pane, status)
		}
	}
	dir := strings.TrimSpace(in.Dir)
	if dir == "" {
		dir = task.ProjectPath
	}
	if dir == "" {
		return DispatchResult{}, fmt.Errorf("%s records no path; pass --dir so the agent knows where the work is", task.Tid)
	}

	sid := dispatchSession(task.Tid, now)
	name := dispatchName(task.Tid, now)
	prompt := dispatchPrompt(task, in.Project, sid, dir, s.Path())

	handle, err := l.Start(LaunchSpec{Name: name, Kind: in.Kind, Dir: dir, Prompt: prompt})
	if err != nil {
		return DispatchResult{}, fmt.Errorf("launcher refused to start %s: %w", task.Tid, err)
	}
	extra, err := json.Marshal(launchExtra{
		Task: task.Tid, Session: sid, Kind: in.Kind, Role: task.Kind,
		Pane: handle.Pane, Name: handle.Name, Dir: dir, At: now.UTC(),
	})
	if err != nil {
		return DispatchResult{}, err
	}
	if err := s.PutLaunch(scope, task.Tid, sid, fmt.Sprintf("started %s for %s", in.Kind, task.Tid), string(extra)); err != nil {
		// The agent is running and fs cannot say so. Closing it is the honest move:
		// a process fs cannot account for is exactly what reap exists to prevent.
		_ = l.Close(handle)
		return DispatchResult{}, err
	}
	return DispatchResult{
		Task: task.Tid, Kind: in.Kind, Session: sid,
		Pane: handle.Pane, Name: handle.Name, Dir: dir, Prompt: prompt,
	}, nil
}

// latestLaunch returns the newest launch record for a task. What that record means
// is decided against the launcher, never against the clock: the record says which
// agent fs started, and only the launcher says whether it still exists.
func latestLaunch(rows []memory.Row, tid string) (launchExtra, bool) {
	var best launchExtra
	var bestAt time.Time
	for _, r := range rows {
		ex, ok := decodeLaunch(r)
		if !ok || ex.Task != tid {
			continue
		}
		if ex.At.After(bestAt) {
			bestAt, best = ex.At, ex
		}
	}
	if best.Session == "" {
		return launchExtra{}, false
	}
	return best, true
}

// dispatchSession is the identity fs hands the agent. It is derived from the task
// and the clock rather than random, so two dispatches in the same second still
// differ and a human can read which task and when.
func dispatchSession(tid string, now time.Time) string {
	short := strings.TrimPrefix(tid, "t-")
	if len(short) > 8 {
		short = short[:8]
	}
	return fmt.Sprintf("d-%s-%s", short, now.UTC().Format("20060102T150405Z"))
}

// dispatchName is the pane/agent name. herdr caps a name at 32 characters:
// "fs-" + 8 hex + "-" + 6 leaves room.
func dispatchName(tid string, now time.Time) string {
	short := strings.TrimPrefix(tid, "t-")
	if len(short) > 8 {
		short = short[:8]
	}
	return fmt.Sprintf("fs-%s-%s", short, now.UTC().Format("150405"))
}

// dispatchPrompt is what the agent is told. It carries everything the agent needs
// and nothing it should inherit: the identity to claim as, the checks to run, the
// commands to record them with, and where the round ends.
func dispatchPrompt(task Task, project, sid, dir string, path string) string {
	var b strings.Builder
	if path != "" {
		b.WriteString("Run this first, in every shell you use: export MEMSTORE_DB=" + path + "\n")
		b.WriteString("(A new pane inherits the launcher's environment, not the caller's, so without it you would claim in a different store.)\n\n")
	}
	b.WriteString("You are working in fs project " + project + ", in " + dir + ", as session " + sid + ".\n\n")
	b.WriteString("1. Claim your task: fs claim --project " + project + " --session " + sid + " --task " + task.Tid + "\n")
	if task.Kind == "review" {
		b.WriteString("You are the independent reviewer: you did not write this code and you do not have its author's context. Do not trust the task text; run the checks and decide.\n\n")
		b.WriteString("2. Run every declared check yourself, exactly as declared:\n")
		for _, c := range task.Checks {
			b.WriteString("   - " + c + "\n")
		}
		b.WriteString("\n3. Record each run as you finish it, with its real exit code:\n")
		b.WriteString("   fs report --project " + project + " --session " + sid + " --task " + task.Tid +
			" --role review --state pass --command '<the exact command>' --exit <exit code>\n")
		b.WriteString("   Use --state fail when the exit was non-zero. Never report a check you did not run.\n\n")
		b.WriteString("4. While you still have reading to do, keep the lease alive: " +
			"fs report --project " + project + " --session " + sid + " --task " + task.Tid +
			" --role review --state progress --note reviewing, at least every 90 s.\n")
		b.WriteString("   If a write is refused because the lease expired, re-run the claim with this same session id: it takes the next round and you can continue.\n\n")
		b.WriteString("5. If every check passed: fs done --project " + project + " --session " + sid + " --task " + task.Tid + ", then stop.\n")
		b.WriteString("   If a check failed or could not be run: report --state fail with a --note saying what is wrong, then stop. Do not fix the code and do not run done: a fresh reviewer gets the next round.\n")
		return b.String()
	}
	b.WriteString("2. Do the work.\n\n")
	b.WriteString("3. Report as you go: fs report --project " + project + " --session " + sid + " --task " + task.Tid +
		" --state progress --note '<where you are>' --digest '<what changed>'. This write is your lease: it lapses 120 s after your newest write, and if it lapses you may re-claim with this same session id.\n\n")
	b.WriteString("4. Run the declared checks yourself before claiming done:\n")
	for _, c := range task.Checks {
		b.WriteString("   - " + c + "\n")
	}
	b.WriteString("\n5. When the work is complete: fs report --project " + project + " --session " + sid + " --task " + task.Tid +
		" --state dev-done --note '<what you changed>' --digest '<commit>'. This does not complete the task.\n\n")
	b.WriteString("6. Then create its review and start it yourself, which is how your task can close: fs add --project " + project +
		" --goal 'review " + task.Tid + "' --review-of " + task.Tid + ", then fs dispatch --project " + project +
		" --task <the review task id>. You do not review your own work: the reviewer is a fresh session that runs the same checks.\n")
	return b.String()
}

// decodeLaunch reads one launch row. There is one reader, because the record's
// shape is the only thing dispatch, reap and status agree on: a second decoder is a
// second definition, and they drift.
func decodeLaunch(r memory.Row) (launchExtra, bool) {
	if r.Kind != memory.KindLaunch {
		return launchExtra{}, false
	}
	var ex launchExtra
	if err := json.Unmarshal([]byte(r.Extra), &ex); err != nil {
		return launchExtra{}, false
	}
	if ex.Task == "" || ex.Session == "" {
		return launchExtra{}, false
	}
	return ex, true
}

// launchExtra is a launch row's extra: what fs started, and as whom.
type launchExtra struct {
	Task    string    `json:"task"`
	Session string    `json:"session"`
	Kind    string    `json:"kind"`
	Role    string    `json:"role"`
	Pane    string    `json:"pane,omitempty"`
	Name    string    `json:"name,omitempty"`
	Dir     string    `json:"dir"`
	At      time.Time `json:"at"`
}

// LaunchRecord is one agent fs started.
type LaunchRecord struct {
	Task    string `json:"task"`
	Session string `json:"session"`
	Kind    string `json:"kind"`
	Role    string `json:"role"`
	Pane    string `json:"pane,omitempty"`
	Name    string `json:"name,omitempty"`
	Dir     string `json:"dir"`
	At      string `json:"at"`
}

// ReapInput is one pass over the agents fs started.
type ReapInput struct {
	Project string // empty: every project
}

// Reaped is one launch and what happened to it.
type Reaped struct {
	Task    string `json:"task"`
	Session string `json:"session"`
	Pane    string `json:"pane,omitempty"`
	Name    string `json:"name,omitempty"`
	Reason  string `json:"reason"`
}

// ReapResult is the pass.
type ReapResult struct {
	Closed []Reaped `json:"closed"`
	Kept   []Reaped `json:"kept"`
}

// Reap closes the agents fs started whose round is over, and leaves every other
// process alone.
//
// A round is over when the task is closed, when this session's newest review
// verdict is fail or blocked (the round ended with feedback), or when the session's
// lease has lapsed. Working agents are never closed, and neither is a pane fs has
// no record of: fs closes what fs opened. Closing is idempotent — the record stays,
// the process does not — so a second pass finds nothing to do.
func Reap(s *memory.Store, in ReapInput, l Launcher) (ReapResult, error) {
	if in.Project != "" {
		if err := memory.ValidProject(in.Project); err != nil {
			return ReapResult{}, err
		}
	}
	var rows []memory.Row
	var err error
	if in.Project != "" {
		rows, err = s.Rows(memory.Scope(in.Project))
	} else {
		rows, err = s.AllRows()
	}
	if err != nil {
		return ReapResult{}, err
	}
	now := s.Now()

	// One record per (task, session), newest first, so a re-dispatch of the same
	// task under a new session is a separate entry.
	type key struct{ tid, sid string }
	seen := map[key]bool{}
	out := ReapResult{Closed: []Reaped{}, Kept: []Reaped{}}

	byScope := map[string][]memory.Row{}
	for _, r := range rows {
		byScope[r.Scope] = append(byScope[r.Scope], r)
	}
	for _, scope := range launchScopes(byScope) {
		local := byScope[scope]
		ix := memory.NewIndex(local)
		tasks := map[string]Task{}
		for _, t := range allTasks(local) {
			tasks[t.Tid] = t
		}
		live, lerr := l.List()
		if lerr != nil {
			for _, r := range local {
				ex, ok := decodeLaunch(r)
				if !ok {
					continue
				}
				k := key{ex.Task, ex.Session}
				if seen[k] {
					continue
				}
				seen[k] = true
				out.Kept = append(out.Kept, Reaped{Task: ex.Task, Session: ex.Session, Pane: ex.Pane, Name: ex.Name,
					Reason: "cannot see the agents: " + lerr.Error()})
			}
			continue
		}
		for _, r := range local {
			ex, ok := decodeLaunch(r)
			if !ok {
				continue
			}
			k := key{ex.Task, ex.Session}
			if seen[k] {
				continue
			}
			seen[k] = true
			handle := Handle{Pane: ex.Pane, Name: ex.Name, Dir: ex.Dir}
			task, ok := tasks[ex.Task]
			if !ok {
				continue
			}
			status, alive := live[ex.Pane]
			reason := roundOver(task, ix, ex.Session, now)
			switch {
			case !alive:
				out.Closed = append(out.Closed, Reaped{Task: ex.Task, Session: ex.Session, Pane: ex.Pane, Name: ex.Name,
					Reason: "agent gone"})
			case status == StatusWorking:
				out.Kept = append(out.Kept, Reaped{Task: ex.Task, Session: ex.Session, Pane: ex.Pane, Name: ex.Name,
					Reason: "still working"})
			case reason != "":
				if cerr := l.Close(handle); cerr != nil {
					out.Kept = append(out.Kept, Reaped{Task: ex.Task, Session: ex.Session, Pane: ex.Pane, Name: ex.Name,
						Reason: "close failed: " + cerr.Error()})
					continue
				}
				out.Closed = append(out.Closed, Reaped{Task: ex.Task, Session: ex.Session, Pane: ex.Pane, Name: ex.Name,
					Reason: reason})
			default:
				out.Kept = append(out.Kept, Reaped{Task: ex.Task, Session: ex.Session, Pane: ex.Pane, Name: ex.Name,
					Reason: "round still open"})
			}
		}
	}
	return out, nil
}

// roundOver names why a launch's round has ended, or "" when it has not. It reads
// facts, never the clock alone: a verdict is a fact, a held claim is a fact, a
// lapsed lease is a fact.
//
// Order matters. A live claim outranks an old verdict, because a reviewer refused
// after a lapse re-claims for the next round with the same session id — its
// previous `fail` is then history, not a reason to close the pane under it. Only
// the session's newest word is read: a negative verdict ends the round, a progress
// report means the agent is working.
func roundOver(task Task, ix *memory.Index, sid string, now time.Time) string {
	if task.done {
		return "task closed"
	}
	if held, ok := ix.Claim(task.Tid); ok && held.Session == sid && held.Live(now) {
		return ""
	}
	for _, ev := range eventsFor(ix, task.Tid, now) {
		if ev.Session != sid {
			continue
		}
		if ev.Role == "review" && (ev.State == "fail" || ev.State == "blocked") {
			return "round ended: " + ev.State
		}
		break // the session's newest word is not a verdict
	}
	if held, ok := ix.Claim(task.Tid); !ok || held.Session != sid {
		return "no longer holds the task"
	} else if !held.Live(now) {
		return "lease lapsed"
	}
	return ""
}

// launchScopes orders scopes so two reaps read the same way.
func launchScopes(set map[string][]memory.Row) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
