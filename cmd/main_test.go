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
	verbs := []string{"add", "claim", "report", "done", "status"}

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

	mustOK(t, "report", "--project", "p", "--session", "s-dev", "--task", dev,
		"--state", "dev-done", "--note", "ready for review")

	// The developer cannot complete its own work.
	if code, env := invoke(t, "done", "--project", "p", "--session", "s-dev", "--task", dev); code != 1 {
		t.Fatalf("the developer completed its own task: %d %v", code, env)
	}

	review := mustOK(t, "add", "--project", "p", "--goal", "review "+dev, "--review-of", dev)
	rev, _ := review["task"].(string)
	if checks, _ := review["checks"].([]any); len(checks) != 1 || checks[0] != "go test ./..." {
		t.Fatalf("the review did not inherit the declared checks: %v", review)
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
		"--state", "pass", "--role", "review", "--command", "go test ./...", "--exit", "0")
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
