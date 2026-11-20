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

// DefaultDispatchWait is how long dispatch waits for the agent to take the task
// before the launch is a refusal.
const DefaultDispatchWait = 60 * time.Second

// Launch outcomes recorded on the launch row. "started" is a launch fs accepted;
// "unconfirmed" is one where no signal arrived in the window, so the record says
// what fs knows rather than what it hoped.
const (
	OutcomeStarted     = "started"
	OutcomeUnconfirmed = "unconfirmed"
)

// dispatchPoll is how often the waiting dispatcher re-reads the store. The
// signals are rows another process writes, so only a fresh listing can see them.
const dispatchPoll = 250 * time.Millisecond

// DispatchInput is one request to start an agent for a task.
type DispatchInput struct {
	Project string
	Task    string
	Kind    string // default pi
	Dir     string // default: --dir, then the newest fs path binding, then the task's path

	// Wait is the window in which the agent must claim the task or write
	// evidence. Zero means DefaultDispatchWait; NoWait skips the wait.
	Wait time.Duration
	// NoWait is fire-and-forget: start, record, return, and mark the result
	// unconfirmed because nothing was seen.
	NoWait bool

	// Worktree asks fs to make the review's own checkout: the developer's newest
	// dev-done digest, checked out in a sibling directory, so the reviewer starts
	// independent by construction instead of by instruction. It is a refusal on
	// anything but a review task.
	Worktree bool

	// Caller is the pane that asked for the work, when one was named. It is
	// carried, never guessed: empty means the caller is unknown, not that the
	// dispatcher is the caller. cap addresses the round's end to this pane.
	Caller string
}

// PriorLaunch names an earlier launch of the same task. It is set when a
// re-dispatch is allowed because that attempt was never seen to start.
type PriorLaunch struct {
	Session string `json:"session"`
	Pane    string `json:"pane,omitempty"`
	At      string `json:"at,omitempty"`
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
	// CallerPane is who asked for this launch, when it was known. A finished
	// round can then be addressed to the pane that is waiting for it.
	CallerPane string `json:"caller_pane,omitempty"`
	// Unconfirmed is true when fs did not see the agent take the task: a
	// --no-wait launch, which returns without looking.
	Unconfirmed bool         `json:"unconfirmed"`
	Previous    *PriorLaunch `json:"previous,omitempty"`
}

// Dispatch starts one agent for one task. fs chooses the session id, hands it to
// the agent in the prompt, waits for the agent to take the task, and records what
// happened.
//
// The order matters and is the reason the record is worth having:
//
//  1. Read only: does the task exist, is it closed, does it have a live holder, is
//     the agent already launched for it still running, is there a directory to
//     work in? All refusals happen here, before anything is started, so a typo
//     cannot leave an orphan pane behind.
//  2. Start the agent. A failure here is reported and not retried.
//  3. Wait for one of two signals from that session: the task is claimed by it, or
//     any evidence arrives from it. A pane that exists is not evidence that the
//     agent took the prompt, so a launch is only successful once a row says so.
//  4. Record `launch:<tid>:<sid>` with the kind, the pane, the directory and the
//     outcome. On a timeout the outcome is unconfirmed, the pane stays open (a
//     slow start must not be killed), and the dispatch is a refusal.
//
// The agent claims itself, under the id in the prompt, so the bind is still the
// agent's own write: fs starts the process, it does not claim on the agent's
// behalf. An agent that never claims leaves a launch record and no claim, which is
// exactly what `reap` needs to find it — and what makes a re-dispatch safe.
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
	var previous *PriorLaunch
	if prev, ok := latestLaunch(rows, task.Tid); ok {
		if prev.Outcome == OutcomeUnconfirmed {
			// A launch nobody saw the agent take is not a worker. The no-retry rule
			// is about a launcher that fails, not an agent that never woke, so a
			// re-dispatch is the recovery — and it names the attempt it replaces.
			previous = &PriorLaunch{
				Session: prev.Session, Pane: prev.Pane,
				At: prev.At.UTC().Format(time.RFC3339),
			}
		} else {
			// A row and a clock cannot tell a dead agent from a slow one: a lapsed
			// lease means the session stopped writing, and silence is not death. Ask.
			live, err := l.List()
			if err != nil {
				return DispatchResult{}, fmt.Errorf("cannot see the agents already running (%w); refusing to start a second one for %s", err, task.Tid)
			}
			if status, alive := live[prev.Pane]; alive {
				return DispatchResult{}, fmt.Errorf("task %s is already being worked on by session %s (agent %s, pane %s, %s); it is alive even if its lease lapsed — run fs reap or wait for it to finish",
					task.Tid, prev.Session, prev.Name, prev.Pane, status)
			}
		}
	}
	var dir, repo string
	created := false
	if in.Worktree {
		dir, repo, err = mintReviewWorktree(rows, ix, now, task, in.Dir)
		if err != nil {
			return DispatchResult{}, err
		}
		created = true
	} else {
		dir, err = resolveDir(in.Dir, rows, task, in.Project)
		if err != nil {
			return DispatchResult{}, err
		}
		if err := refuseReviewInDevCheckout(rows, task, dir); err != nil {
			return DispatchResult{}, err
		}
	}

	sid := dispatchSession(task.Tid, now)
	name := dispatchName(task.Tid, now)
	prompt := dispatchPrompt(task, in.Project, sid, dir, s.Path())

	handle, err := l.Start(LaunchSpec{Name: name, Kind: in.Kind, Dir: dir, Prompt: prompt})
	if err != nil {
		if created {
			// Nothing ever ran in the directory fs just made, and a refusal must not
			// leave the litter this round exists to remove.
			_ = removeWorktree(repo, dir)
		}
		return DispatchResult{}, fmt.Errorf("launcher refused to start %s: %w", task.Tid, err)
	}

	outcome := OutcomeStarted
	unconfirmed := in.NoWait
	var waitErr error
	if !in.NoWait {
		window := in.Wait
		if window <= 0 {
			window = DefaultDispatchWait
		}
		took, err := waitForAgent(s, scope, task.Tid, sid, window)
		switch {
		case err != nil:
			unconfirmed, outcome = true, OutcomeUnconfirmed
			waitErr = fmt.Errorf("cannot read the store while waiting for session %s (pane %s): %w", sid, handle.Pane, err)
		case !took:
			unconfirmed, outcome = true, OutcomeUnconfirmed
			waitErr = fmt.Errorf("session %s did not take task %s within %s (pane %s); the pane is left open",
				sid, task.Tid, window, handle.Pane)
		}
	}

	extra, err := json.Marshal(launchExtra{
		Task: task.Tid, Session: sid, Kind: in.Kind, Role: task.Kind,
		Pane: handle.Pane, Name: handle.Name, Dir: dir, At: now.UTC(), Outcome: outcome,
		CallerPane: in.Caller, Worktree: created,
	})
	if err != nil {
		return DispatchResult{}, err
	}
	if err := s.PutLaunch(scope, task.Tid, sid, fmt.Sprintf("%s %s for %s", outcome, in.Kind, task.Tid), string(extra)); err != nil {
		// The agent is running and fs cannot say so. Closing it is the honest move:
		// a process fs cannot account for is exactly what reap exists to prevent.
		_ = l.Close(handle)
		return DispatchResult{}, err
	}
	if waitErr != nil {
		return DispatchResult{}, waitErr
	}
	return DispatchResult{
		Task: task.Tid, Kind: in.Kind, Session: sid,
		Pane: handle.Pane, Name: handle.Name, Dir: dir, Prompt: prompt,
		CallerPane: in.Caller, Unconfirmed: unconfirmed, Previous: previous,
	}, nil
}

// refuseReviewInDevCheckout asks the gate's question about independence one step
// earlier. A review judges a tree its author did not work in, so it runs in a
// checkout of its own; asking at the signature refuses a reviewer after it has read
// the code and run every check, while asking here refuses a dispatch that was never
// started. A development task's own dispatch is untouched — the question only has an
// answer when the task names the development task it reviews. A development row
// written before paths were recorded names no directory, and nothing is refused on a
// fact that was never recorded.
func refuseReviewInDevCheckout(rows []memory.Row, review Task, dir string) error {
	if review.ParentDev == "" {
		return nil
	}
	dev, err := loadTask(rows, review.ParentDev)
	if err != nil {
		return err
	}
	if dev.ProjectPath == "" || sameDir(dir) != sameDir(dev.ProjectPath) {
		return nil
	}
	return fmt.Errorf("refusing to dispatch review %s into %s: %s was developed in %s, one directory — a review runs in a checkout of its own: `git worktree add <dir> <commit>` at the commit under review, then dispatch again with --dir <dir>",
		review.Tid, dir, dev.Tid, dev.ProjectPath)
}

// waitForAgent waits until sid claims tid or writes evidence on it. It answers
// false when the window closes with neither. An unreadable store is an error: a
// dispatcher that cannot see the signal cannot confirm anything.
func waitForAgent(s *memory.Store, scope, tid, sid string, window time.Duration) (bool, error) {
	deadline := time.Now().Add(window)
	for {
		rows, err := s.Rows(scope)
		if err != nil {
			return false, err
		}
		ix := memory.NewIndex(rows)
		if held, ok := ix.Claim(tid); ok && held.Session == sid {
			return true, nil
		}
		for _, ev := range ix.Events(tid) {
			if esid, ok := memory.ParseSession(ev.OriginID); ok && esid == sid {
				return true, nil
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, nil
		}
		if remaining < dispatchPoll {
			time.Sleep(remaining)
		} else {
			time.Sleep(dispatchPoll)
		}
	}
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
		b.WriteString("\n3. Record each run as you finish it, with its real exit code, against the commit you are reviewing:\n")
		b.WriteString("   fs report --project " + project + " --session " + sid + " --task " + task.Tid +
			" --role review --state pass --command '<the exact command>' --exit <exit code> --digest '<the commit under review>'\n")
		b.WriteString("   Use --state fail when the exit was non-zero. Never report a check you did not run.\n")
		b.WriteString("   --digest is what the gate compares with the developer's dev-done: name the commit you reviewed, and a verdict for a different tree is refused.\n\n")
		// The three questions, one line each, matched to the three slots: the close
		// refuses until each has a passing answer at the reviewed commit, and the
		// reviewer chooses the command that answers it.
		b.WriteString("4. Answer the three questions the close asks, one --slot each, at the same digest. No answer, no close:\n")
		b.WriteString("   spec        are the specs honoured? run the project's own spec check (observe, a test named for an acceptance criterion, or a demo script); when no spec governs this change, record the gap: --slot spec --none '<why>'\n")
		b.WriteString("   tests       are the code and tests good? the declared checks above, plus scripts/changed-needs-test.sh <parent commit>: it exits non-zero when the change touches a non-test .go file and no test file, and prints one line to paste into --note\n")
		b.WriteString("   regression  did this change break anything? go test -race ./... or the project's end-to-end suite, or the change's own test at the parent commit (it must fail there) and at this one (it must pass)\n")
		b.WriteString("   fs report --project " + project + " --session " + sid + " --task " + task.Tid +
			" --slot spec|tests|regression --state pass --command '<what answered it>' --exit 0 --digest '<the commit under review>' --note '<one line>'\n\n")
		b.WriteString("5. While you still have reading to do, keep the lease alive: " +
			"fs report --project " + project + " --session " + sid + " --task " + task.Tid +
			" --role review --state progress --note reviewing, at least every 90 s.\n")
		b.WriteString("   If a write is refused because the lease expired, re-run the claim with this same session id: it takes the next round and you can continue.\n\n")
		b.WriteString("6. If every check passed and every question is answered: fs done --project " + project + " --session " + sid + " --task " + task.Tid + ", then stop.\n")
		b.WriteString("   Run it from the directory you reviewed in, not the developer's checkout: the gate refuses a signature made in the directory the developer recorded.\n")
		b.WriteString("   If a check or a question failed: report --state fail with the --slot it answers and a --note saying what is wrong, then stop. Do not fix the code and do not run done: a fresh reviewer gets the next round.\n")
		return b.String()
	}
	b.WriteString("2. Do the work.\n\n")
	b.WriteString("3. Report as you go: fs report --project " + project + " --session " + sid + " --task " + task.Tid +
		" --state progress --note '<where you are>' --digest '<what changed>'. This write is your lease: it lapses 120 s after your newest write, and if it lapses you may re-claim with this same session id.\n\n")
	b.WriteString("4. Run the declared checks yourself before claiming done:\n")
	for _, c := range task.Checks {
		b.WriteString("   - " + c + "\n")
	}
	b.WriteString("\n5. Create its review task; the handover below is refused until it exists: fs add --project " + project +
		" --goal 'review " + task.Tid + "' --review-of " + task.Tid + "\n\n")
	b.WriteString("6. When the work is complete: fs report --project " + project + " --session " + sid + " --task " + task.Tid +
		" --state dev-done --digest '<commit>' --note '<what you changed>'. This does not complete the task, and it is refused without a --digest naming the commit you finished.\n\n")
	b.WriteString("7. Then start the review, which is how your task can close: fs dispatch --project " + project +
		" --task <the review task id> --worktree. You do not review your own work: the reviewer is a fresh session that runs the same checks.\n")
	b.WriteString("   --worktree makes the reviewer's checkout at the commit you reported dev-done with and starts it there. A review dispatched into your own checkout is refused.\n")
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

// launchExtra is a launch row's extra: what fs started, as whom, and whether the
// agent was ever seen to take the task.
type launchExtra struct {
	Task    string    `json:"task"`
	Session string    `json:"session"`
	Kind    string    `json:"kind"`
	Role    string    `json:"role"`
	Pane    string    `json:"pane,omitempty"`
	Name    string    `json:"name,omitempty"`
	Dir     string    `json:"dir"`
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome,omitempty"`
	// CallerPane is the pane that asked for the launch. Empty when no caller was
	// named: fs never invents one.
	CallerPane string `json:"caller_pane,omitempty"`
	// Worktree is true when fs made the directory itself with `git worktree add`.
	// fs removes only what it made, and only while it is clean.
	Worktree bool `json:"worktree,omitempty"`
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
// report means the agent is working. A completion refusal (`refused`) is not a
// verdict: it says the gate declined to close, not that a check failed, so it
// never ends the round.
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
