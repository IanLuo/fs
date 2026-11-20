package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flagship-dev/flagship/internal/coord"
	"github.com/flagship-dev/flagship/internal/memory"
)

// invoke runs one command line against a scratch memstore and returns its exit
// code and decoded envelope. It is the whole CLI surface under test: flags in,
// JSON out, exit code out.
func invoke(t *testing.T, args ...string) (int, map[string]any) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	var env map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("fs %s: stdout is not JSON: %v\n%s", strings.Join(args, " "), err, stdout.String())
	}
	return code, env
}

// scratch points the CLI at a throwaway store for the duration of one test.
func scratch(t *testing.T) {
	t.Helper()
	t.Setenv("MEMSTORE_DB", filepath.Join(t.TempDir(), "memory.db"))
	t.Setenv("HOME", t.TempDir()) // the log sink writes under ~/.fs/logs

	// No verb may reach the host's herdr, or the test would be testing the
	// machine: a run on a laptop with herdr installed and one on a build host
	// without it must take the same path.
	old := launcher
	launcher = &stubLauncher{live: map[string]string{}, panes: map[string]string{}}
	t.Cleanup(func() { launcher = old })
}

// stubLauncher answers as herdr would, with no process and no PATH lookup.
type stubLauncher struct {
	live    map[string]string // pane -> working|idle
	panes   map[string]string // name -> pane, for the handles it issued
	closed  []string
	onStart func(coord.LaunchSpec) // what the started agent does, if anything
}

func (l *stubLauncher) Start(spec coord.LaunchSpec) (coord.Handle, error) {
	pane := "w1:" + spec.Name
	l.panes[spec.Name] = pane
	l.live[pane] = coord.StatusWorking
	if l.onStart != nil {
		l.onStart(spec)
	}
	return coord.Handle{Pane: pane, Name: spec.Name, Dir: spec.Dir}, nil
}

func (l *stubLauncher) List() (map[string]string, error) {
	out := map[string]string{}
	for pane, st := range l.live {
		out[pane] = st
	}
	return out, nil
}

func (l *stubLauncher) Close(h coord.Handle) error {
	l.closed = append(l.closed, h.Pane)
	delete(l.live, h.Pane)
	return nil
}

// okData asserts a successful envelope and returns its data.
func okData(t *testing.T, code int, env map[string]any) map[string]any {
	t.Helper()
	if code != 0 {
		t.Fatalf("exit %d, want 0: %v", code, env)
	}
	if env["ok"] != true {
		t.Fatalf("ok = %v, want true: %v", env["ok"], env)
	}
	data, _ := env["data"].(map[string]any)
	if data == nil {
		t.Fatalf("no data in %v", env)
	}
	return data
}

// TestHelpExitsZero: help is an answer, not a refusal. A wrapper that reads the
// exit code must not see a help call as a failed command, and a verb's own help
// must describe that verb rather than the whole interface.
func TestHelpExitsZero(t *testing.T) {
	scratch(t)
	verbs := []string{"add", "claim", "report", "done", "dispatch", "reap", "path", "status"}

	// Top-level help names every verb and describes none of them alone.
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("fs %v: exit %d, want 0", args, code)
		}
		for _, verb := range verbs {
			if !strings.Contains(stdout.String(), "fs "+verb) {
				t.Errorf("fs %v does not mention %q:\n%s", args, verb, stdout.String())
			}
		}
	}

	// Per-verb help names its own verb and no other.
	for _, verb := range verbs {
		for _, help := range []string{"--help", "-h"} {
			var stdout, stderr bytes.Buffer
			if code := run([]string{verb, help}, &stdout, &stderr); code != 0 {
				t.Fatalf("fs %s %s: exit %d, want 0", verb, help, code)
			}
			got := stdout.String()
			if !strings.Contains(got, "fs "+verb) {
				t.Errorf("fs %s %s does not describe %s:\n%s", verb, help, verb, got)
			}
			for _, other := range verbs {
				if other != verb && strings.Contains(got, "fs "+other) {
					t.Errorf("fs %s %s also describes %s:\n%s", verb, help, other, got)
				}
			}
		}
	}

	// Help answers even after a real flag, and writes nothing.
	var stdout, stderr bytes.Buffer
	if code := run([]string{"add", "--project", "p", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("fs add --project p --help: exit %d, want 0", code)
	}
	code, env := invoke(t, "status")
	if projects, _ := okData(t, code, env)["projects"].([]any); len(projects) != 0 {
		t.Fatalf("a help call wrote records: %v", projects)
	}
}

// TestHelpFlagIsNotStolenFromAValue: `--goal -h` sets a goal, it does not ask for
// help. The flag set decides what is a flag, not a scan of the arguments.
func TestHelpFlagIsNotStolenFromAValue(t *testing.T) {
	scratch(t)
	add := mustOK(t, "add", "--project", "p", "--goal", "-h", "--check", "true")
	if add["goal"] != "-h" {
		t.Fatalf("goal = %v, want -h: %v", add["goal"], add)
	}
}

// TestStatusTableIsForHumans: --format table renders the same report for a human —
// no JSON, no second read — while the default stays the envelope a script parses.
func TestStatusTableIsForHumans(t *testing.T) {
	scratch(t)
	tid, _ := mustOK(t, "add", "--project", "p", "--goal", "ship it", "--check", "go test ./...")["task"].(string)
	mustOK(t, "claim", "--project", "p", "--session", "s-1", "--task", tid)
	mustOK(t, "report", "--project", "p", "--session", "s-1", "--task", tid, "--state", "progress")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"status", "--format", "table"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, want 0: %s", code, stderr.String())
	}
	got := stdout.String()
	if strings.Contains(got, `"ok"`) {
		t.Errorf("--format table printed JSON:\n%s", got)
	}
	for _, want := range []string{"1 project, 1 task", "TASK", "p", tid, "claimed", "s-1", "ship it"} {
		if !strings.Contains(got, want) {
			t.Errorf("table is missing %q:\n%s", want, got)
		}
	}

	// The default answer is unchanged: the envelope every other verb prints.
	code, env := invoke(t, "status")
	if code != 0 || env["ok"] != true {
		t.Fatalf("default status = %d %v, want the JSON envelope", code, env)
	}
	if data, _ := env["data"].(map[string]any); data == nil || data["projects"] == nil {
		t.Fatalf("default status is not the JSON report: %v", env)
	}

	// A format fs does not have is a malformed command line, not a refusal.
	if code, _ := invoke(t, "status", "--format", "yaml"); code != 2 {
		t.Errorf("unknown format: exit %d, want 2", code)
	}
}

// TestStatusTableOnAnEmptyStore: a human asking about nothing gets a sentence,
// not a blank screen.
func TestStatusTableOnAnEmptyStore(t *testing.T) {
	scratch(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"status", "--format", "table"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if got := stdout.String(); !strings.Contains(got, "0 projects, 0 tasks") {
		t.Fatalf("empty table = %q", got)
	}
}

// TestColumnsKeepWhatDistinguishes: a session name leads with when it started, so
// a cut must keep its tail; a goal is prose, so its cut is at the end.
func TestColumnsKeepWhatDistinguishes(t *testing.T) {
	if got := tail("pi-2026-11-01-observe-5a2", 16); got != "…-01-observe-5a2" {
		t.Errorf("tail = %q, want the distinguishing end", got)
	}
	if got := tail("short", 16); got != "short" {
		t.Errorf("tail = %q, want short unchanged", got)
	}
	if got := cell("a goal long enough to be cut", 10); got != "a goal lo…" {
		t.Errorf("cell = %q, want the head plus an ellipsis", got)
	}
	if got := oneLine("two\nlines\there"); got != "two lines here" {
		t.Errorf("oneLine = %q, want one line", got)
	}
}

// TestStatusTableMarksAStaleReport: an old report still says what last happened,
// with one mark and one legend line, rather than collapsing to "unknown" and
// erasing the difference between an agent that finished and one that vanished.
func TestStatusTableMarksAStaleReport(t *testing.T) {
	age, at := 3600.0, time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	report := coord.StatusReport{Projects: []coord.ProjectStatus{{
		Project: "p",
		Tasks: []coord.TaskStatus{{
			Task: "t-1", Kind: "dev", State: "open", Session: "s-1", Round: 1,
			Goal:  "ship it",
			Agent: coord.Observed{State: "unknown", Reported: "dev-done", Fresh: false, Age: &age, At: &at},
		}},
	}}}
	got := renderStatus(report, at)
	if !strings.Contains(got, "dev-done~") {
		t.Errorf("table lost the last reported state:\n%s", got)
	}
	if !strings.Contains(got, "~ reported longer ago than the lease") {
		t.Errorf("table shows a mark with no legend:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "t-1") && strings.Contains(line, "unknown") {
			t.Errorf("the row shows the verdict instead of the report:\n%s", line)
		}
	}
}

// TestStatusTableShowsARefusalNotAFailure: a completion refusal is its own state,
// so the human view says "refused" instead of telling an operator that a passing
// review failed.
func TestStatusTableShowsARefusalNotAFailure(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	report := coord.StatusReport{Projects: []coord.ProjectStatus{{
		Project: "p",
		Tasks: []coord.TaskStatus{{
			Task: "t-1", Kind: "review", State: "claimed", Session: "s-rev", Round: 1,
			Goal:  "review t-0",
			Agent: coord.Observed{State: "refused", Reported: "refused", Fresh: true, Role: "review"},
		}},
	}}}
	got := renderStatus(report, at)
	if !strings.Contains(got, "refused") {
		t.Errorf("table does not show the refusal:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "t-1") && strings.Contains(line, "fail") {
			t.Errorf("table renders a refusal as a failure:\n%s", line)
		}
	}
}

// TestStatusTableShowsTheProcess: the PROCESS column is what the launcher said
// about the agent fs started. Working, idle and gone are three different answers,
// and "-" means fs launched nothing for the task — a pane somebody opened by hand,
// which fs does not judge.
func TestStatusTableShowsTheProcess(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	task := func(id, status string) coord.TaskStatus {
		return coord.TaskStatus{Task: id, Kind: "dev", State: "claimed",
			Process: coord.Process{Status: status},
			Agent:   coord.Observed{State: "unknown"},
			Goal:    "goal " + id}
	}
	report := coord.StatusReport{Projects: []coord.ProjectStatus{{
		Project: "p",
		Tasks: []coord.TaskStatus{
			task("t-working", coord.ProcessWorking),
			task("t-idle", coord.ProcessIdle),
			task("t-gone", coord.ProcessGone),
			task("t-unlaunched", coord.ProcessUnknown),
			task("t-unanswered", ""),
		},
	}}}
	got := renderStatus(report, at)
	if !strings.Contains(got, "PROCESS") {
		t.Fatalf("table has no PROCESS column:\n%s", got)
	}

	want := map[string]string{
		"t-working":    "working",
		"t-idle":       "idle",
		"t-gone":       "gone",
		"t-unlaunched": "-",
		"t-unanswered": "-",
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(got, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		status, ok := want[fields[0]]
		if !ok {
			continue
		}
		seen[fields[0]] = true
		if len(fields) < 8 {
			t.Errorf("row for %s is short: %q", fields[0], line)
			continue
		}
		if got := fields[7]; got != status {
			t.Errorf("PROCESS for %s = %q, want %q", fields[0], got, status)
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("no row for %s:\n%s", id, got)
		}
	}
}

// TestStatusTableSaysWhenTheProcessViewIsUnavailable: a launcher that cannot answer
// must not read as "gone". The table says why the cell is empty, so a silent agent
// is never mistaken for a dead one.
func TestStatusTableSaysWhenTheProcessViewIsUnavailable(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	report := coord.StatusReport{
		Projects: []coord.ProjectStatus{{Project: "p", Tasks: []coord.TaskStatus{{
			Task: "t-1", Kind: "dev", State: "open", Goal: "g",
			Process: coord.Process{Status: coord.ProcessUnknown},
		}}}},
		ProcessWarning: "cannot see running agents: herdr: no server",
	}
	got := renderStatus(report, at)
	if !strings.Contains(got, "process shown as unknown") {
		t.Errorf("table hides an unavailable process view:\n%s", got)
	}
	if !strings.Contains(got, "herdr: no server") {
		t.Errorf("table drops the reason:\n%s", got)
	}
}

// TestUsageExitsTwo: a malformed command line is a different answer from a refusal,
// and a script can tell them apart by the exit code alone.
func TestUsageExitsTwo(t *testing.T) {
	scratch(t)
	cases := [][]string{
		{},
		{"nonsense"},
		{"add", "--project"},        // a flag with no value
		{"claim", "--unknown-flag"}, // a flag fs does not have
		{"status", "extra"},
	}
	for _, args := range cases {
		code, env := invoke(t, args...)
		if code != 2 {
			t.Errorf("fs %v: exit %d, want 2", args, code)
		}
		if env["ok"] != false || env["error"] == "" {
			t.Errorf("fs %v: envelope = %v, want ok:false with an error", args, env)
		}
	}
}

// TestRefusalsExitOne: fs understood and refused. Zero writes, one JSON answer.
func TestRefusalsExitOne(t *testing.T) {
	scratch(t)
	cases := [][]string{
		{"add", "--project", "p", "--goal", "", "--check", "true"}, // no goal
		{"add", "--project", "", "--goal", "g", "--check", "true"}, // no scope
		{"add", "--project", "p", "--goal", "g"},                   // no declared check
		{"claim", "--project", "p"},                                // no session identity
		{"report", "--project", "p", "--session", "s", "--task", "t-1", "--state", "invented"},
		{"report", "--project", "p", "--session", "s", "--task", "t-1", "--state", "pass", "--command", "true"}, // no exit
		{"done", "--project", "p", "--session", "s", "--task", "t-1"},
	}
	for _, args := range cases {
		code, env := invoke(t, args...)
		if code != 1 {
			t.Errorf("fs %v: exit %d, want 1", args, code)
		}
		if env["ok"] != false {
			t.Errorf("fs %v: ok = %v, want false", args, env["ok"])
		}
	}
	// Nothing was written by any of them.
	code, env := invoke(t, "status")
	data := okData(t, code, env)
	if projects, _ := data["projects"].([]any); len(projects) != 0 {
		t.Fatalf("%d projects exist after every add was refused: %v", len(projects), projects)
	}
}

// TestFiveVerbsEndToOneCompletion walks the whole product once, through the CLI, and
// checks that only the reviewer's passing run closes anything.
func TestFiveVerbsEndToOneCompletion(t *testing.T) {
	scratch(t)

	add := mustOK(t, "add", "--project", "p", "--goal", "ship it", "--check", "go test ./...")
	dev, _ := add["task"].(string)
	if dev == "" {
		t.Fatalf("add returned no task id: %v", add)
	}

	claimed := mustOK(t, "claim", "--project", "p", "--session", "s-dev", "--task", dev)
	if claimed["session"] != "s-dev" || claimed["round"] != float64(1) {
		t.Fatalf("claim = %v, want s-dev at round 1", claimed)
	}
	// A second session finds nothing eligible: the only task is held.
	if code, env := invoke(t, "claim", "--project", "p", "--session", "s-other"); code != 1 {
		t.Fatalf("a second session claimed held work: %d %v", code, env)
	}

	// The round cannot leave development without a review task: dev-done is
	// refused until the review exists, and the refusal names the verb.
	if code, env := invoke(t, "report", "--project", "p", "--session", "s-dev", "--task", dev,
		"--state", "dev-done", "--digest", "abc123"); code != 1 {
		t.Fatalf("dev-done was recorded with no review task: %d %v", code, env)
	} else if msg, _ := env["error"].(string); !strings.Contains(msg, "--review-of") {
		t.Fatalf("refusal %q does not name the verb that creates the review", msg)
	}

	review := mustOK(t, "add", "--project", "p", "--goal", "review "+dev, "--review-of", dev)
	rev, _ := review["task"].(string)
	if checks, _ := review["checks"].([]any); len(checks) != 1 || checks[0] != "go test ./..." {
		t.Fatalf("the review did not inherit the declared checks: %v", review)
	}

	// Status derives where the round stands: the review is the open step, and the
	// development task is not between steps until it hands the work over.
	waiting := roundStates(t, "p")
	if waiting[rev] != "review-open" || waiting[dev] != "" {
		t.Fatalf("round_state = %v, want review-open on the review and nothing on the development task", waiting)
	}
	mustOK(t, "report", "--project", "p", "--session", "s-dev", "--task", dev,
		"--state", "dev-done", "--digest", "abc123", "--note", "ready for review")
	waiting = roundStates(t, "p")
	if waiting[dev] != "review-open" {
		t.Fatalf("round_state for %s = %q, want review-open while the review has no verdict", dev, waiting[dev])
	}

	// The developer cannot complete its own work.
	if code, env := invoke(t, "done", "--project", "p", "--session", "s-dev", "--task", dev); code != 1 {
		t.Fatalf("the developer completed its own task: %d %v", code, env)
	}

	mustOK(t, "claim", "--project", "p", "--session", "s-rev", "--task", rev)

	// No recorded run yet: refused.
	if code, env := invoke(t, "done", "--project", "p", "--session", "s-rev", "--task", rev); code != 1 {
		t.Fatalf("completion passed with no reviewer run: %d %v", code, env)
	}
	// A failing run: refused, and the exit code is the reason.
	if code, _ := invoke(t, "report", "--project", "p", "--session", "s-rev", "--task", rev,
		"--state", "fail", "--role", "review", "--command", "go test ./...", "--exit", "1"); code != 0 {
		t.Fatalf("a failing reviewer run was not recorded: %d", code)
	}
	if code, env := invoke(t, "done", "--project", "p", "--session", "s-rev", "--task", rev); code != 1 {
		t.Fatalf("completion passed on a failing run: %d %v", code, env)
	}
	// The passing run: completion.
	mustOK(t, "report", "--project", "p", "--session", "s-rev", "--task", rev,
		"--state", "pass", "--role", "review", "--command", "go test ./...", "--exit", "0",
		"--digest", "abc123")
	done := mustOK(t, "done", "--project", "p", "--session", "s-rev", "--task", rev)
	if done["dev"] != dev || done["review"] != rev {
		t.Fatalf("done = %v, want both tasks named", done)
	}
	if checks, _ := done["checks"].([]any); len(checks) != 1 {
		t.Fatalf("done recorded %d checks, want the one that closed it: %v", len(checks), done)
	}

	// Status agrees, from rows alone.
	status := mustOK(t, "status", "--project", "p")
	projects, _ := status["projects"].([]any)
	if len(projects) != 1 {
		t.Fatalf("status = %v, want one project", status)
	}
	states := map[string]string{}
	for _, raw := range projects[0].(map[string]any)["tasks"].([]any) {
		task := raw.(map[string]any)
		states[task["task"].(string)] = task["state"].(string)
	}
	if states[dev] != "done" || states[rev] != "done" {
		t.Fatalf("states = %v, want both done", states)
	}
}

// TestReportDefaultsTheRoleFromTheTask is item B through the CLI: a report that names
// no role takes it from the task, so the verdict a hand-run reviewer writes is the one
// the gate reads.
func TestReportDefaultsTheRoleFromTheTask(t *testing.T) {
	scratch(t)
	add := mustOK(t, "add", "--project", "p", "--goal", "ship it", "--check", "true")
	dev, _ := add["task"].(string)
	review := mustOK(t, "add", "--project", "p", "--goal", "review "+dev, "--review-of", dev)
	rev, _ := review["task"].(string)
	mustOK(t, "claim", "--project", "p", "--session", "s-dev", "--task", dev)
	mustOK(t, "report", "--project", "p", "--session", "s-dev", "--task", dev,
		"--state", "dev-done", "--digest", "abc123")
	mustOK(t, "claim", "--project", "p", "--session", "s-rev", "--task", rev)

	// No --role: the review task supplies it, and the report says so.
	pass := mustOK(t, "report", "--project", "p", "--session", "s-rev", "--task", rev,
		"--state", "pass", "--command", "true", "--exit", "0", "--digest", "abc123")
	if pass["role"] != "review" {
		t.Fatalf("role = %v, want review for a report on a review task that named none", pass["role"])
	}

	// The gate reads that verdict: the round closes with no second report.
	if done := mustOK(t, "done", "--project", "p", "--session", "s-rev", "--task", rev); done["review"] != rev || done["dev"] != dev {
		t.Fatalf("done = %v, want both tasks named", done)
	}
}

// TestDoneSignsFromItsWorkingDirectory is P1-2 through the CLI: the gate reads the
// directory the command runs in, so a reviewer signing from the developer's
// checkout is refused and a signature from anywhere else closes the round.
func TestDoneSignsFromItsWorkingDirectory(t *testing.T) {
	scratch(t)
	checkout := t.TempDir()
	add := mustOK(t, "add", "--project", "p", "--goal", "ship it", "--check", "true", "--path", checkout)
	dev, _ := add["task"].(string)
	review := mustOK(t, "add", "--project", "p", "--goal", "review "+dev, "--review-of", dev)
	rev, _ := review["task"].(string)
	mustOK(t, "claim", "--project", "p", "--session", "s-dev", "--task", dev)
	mustOK(t, "report", "--project", "p", "--session", "s-dev", "--task", dev,
		"--state", "dev-done", "--digest", "abc123")
	mustOK(t, "claim", "--project", "p", "--session", "s-rev", "--task", rev)
	mustOK(t, "report", "--project", "p", "--session", "s-rev", "--task", rev,
		"--state", "pass", "--role", "review", "--command", "true", "--exit", "0", "--digest", "abc123")

	// The reviewer is standing in the developer's checkout: refused, and the refusal
	// names both the task and the directory the command ran in.
	t.Chdir(checkout)
	code, env := invoke(t, "done", "--project", "p", "--session", "s-rev", "--task", rev)
	if code != 1 {
		t.Fatalf("completion passed from the developer's checkout: %d %v", code, env)
	}
	msg, _ := env["error"].(string)
	if !strings.Contains(msg, checkout) || !strings.Contains(msg, dev) {
		t.Fatalf("refusal %q does not name %s and the directory %s", msg, dev, checkout)
	}

	// From its own directory the same signature closes, and the record says where.
	own := t.TempDir()
	t.Chdir(own)
	mustOK(t, "done", "--project", "p", "--session", "s-rev", "--task", rev)
	status := mustOK(t, "status", "--project", "p")
	var closed string
	projects, _ := status["projects"].([]any)
	for _, raw := range projects[0].(map[string]any)["tasks"].([]any) {
		task := raw.(map[string]any)
		if task["task"] != rev {
			continue
		}
		events, _ := task["evidence"].([]any)
		for _, rawEvent := range events {
			ev := rawEvent.(map[string]any)
			if ev["state"] == "closed" {
				closed, _ = ev["note"].(string)
			}
		}
	}
	if !strings.Contains(closed, own) {
		t.Fatalf("close note = %q, want the signing directory %s", closed, own)
	}
}

// roundStates reads one project's derived round state per task from the status
// envelope: the field a caller sees between a round's steps.
func roundStates(t *testing.T, project string) map[string]string {
	t.Helper()
	status := mustOK(t, "status", "--project", project)
	projects, _ := status["projects"].([]any)
	out := map[string]string{}
	for _, rawProject := range projects {
		tasks, _ := rawProject.(map[string]any)["tasks"].([]any)
		for _, rawTask := range tasks {
			task := rawTask.(map[string]any)
			state, _ := task["round_state"].(string)
			out[task["task"].(string)] = state
		}
	}
	return out
}

// TestStorePathComesFromTheEnvironment: fs opens the shared file it is told to open,
// and nothing of its own.
func TestStorePathComesFromTheEnvironment(t *testing.T) {
	db := filepath.Join(t.TempDir(), "named.db")
	t.Setenv("MEMSTORE_DB", db)
	t.Setenv("HOME", t.TempDir())

	mustOK(t, "add", "--project", "p", "--goal", "g", "--check", "true")
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("the named store was not created: %v", err)
	}
}

// mustOK invokes a command that has to succeed and returns its data.
func mustOK(t *testing.T, args ...string) map[string]any {
	t.Helper()
	code, env := invoke(t, args...)
	if code != 0 {
		t.Fatalf("fs %s: exit %d: %v", strings.Join(args, " "), code, env)
	}
	return okData(t, code, env)
}

// TestStatusShowsTheProcessViewFromTheLauncher: the PROCESS column is what the
// launcher says about the agent fs started, and nothing else. Silence in the rows
// is not the answer — a live agent that stopped writing must still read working.
func TestStatusShowsTheProcessViewFromTheLauncher(t *testing.T) {
	scratch(t)
	stub := launcher.(*stubLauncher)

	tid := mustOK(t, "add", "--project", "p", "--goal", "ship it", "--check", "true")["task"].(string)
	mustOK(t, "dispatch", "--project", "p", "--task", tid, "--dir", t.TempDir(), "--no-wait")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"status", "--project", "p", "--format", "table"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, want 0: %s", code, stderr.String())
	}
	row := ""
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.Contains(line, tid) {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("no row for %s:\n%s", tid, stdout.String())
	}
	if !strings.Contains(row, "working") {
		t.Errorf("row does not carry the launcher's answer:\n%s", row)
	}

	// The agent ends: the same task now reads gone, without a lease ever moving.
	for name, pane := range stub.panes {
		if strings.HasPrefix(name, "fs-") {
			delete(stub.live, pane)
		}
	}
	stdout.Reset()
	if code := run([]string{"status", "--project", "p", "--format", "table"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, want 0: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "gone") {
		t.Errorf("a finished agent does not read gone:\n%s", stdout.String())
	}
}

// TestPathMovesTheProjectWithoutTouchingATask: one command re-points the project,
// the next dispatch starts in the new directory, and the task row still records
// where it was written down. Status shows the directory the project would dispatch
// into, in both views.
func TestPathMovesTheProjectWithoutTouchingATask(t *testing.T) {
	scratch(t)
	old, moved := t.TempDir(), t.TempDir()
	tid := mustOK(t, "add", "--project", "p", "--goal", "ship it", "--check", "true", "--path", old)["task"].(string)

	bound := mustOK(t, "path", "--project", "p", "--path", moved)
	if bound["project"] != "p" || bound["path"] != moved {
		t.Fatalf("path = %v, want p in %s", bound, moved)
	}

	started := mustOK(t, "dispatch", "--project", "p", "--task", tid, "--no-wait")
	if started["dir"] != moved {
		t.Errorf("dispatched into %v, want the project binding %s", started["dir"], moved)
	}

	status := mustOK(t, "status", "--project", "p")
	project := status["projects"].([]any)[0].(map[string]any)
	if project["dir"] != moved {
		t.Errorf("status dir = %v, want %s", project["dir"], moved)
	}
	if got := project["tasks"].([]any)[0].(map[string]any)["project_path"]; got != old {
		t.Errorf("task path = %v, want the recorded %s: a move touches no task row", got, old)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"status", "--project", "p", "--format", "table"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, want 0: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), moved) {
		t.Errorf("table does not show the directory the project dispatches into:\n%s", stdout.String())
	}
}

// TestDispatchRefusesADeadPathThroughTheCLI: the refusal is one JSON answer naming
// the task and the path, and no agent is started — the pane list stays empty.
func TestDispatchRefusesADeadPathThroughTheCLI(t *testing.T) {
	scratch(t)
	stub := launcher.(*stubLauncher)
	dead := filepath.Join(t.TempDir(), "gone")
	tid := mustOK(t, "add", "--project", "p", "--goal", "ship it", "--check", "true", "--path", dead)["task"].(string)

	code, env := invoke(t, "dispatch", "--project", "p", "--task", tid)
	if code != 1 {
		t.Fatalf("exit %d, want 1: %v", code, env)
	}
	msg, _ := env["error"].(string)
	if !strings.Contains(msg, tid) || !strings.Contains(msg, dead) || strings.Contains(msg, "\n") {
		t.Errorf("refusal = %q, want one line naming the task and the path", msg)
	}
	if len(stub.panes) != 0 {
		t.Errorf("a pane was started for a dead path: %v", stub.panes)
	}
}

// TestPathRefusalsExitOne: a binding with no project or no directory is refused
// without writing anything.
func TestPathRefusalsExitOne(t *testing.T) {
	scratch(t)
	for _, args := range [][]string{
		{"path", "--project", "p"},
		{"path", "--path", t.TempDir()},
	} {
		if code, env := invoke(t, args...); code != 1 || env["ok"] != false {
			t.Errorf("fs %v: exit %d %v, want a refusal", args, code, env)
		}
	}
	if projects := mustOK(t, "status")["projects"].([]any); len(projects) != 0 {
		t.Fatalf("%d projects exist after every path was refused: %v", len(projects), projects)
	}
}

// promptSession reads the identity fs minted for the agent out of the prompt.
func promptSession(t *testing.T, prompt string) string {
	t.Helper()
	fields := strings.Fields(prompt)
	for i, f := range fields {
		if f == "session" && i+1 < len(fields) {
			return strings.TrimSuffix(fields[i+1], ".")
		}
	}
	t.Fatalf("prompt names no session:\n%s", prompt)
	return ""
}

// latestLaunchOutcome reads the outcome on the newest launch row for a task.
func latestLaunchOutcome(t *testing.T, tid string) string {
	t.Helper()
	s, err := memory.Open(os.Getenv("MEMSTORE_DB"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	rows, err := s.Rows(memory.Scope("p"))
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	outcome := ""
	for _, r := range rows {
		if r.Kind != memory.KindLaunch || r.Task() != tid {
			continue
		}
		var ex struct {
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal([]byte(r.Extra), &ex); err != nil {
			t.Fatalf("launch extra: %v", err)
		}
		outcome = ex.Outcome // newest first
		break
	}
	return outcome
}

// latestLaunchCaller reads caller_pane from the newest launch row for a task. The
// second result says whether the field was there at all, so a test can tell an
// empty caller from a missing one.
func latestLaunchCaller(t *testing.T, tid string) (string, bool) {
	t.Helper()
	s, err := memory.Open(os.Getenv("MEMSTORE_DB"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	rows, err := s.Rows(memory.Scope("p"))
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	for _, r := range rows {
		if r.Kind != memory.KindLaunch || r.Task() != tid {
			continue
		}
		var ex struct {
			CallerPane *string `json:"caller_pane"`
		}
		if err := json.Unmarshal([]byte(r.Extra), &ex); err != nil {
			t.Fatalf("launch extra: %v", err)
		}
		if ex.CallerPane == nil {
			return "", false
		}
		return *ex.CallerPane, true
	}
	return "", false
}

// TestDispatchRecordsTheCallerPane: the caller is the pane that asked for the
// work. fs records it on the launch row and reports it in the result, so the
// finished round can be addressed instead of broadcast.
func TestDispatchRecordsTheCallerPane(t *testing.T) {
	scratch(t)
	tid := mustOK(t, "add", "--project", "p", "--goal", "ship it", "--check", "true")["task"].(string)

	data := mustOK(t, "dispatch", "--project", "p", "--task", tid, "--dir", t.TempDir(),
		"--no-wait", "--caller-pane", "w1:caller")
	if got := data["caller_pane"]; got != "w1:caller" {
		t.Errorf("dispatch result caller_pane = %v, want w1:caller", got)
	}
	if got, ok := latestLaunchCaller(t, tid); !ok || got != "w1:caller" {
		t.Errorf("launch row caller_pane = %q (present %v), want w1:caller", got, ok)
	}
}

// TestDispatchCallerPaneComesFromTheFlagOrTheHarness: --caller-pane wins when it
// is given; otherwise the harness's own pane id is the caller. The flag is what
// lets a caller say who it is when the harness exports nothing.
func TestDispatchCallerPaneComesFromTheFlagOrTheHarness(t *testing.T) {
	scratch(t)
	t.Setenv("HERDR_PANE_ID", "w9:harness")

	flagTid := mustOK(t, "add", "--project", "p", "--goal", "flag wins", "--check", "true")["task"].(string)
	data := mustOK(t, "dispatch", "--project", "p", "--task", flagTid, "--dir", t.TempDir(),
		"--no-wait", "--caller-pane", "w1:typed")
	if got := data["caller_pane"]; got != "w1:typed" {
		t.Errorf("with both, caller_pane = %v, want the flag w1:typed", got)
	}

	envTid := mustOK(t, "add", "--project", "p", "--goal", "env wins", "--check", "true")["task"].(string)
	data = mustOK(t, "dispatch", "--project", "p", "--task", envTid, "--dir", t.TempDir(), "--no-wait")
	if got := data["caller_pane"]; got != "w9:harness" {
		t.Errorf("without the flag, caller_pane = %v, want $HERDR_PANE_ID w9:harness", got)
	}
	if got, ok := latestLaunchCaller(t, envTid); !ok || got != "w9:harness" {
		t.Errorf("launch row caller_pane = %q (present %v), want w9:harness", got, ok)
	}
}

// TestDispatchWithoutACallerInventsNone: fs never guesses a pane. With no flag and
// no harness variable the caller is simply unknown, and the field is absent.
func TestDispatchWithoutACallerInventsNone(t *testing.T) {
	scratch(t)
	t.Setenv("HERDR_PANE_ID", "")
	tid := mustOK(t, "add", "--project", "p", "--goal", "ship it", "--check", "true")["task"].(string)

	data := mustOK(t, "dispatch", "--project", "p", "--task", tid, "--dir", t.TempDir(), "--no-wait")
	if got, ok := data["caller_pane"]; ok {
		t.Errorf("dispatch invented a caller: %v", got)
	}
	if got, ok := latestLaunchCaller(t, tid); ok {
		t.Errorf("launch row invented a caller: %q", got)
	}
}

// TestStatusCarriesTheCallerPane: status answers who is waiting without a second
// query, and says nothing about a task fs never launched, rather than inventing a
// pane.
func TestStatusCarriesTheCallerPane(t *testing.T) {
	scratch(t)
	t.Setenv("HERDR_PANE_ID", "")

	tid := mustOK(t, "add", "--project", "p", "--goal", "ship it", "--check", "true")["task"].(string)
	mustOK(t, "dispatch", "--project", "p", "--task", tid, "--dir", t.TempDir(), "--no-wait", "--caller-pane", "w1:caller")
	hand := mustOK(t, "add", "--project", "p", "--goal", "never launched", "--check", "true")["task"].(string)

	data := mustOK(t, "status", "--project", "p")
	projects := data["projects"].([]any)
	tasks := projects[0].(map[string]any)["tasks"].([]any)
	byTask := map[string]map[string]any{}
	for _, raw := range tasks {
		task := raw.(map[string]any)
		byTask[task["task"].(string)] = task
	}
	process, _ := byTask[tid]["process"].(map[string]any)
	if got := process["caller_pane"]; got != "w1:caller" {
		t.Errorf("status caller_pane = %v, want w1:caller", got)
	}
	process, _ = byTask[hand]["process"].(map[string]any)
	if got, ok := process["caller_pane"]; ok {
		t.Errorf("status invented a caller for an unlaunched task: %v", got)
	}
}

// TestDispatchRefusesAnAgentThatNeverTakesThePrompt: a pane and a printed ok is
// not evidence the agent woke. The refusal exits non-zero, names the pane and the
// session, records an unconfirmed launch, and leaves the pane open.
func TestDispatchRefusesAnAgentThatNeverTakesThePrompt(t *testing.T) {
	scratch(t)
	stub := launcher.(*stubLauncher)
	tid := mustOK(t, "add", "--project", "p", "--goal", "ship it", "--check", "true")["task"].(string)

	code, env := invoke(t, "dispatch", "--project", "p", "--task", tid, "--dir", t.TempDir(),
		"--wait-seconds", "1", "--caller-pane", "w1:waiting")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %v", code, env)
	}
	// A launch nobody saw start still has a caller waiting, so it is recorded.
	if got, ok := latestLaunchCaller(t, tid); !ok || got != "w1:waiting" {
		t.Errorf("unconfirmed launch caller_pane = %q (present %v), want w1:waiting", got, ok)
	}
	msg, _ := env["error"].(string)
	if strings.Contains(msg, "\n") {
		t.Errorf("refusal is more than one line: %q", msg)
	}
	if len(stub.panes) != 1 {
		t.Fatalf("started %d panes, want 1", len(stub.panes))
	}
	var pane string
	for _, p := range stub.panes {
		pane = p
	}
	if !strings.Contains(msg, pane) || !strings.Contains(msg, "session d-") {
		t.Errorf("refusal = %q, want the pane %q and the session named", msg, pane)
	}
	if len(stub.closed) != 0 || len(stub.live) != 1 {
		t.Errorf("the slow pane was closed: closed %v, live %v", stub.closed, stub.live)
	}
	if got := latestLaunchOutcome(t, tid); got != coord.OutcomeUnconfirmed {
		t.Errorf("launch outcome = %q, want %q", got, coord.OutcomeUnconfirmed)
	}
}

// TestDispatchWaitsForTheClaimingAgent: dispatch returns ok only after the
// dispatched session has taken the task.
func TestDispatchWaitsForTheClaimingAgent(t *testing.T) {
	scratch(t)
	stub := launcher.(*stubLauncher)
	tid := mustOK(t, "add", "--project", "p", "--goal", "ship it", "--check", "true")["task"].(string)

	stub.onStart = func(spec coord.LaunchSpec) {
		sid := promptSession(t, spec.Prompt)
		mustOK(t, "claim", "--project", "p", "--session", sid, "--task", tid)
	}
	started := mustOK(t, "dispatch", "--project", "p", "--task", tid, "--dir", t.TempDir(), "--wait-seconds", "2")
	if started["unconfirmed"] != false {
		t.Errorf("a claimed task reads unconfirmed: %v", started)
	}
	if got := latestLaunchOutcome(t, tid); got != coord.OutcomeStarted {
		t.Errorf("launch outcome = %q, want %q", got, coord.OutcomeStarted)
	}
}

// TestDispatchNoWaitReturnsUnconfirmed: fire-and-forget returns at once, and the
// output says the launch was not confirmed.
func TestDispatchNoWaitReturnsUnconfirmed(t *testing.T) {
	scratch(t)
	tid := mustOK(t, "add", "--project", "p", "--goal", "ship it", "--check", "true")["task"].(string)

	begin := time.Now()
	data := mustOK(t, "dispatch", "--project", "p", "--task", tid, "--dir", t.TempDir(), "--no-wait")
	if elapsed := time.Since(begin); elapsed > 2*time.Second {
		t.Errorf("--no-wait took %s", elapsed)
	}
	if data["unconfirmed"] != true {
		t.Errorf("--no-wait output does not mark the launch unconfirmed: %v", data)
	}
	if got := latestLaunchOutcome(t, tid); got != coord.OutcomeStarted {
		t.Errorf("launch outcome = %q, want %q", got, coord.OutcomeStarted)
	}
}
