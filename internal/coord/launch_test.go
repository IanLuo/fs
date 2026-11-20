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
	status   map[string]string // pane -> working|idle; absent means gone
	closed   []string
	startErr error
	listErr  error
	onStart  func(coord.LaunchSpec) // what the started agent does, if anything
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
	if f.onStart != nil {
		f.onStart(spec)
	}
	return coord.Handle{Pane: pane, Name: spec.Name, Dir: spec.Dir}, nil
}

func (f *fakeLauncher) List() (map[string]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := map[string]string{}
	for pane, st := range f.status {
		out[pane] = st
	}
	return out, nil
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

	got, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid, Kind: "pi", Dir: t.TempDir()}, fake)
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
	// The handover is refused until the review task exists, so the prompt asks for
	// the review task before the dev-done, not after it.
	prompt := fake.started[0].Prompt
	addReviewAt := strings.Index(prompt, "--review-of "+task.Tid)
	devDoneAt := strings.Index(prompt, "--state dev-done")
	if addReviewAt < 0 || devDoneAt < 0 || addReviewAt > devDoneAt {
		t.Errorf("prompt does not create the review task before the dev-done:\n%s", prompt)
	}
	if !strings.Contains(prompt, "--state dev-done --digest") {
		t.Errorf("dev prompt does not carry the digest a dev-done needs:\n%s", prompt)
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

	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid}, &fakeLauncher{}); err == nil {
		t.Error("dispatched a task that is already held")
	} else if !strings.Contains(err.Error(), "already being worked on") {
		t.Errorf("refusal = %v, want the holder named", err)
	}

	other := add(t, s, "p")
	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: other.Tid}, &fakeLauncher{}); err == nil {
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
	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid}, fake); err == nil {
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
	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid, Dir: t.TempDir()}, fake); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !strings.Contains(fake.started[0].Prompt, "export MEMSTORE_DB="+s.Path()) {
		t.Errorf("prompt does not pin the store %q:\n%s", s.Path(), fake.started[0].Prompt)
	}
}

// TestDispatchRefusesASecondLaunch: a dispatch that runs twice — a double click, a
// retried command — must not put two agents on one task. The first agent may not
// have claimed yet, and after its lease lapses it may merely be silent, so the
// launcher is asked. Only a gone process frees the task.
func TestDispatchRefusesASecondLaunch(t *testing.T) {
	s, now := open(t)
	task := add(t, s, "p")
	fake := &fakeLauncher{}
	first, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid, Dir: t.TempDir()}, fake)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid, Dir: t.TempDir()}, fake); err == nil {
		t.Fatal("dispatched the same task twice while the first agent is running")
	}

	// Silence is not death: a lapsed lease with a live process still refuses.
	*now = now.Add(2 * memory.DefaultTTL)
	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid, Dir: t.TempDir()}, fake); err == nil {
		t.Fatal("dispatched over an agent whose lease lapsed but whose process is alive")
	}

	// A process that is gone frees the task: that is the recovery path.
	delete(fake.status, first.Pane)
	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid, Dir: t.TempDir()}, fake); err != nil {
		t.Fatalf("re-dispatch after the agent died: %v", err)
	}
}

// TestDispatchRefusesWhenItCannotSee: if the launcher cannot answer, fs does not
// guess. Starting a second agent for a task that might already have one is worse
// than refusing.
func TestDispatchRefusesWhenItCannotSee(t *testing.T) {
	s, _ := open(t)
	task := add(t, s, "p")
	fake := &fakeLauncher{}
	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid, Dir: t.TempDir()}, fake); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	fake.listErr = errLauncher{}
	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid, Dir: t.TempDir()}, fake); err == nil {
		t.Fatal("dispatched while the launcher was unreadable")
	}
	if len(fake.started) != 1 {
		t.Errorf("started %d agents, want 1", len(fake.started))
	}
}

// TestDispatchAsksAReviewerForChecks: a review's prompt names every declared check
// and requires the exit code, because fs's gate accepts nothing less.
func TestDispatchAsksAReviewerForChecks(t *testing.T) {
	s, _ := open(t)
	dev := add(t, s, "p")
	review := addReview(t, s, "p", dev.Tid)
	fake := &fakeLauncher{}
	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: review.Tid, Dir: t.TempDir()}, fake); err != nil {
		t.Fatalf("dispatch review: %v", err)
	}
	prompt := fake.started[0].Prompt
	for _, want := range []string{"independent reviewer", dev.Checks[0], "--role review", "--exit", "--digest", "not trust the task text", "not the developer's checkout"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("reviewer prompt is missing %q:\n%s", want, prompt)
		}
	}
}

// TestDispatchRefusesAReviewInTheDeveloperCheckout: the separate checkout is a fact
// of the dispatch call, so fs asks it there — before an agent exists, where the
// refusal costs a dispatch instead of a finished review the gate then will not
// close. The message names both paths and the command that makes a worktree. A
// checkout of its own starts the review, a development task's own dispatch is
// unchanged, and a development row with no recorded path refuses nothing.
func TestDispatchRefusesAReviewInTheDeveloperCheckout(t *testing.T) {
	s, _ := open(t)
	checkout := t.TempDir()
	dev, err := coord.Add(s, coord.AddInput{Project: "p", Goal: "g", Checks: []string{"true"}, Path: checkout})
	if err != nil {
		t.Fatal(err)
	}
	review := addReview(t, s, "p", dev.Tid)
	fake := &fakeLauncher{}

	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: review.Tid, Dir: checkout}, fake); err == nil {
		t.Fatal("dispatched a review into the developer's checkout")
	} else {
		for _, want := range []string{checkout, dev.Tid, "git worktree add"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal = %v, want %q named", err, want)
			}
		}
	}
	if len(fake.started) != 0 {
		t.Errorf("started %d agents for a refused dispatch, want 0", len(fake.started))
	}
	if len(launchRows(t, s, "p")) != 0 {
		t.Error("a refused dispatch wrote a launch record")
	}

	// A worktree of its own is where the review belongs: it starts, and the launch
	// record names the directory it was started in.
	worktree := t.TempDir()
	started, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: review.Tid, Dir: worktree}, fake)
	if err != nil {
		t.Fatalf("dispatch into a worktree: %v", err)
	}
	if started.Dir != worktree {
		t.Errorf("launch dir = %q, want %q", started.Dir, worktree)
	}
	if len(launchRows(t, s, "p")) != 1 {
		t.Errorf("launch records = %d, want 1", len(launchRows(t, s, "p")))
	}

	// The developer's own dispatch is unchanged: it works where the work is.
	devLaunch := &fakeLauncher{}
	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: dev.Tid, Dir: checkout}, devLaunch); err != nil {
		t.Fatalf("dispatch of the development task: %v", err)
	}
	if !strings.Contains(devLaunch.started[0].Prompt, "worktree of its own") {
		t.Errorf("the developer's prompt does not say where to dispatch the review:\n%s", devLaunch.started[0].Prompt)
	}

	// A development row from before paths were recorded: there is no fact to compare.
	bare := add(t, s, "q")
	bareReview := addReview(t, s, "q", bare.Tid)
	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "q", Task: bareReview.Tid, Dir: t.TempDir()}, &fakeLauncher{}); err != nil {
		t.Fatalf("a review of a development task with no recorded path was refused: %v", err)
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
	devDone(t, s, "p", "s-dev", dev.Tid)
	started, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: review.Tid, Dir: t.TempDir()}, fake)
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
	busyLaunched, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: busy.Tid, Dir: t.TempDir()}, fake)
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
	got, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid}, fake)
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

// TestReapClosesAReviewThatEndedInFeedback: a `fail` ends the round, and the pane
// goes with it once that report is no longer keeping the claim alive — a reviewer
// that just wrote its verdict is still writing its last words, so a live claim is
// respected first. The next round is a new dispatch, not the same pane.
func TestReapClosesAReviewThatEndedInFeedback(t *testing.T) {
	s, now := open(t)
	dev := add(t, s, "p")
	review := addReview(t, s, "p", dev.Tid)
	claim(t, s, "p", "s-dev", dev.Tid)
	fake := &fakeLauncher{}
	got, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: review.Tid, Dir: t.TempDir()}, fake)
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
	*now = now.Add(2 * memory.DefaultTTL) // the verdict's own write stopped keeping the claim alive

	res, err := coord.Reap(s, coord.ReapInput{Project: "p"}, fake)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Closed) != 1 || !strings.Contains(res.Closed[0].Reason, "fail") {
		t.Fatalf("reap = %+v, want one close with the verdict named", res)
	}
}

var _ = time.Now

// TestStatusTellsSilenceFromDeath: the lease says whether a session is writing, the
// launcher says whether its process exists. A lapsed lease with a live process is a
// slow agent, not a dead one, and a task fs never launched reads unknown rather than
// gone — fs judges only what it opened.
func TestStatusTellsSilenceFromDeath(t *testing.T) {
	s, now := open(t)
	fake := &fakeLauncher{}

	launched := add(t, s, "p")
	got, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: launched.Tid, Dir: t.TempDir()}, fake)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := coord.Claim(s, coord.ClaimInput{Project: "p", Session: got.Session, Task: launched.Tid}); err != nil {
		t.Fatal(err)
	}
	hand := add(t, s, "p") // a pane somebody opened by hand: no launch record

	*now = now.Add(2 * memory.DefaultTTL) // every lease lapses; both processes stay
	rep, err := coord.Status(s, coord.StatusInput{Project: "p", Launcher: fake})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	byTask := map[string]coord.TaskStatus{}
	for _, task := range rep.Projects[0].Tasks {
		byTask[task.Task] = task
	}
	if p := byTask[launched.Tid]; p.Process.Status != coord.ProcessWorking {
		t.Errorf("launched task process = %q, want working: the agent is alive though silent", p.Process.Status)
	}
	if byTask[launched.Tid].LeaseLive {
		t.Error("the lease should read lapsed")
	}
	if p := byTask[hand.Tid]; p.Process.Status != coord.ProcessUnknown {
		t.Errorf("hand-opened task process = %q, want unknown", p.Process.Status)
	}

	// The process really ended: now it reads gone.
	delete(fake.status, got.Pane)
	rep, err = coord.Status(s, coord.StatusInput{Project: "p", Launcher: fake})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, task := range rep.Projects[0].Tasks {
		if task.Task == launched.Tid && task.Process.Status != coord.ProcessGone {
			t.Errorf("process = %q, want gone", task.Process.Status)
		}
	}
}

// TestStatusKeepsTheNewestCaller: a re-dispatch is a new launch, so status names
// the caller waiting for the round that is running now, not the one that asked
// first. A pane fs never opened has no caller at all.
func TestStatusKeepsTheNewestCaller(t *testing.T) {
	s, now := open(t)
	fake := &fakeLauncher{}
	task := add(t, s, "p")

	first, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid,
		Dir: t.TempDir(), Caller: "w1:first"}, fake)
	if err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	if first.CallerPane != "w1:first" {
		t.Errorf("first result caller = %q, want w1:first", first.CallerPane)
	}
	delete(fake.status, first.Pane) // the agent died: a re-dispatch is allowed
	*now = now.Add(time.Second)     // a new launch, not an overwrite of the same key

	second, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid,
		Dir: t.TempDir(), Caller: "w1:second"}, fake)
	if err != nil {
		t.Fatalf("re-dispatch: %v", err)
	}
	rep, err := coord.Status(s, coord.StatusInput{Project: "p", Launcher: fake})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	byTask := map[string]coord.TaskStatus{}
	for _, ts := range rep.Projects[0].Tasks {
		byTask[ts.Task] = ts
	}
	if got := byTask[task.Tid].Process.CallerPane; got != "w1:second" {
		t.Errorf("caller_pane = %q, want the newest caller %q", got, second.CallerPane)
	}
	hand := add(t, s, "p")
	rep, err = coord.Status(s, coord.StatusInput{Project: "p", Launcher: fake})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, ts := range rep.Projects[0].Tasks {
		if ts.Task == hand.Tid && ts.Process.CallerPane != "" {
			t.Errorf("unlaunched task caller_pane = %q, want none", ts.Process.CallerPane)
		}
	}
}

// TestStatusWithoutALauncherStillAnswers: a read never fails closed. Without a
// launcher, or with one that errors, the process reads unknown and the rows still
// answer.
func TestStatusWithoutALauncherStillAnswers(t *testing.T) {
	s, _ := open(t)
	task := add(t, s, "p")
	fake := &fakeLauncher{}
	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid, Dir: t.TempDir()}, fake); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	rep, err := coord.Status(s, coord.StatusInput{Project: "p"})
	if err != nil {
		t.Fatalf("status with no launcher: %v", err)
	}
	if p := rep.Projects[0].Tasks[0].Process.Status; p != coord.ProcessUnknown {
		t.Errorf("process = %q, want unknown", p)
	}

	fake.listErr = errLauncher{}
	rep, err = coord.Status(s, coord.StatusInput{Project: "p", Launcher: fake})
	if err != nil {
		t.Fatalf("status with a broken launcher: %v", err)
	}
	if rep.ProcessWarning == "" {
		t.Error("a broken launcher was not reported as a warning")
	}
	if p := rep.Projects[0].Tasks[0].Process.Status; p != coord.ProcessUnknown {
		t.Errorf("process = %q, want unknown when the launcher cannot answer", p)
	}
}

// TestReapKeepsAReviewerThatReclaimed: round 2 is the same session id claiming
// again after its lease lapsed. Its old `fail` is history — closing the pane then
// would kill the round that had just restarted, which is how a review gets burned.
func TestReapKeepsAReviewerThatReclaimed(t *testing.T) {
	s, now := open(t)
	dev := add(t, s, "p")
	review := addReview(t, s, "p", dev.Tid)
	claim(t, s, "p", "s-dev", dev.Tid)
	fake := &fakeLauncher{}
	got, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: review.Tid, Dir: t.TempDir()}, fake)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Claim(s, coord.ClaimInput{Project: "p", Session: got.Session, Task: review.Tid}); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Report(s, coord.ReportInput{
		Project: "p", Session: got.Session, Task: review.Tid, Role: "review",
		State: "fail", Note: "one check failed",
	}); err != nil {
		t.Fatal(err)
	}
	// The lease lapses, the same reviewer re-claims for round 2, and it is idle
	// between two checks.
	*now = now.Add(2 * memory.DefaultTTL)
	if _, err := coord.Claim(s, coord.ClaimInput{Project: "p", Session: got.Session, Task: review.Tid}); err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	fake.status[got.Pane] = coord.StatusIdle

	res, err := coord.Reap(s, coord.ReapInput{Project: "p"}, fake)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Closed {
		if c.Task == review.Tid {
			t.Fatalf("closed a reviewer that holds the round again: %+v", c)
		}
	}
}

// TestReapDoesNotReadACompletionRefusalAsAVerdict: the gate declining to close
// writes `refused`, not `fail`. That is not a verdict, so reap never ends the round
// on it; once the lease lapses the pane closes for the lease, with no verdict named.
func TestReapDoesNotReadACompletionRefusalAsAVerdict(t *testing.T) {
	s, now := open(t)
	dev := add(t, s, "p")
	review := addReview(t, s, "p", dev.Tid)
	claim(t, s, "p", "s-dev", dev.Tid)
	fake := &fakeLauncher{}
	got, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: review.Tid, Dir: t.TempDir()}, fake)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Claim(s, coord.ClaimInput{Project: "p", Session: got.Session, Task: review.Tid}); err != nil {
		t.Fatal(err)
	}
	// No run of the declared check: the gate refuses and records a refusal.
	if _, err := coord.Done(s, coord.DoneInput{Project: "p", Session: got.Session, Task: review.Tid}); err == nil {
		t.Fatal("completion passed with no reviewer-run check")
	}
	fake.status[got.Pane] = coord.StatusIdle
	*now = now.Add(2 * memory.DefaultTTL) // the refusal's own write no longer keeps the claim

	res, err := coord.Reap(s, coord.ReapInput{Project: "p"}, fake)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, c := range append(res.Closed, res.Kept...) {
		if c.Task != review.Tid {
			continue
		}
		seen = true
		if strings.Contains(c.Reason, "round ended") {
			t.Fatalf("a completion refusal was read as a verdict: %+v", c)
		}
	}
	if !seen {
		t.Fatalf("reap said nothing about the review: %+v", res)
	}
	assertOpen(t, s, review.Tid)
}
