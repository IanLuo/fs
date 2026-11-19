package coord_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flagship-dev/flagship/internal/coord"
	"github.com/flagship-dev/flagship/internal/memory"
)

// setPath records a project-level directory binding and fails the test if it is
// refused.
func setPath(t *testing.T, s *memory.Store, project, dir string) coord.PathBinding {
	t.Helper()
	got, err := coord.SetPath(s, coord.PathInput{Project: project, Path: dir})
	if err != nil {
		t.Fatalf("path %s: %v", dir, err)
	}
	return got
}

// TestSetPathAppendsAnAbsoluteBinding: the binding is a row in the project's own
// scope, holding the directory absolutely — a relative path means whatever
// directory the writer stood in, which is not a fact a later reader can use.
func TestSetPathAppendsAnAbsoluteBinding(t *testing.T) {
	s, _ := open(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	got := setPath(t, s, "p", ".")
	if got.Project != "p" || got.Path != wd {
		t.Fatalf("binding = %+v, want p in %s", got, wd)
	}
	var pathRows []memory.Row
	for _, r := range rows(t, s, "p") {
		if r.Kind == memory.KindPath {
			pathRows = append(pathRows, r)
		}
	}
	if len(pathRows) != 1 {
		t.Fatalf("path rows = %d, want 1", len(pathRows))
	}
	if want := memory.PathKey("p", 1); pathRows[0].OriginID != want {
		t.Errorf("path key = %q, want %q", pathRows[0].OriginID, want)
	}
	if pathRows[0].Scope != memory.Scope("p") {
		t.Errorf("path row scope = %q, want the project's own scope", pathRows[0].Scope)
	}
	if !strings.Contains(pathRows[0].Extra, wd) {
		t.Errorf("path row does not carry the directory: %s", pathRows[0].Extra)
	}
	if _, err := coord.SetPath(s, coord.PathInput{Project: "p"}); err == nil {
		t.Error("recorded a binding with no path")
	}
	if _, err := coord.SetPath(s, coord.PathInput{Path: wd}); err == nil {
		t.Error("recorded a binding with no project")
	}
}

// TestDispatchFollowsTheNewestBinding: a repository that moved is one command.
// Nothing about the task changes — its row still records where it was written
// down — and the next dispatch starts in the project's newest directory.
func TestDispatchFollowsTheNewestBinding(t *testing.T) {
	s, now := open(t)
	old, moved := t.TempDir(), t.TempDir()
	task, err := coord.Add(s, coord.AddInput{
		Project: "p", Goal: "g", Checks: []string{"true"}, Path: old,
	})
	if err != nil {
		t.Fatal(err)
	}

	setPath(t, s, "p", moved)
	fake := &fakeLauncher{}
	got, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid}, fake)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got.Dir != moved {
		t.Errorf("dispatched into %q, want the project binding %q", got.Dir, moved)
	}
	if len(fake.started) != 1 || fake.started[0].Dir != moved {
		t.Errorf("launcher was given %+v, want the binding", fake.started)
	}
	if !strings.Contains(fake.started[0].Prompt, moved) {
		t.Errorf("prompt does not name the binding:\n%s", fake.started[0].Prompt)
	}

	// The task row is untouched: still one task row, still its own recorded path.
	if n := countKind(rows(t, s, "p"), memory.KindTask); n != 1 {
		t.Errorf("task rows = %d, want the one that was already there", n)
	}
	rep, err := coord.Status(s, coord.StatusInput{Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Projects[0].Tasks[0].ProjectPath; got != old {
		t.Errorf("task path = %q, want the recorded %q: a move touches no task row", got, old)
	}
	if got := rep.Projects[0].Dir; got != moved {
		t.Errorf("status dir = %q, want the newest binding %q", got, moved)
	}

	// A newer binding wins over the older one, without a second task row.
	*now = now.Add(time.Second) // a second dispatch is a second session id
	newest := t.TempDir()
	setPath(t, s, "p", newest)
	again := &fakeLauncher{}
	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid}, again); err != nil {
		t.Fatalf("dispatch after the second move: %v", err)
	}
	if again.started[0].Dir != newest {
		t.Errorf("dispatched into %q, want the newest binding %q", again.started[0].Dir, newest)
	}
}

// TestDispatchOrderIsFlagThenBindingThenTask: the order is the user's. A one-off
// --dir is this run and nothing else; a project binding beats the task's own path;
// the task's path is the last resort.
func TestDispatchOrderIsFlagThenBindingThenTask(t *testing.T) {
	s, now := open(t)
	taskDir, binding, oneOff := t.TempDir(), t.TempDir(), t.TempDir()

	byTask, err := coord.Add(s, coord.AddInput{
		Project: "p", Goal: "g", Checks: []string{"true"}, Path: taskDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeLauncher{}
	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: byTask.Tid}, fake); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if fake.started[0].Dir != taskDir {
		t.Errorf("dispatched into %q, want the task's path %q when no binding exists", fake.started[0].Dir, taskDir)
	}

	setPath(t, s, "p", binding)
	*now = now.Add(time.Second)
	overBinding := &fakeLauncher{}
	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: byTask.Tid, Dir: oneOff}, overBinding); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if overBinding.started[0].Dir != oneOff {
		t.Errorf("dispatched into %q, want --dir %q", overBinding.started[0].Dir, oneOff)
	}
}

// TestDispatchRefusesADeadPathBeforeStarting: a directory that is not there is a
// refusal, named once, before any process exists. The old path used to reach exec
// and come back as "fork/exec <shell>: no such file or directory", which names
// neither the task nor the directory to fix.
func TestDispatchRefusesADeadPathBeforeStarting(t *testing.T) {
	s, _ := open(t)
	dead := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(dead, 0o755); err != nil {
		t.Fatal(err)
	}
	task, err := coord.Add(s, coord.AddInput{
		Project: "p", Goal: "g", Checks: []string{"true"}, Path: dead,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dead); err != nil {
		t.Fatal(err)
	}

	fake := &fakeLauncher{}
	_, err = coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid}, fake)
	if err == nil {
		t.Fatal("dispatched into a directory that does not exist")
	}
	msg := err.Error()
	if !strings.Contains(msg, task.Tid) || !strings.Contains(msg, dead) {
		t.Errorf("refusal = %q, want the task and the path named", msg)
	}
	if strings.Contains(msg, "\n") {
		t.Errorf("refusal is not one line: %q", msg)
	}
	if len(fake.started) != 0 {
		t.Error("a pane was started for a dead path")
	}
	if n := countKind(rows(t, s, "p"), memory.KindLaunch); n != 0 {
		t.Errorf("launch rows = %d, want none", n)
	}

	// A file is not a directory either.
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid, Dir: file}, fake); err == nil {
		t.Error("dispatched into a file")
	}
	if len(fake.started) != 0 {
		t.Error("a pane was started for a file")
	}
}

// TestDispatchDoesNotFallThroughADeadBinding: the binding is the project's answer.
// A binding that names a directory which is gone refuses rather than quietly
// dispatching into a task's older path, so a reader never works in a checkout the
// project just said it left.
func TestDispatchDoesNotFallThroughADeadBinding(t *testing.T) {
	s, _ := open(t)
	taskDir := t.TempDir()
	task, err := coord.Add(s, coord.AddInput{
		Project: "p", Goal: "g", Checks: []string{"true"}, Path: taskDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(t.TempDir(), "moved-away")
	setPath(t, s, "p", dead)

	fake := &fakeLauncher{}
	if _, err := coord.Dispatch(s, coord.DispatchInput{NoWait: true, Project: "p", Task: task.Tid}, fake); err == nil {
		t.Fatal("dispatched past a binding whose directory is gone")
	} else if !strings.Contains(err.Error(), dead) {
		t.Errorf("refusal = %v, want the binding's path named", err)
	}
	if len(fake.started) != 0 {
		t.Errorf("started %d agents for a dead binding, want 0", len(fake.started))
	}
}

// TestStatusReportsWhereTheProjectRuns: the answer is read from the rows on every
// call, so a reader never sees a stale directory, and a project whose only row is a
// binding still says where it would dispatch.
func TestStatusReportsWhereTheProjectRuns(t *testing.T) {
	s, _ := open(t)
	dir := t.TempDir()
	setPath(t, s, "p", dir)
	add(t, s, "p")

	rep, err := coord.Status(s, coord.StatusInput{Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Projects[0].Dir; got != dir {
		t.Errorf("status dir = %q, want %q", got, dir)
	}

	// A project with a binding and no task row still names its directory.
	bare := t.TempDir()
	setPath(t, s, "empty", bare)
	rep, err = coord.Status(s, coord.StatusInput{Project: "empty"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Projects) != 1 || len(rep.Projects[0].Tasks) != 0 || rep.Projects[0].Dir != bare {
		t.Errorf("a binding-only project = %+v, want its directory and no tasks", rep.Projects)
	}
}
