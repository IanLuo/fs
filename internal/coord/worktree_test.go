package coord_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flagship-dev/flagship/internal/coord"
	"github.com/flagship-dev/flagship/internal/memory"
)

// gitEnv pins what git needs and removes what a machine could otherwise decide: one
// identity, no user or system configuration. A test that ran a developer's gitconfig
// would pass or fail for a reason that is not fs.
func gitEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "fs test")
	t.Setenv("GIT_AUTHOR_EMAIL", "fs@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "fs test")
	t.Setenv("GIT_COMMITTER_EMAIL", "fs@example.test")
}

// gitRun runs one git command and fails the test on a non-zero exit.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo makes a repository with one commit, and returns it and that commit.
func newRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("make repo dir: %v", err)
	}
	gitRun(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("one\n"), 0o644); err != nil {
		t.Fatalf("write a file: %v", err)
	}
	gitRun(t, repo, "add", "README.md")
	gitRun(t, repo, "commit", "-q", "-m", "one")
	return repo, gitRun(t, repo, "rev-parse", "HEAD")
}

// devTaskIn records a development task that names its repository.
func devTaskIn(t *testing.T, s *memory.Store, project, path string) coord.Task {
	t.Helper()
	task, err := coord.Add(s, coord.AddInput{
		Project: project, Goal: "make the thing work",
		Checks: []string{"go test ./..."}, Path: path,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	return task
}

// checkedOut counts the checkouts git knows for a repository, the main one included.
func checkedOut(t *testing.T, repo string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(gitRun(t, repo, "worktree", "list", "--porcelain"), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			n++
		}
	}
	return n
}

// reviewWorktreeRound sets a round up as far as the reviewer's checkout: the
// developer finished at the repository's one commit, and fs started the review with
// --worktree. Nothing is run or signed, so each test finishes the round its own way.
type reviewWorktreeRound struct {
	s      *memory.Store
	dev    coord.Task
	review coord.Task
	revSid string
	repo   string
	head   string
	dir    string
}

func newReviewWorktreeRound(t *testing.T) reviewWorktreeRound {
	t.Helper()
	gitEnv(t)
	repo, head := newRepo(t)
	s, _ := open(t)

	dev := devTaskIn(t, s, "p", repo)
	claim(t, s, "p", "s-dev", dev.Tid)
	review := addReview(t, s, "p", dev.Tid)
	report(t, s, coord.ReportInput{
		Project: "p", Session: "s-dev", Task: dev.Tid, State: "dev-done",
		Digest: head, Note: "ready for review",
	})

	started, err := coord.Dispatch(s, coord.DispatchInput{
		Project: "p", Task: review.Tid, NoWait: true, Worktree: true,
	}, &fakeLauncher{})
	if err != nil {
		t.Fatalf("dispatch with --worktree: %v", err)
	}
	claim(t, s, "p", started.Session, review.Tid)
	return reviewWorktreeRound{
		s: s, dev: dev, review: review, revSid: started.Session,
		repo: repo, head: head, dir: started.Dir,
	}
}

// sign runs the declared check at the tree under review and asks for completion from
// inside the reviewer's own checkout, as a dispatched reviewer does.
func (r reviewWorktreeRound) sign(t *testing.T) (coord.DoneResult, error) {
	t.Helper()
	report(t, r.s, coord.ReportInput{
		Project: "p", Session: r.revSid, Task: r.review.Tid, Role: "review",
		State: "pass", Command: r.dev.Checks[0], Exit: exit(0), Digest: r.head,
	})
	return coord.Done(r.s, coord.DoneInput{Project: "p", Session: r.revSid, Task: r.review.Tid, Dir: r.dir})
}

// closeNotes reads the note of every close row in a project, by task.
func closeNotes(t *testing.T, s *memory.Store, project string) map[string]string {
	t.Helper()
	notes := map[string]string{}
	for _, r := range rows(t, s, project) {
		if r.Kind != memory.KindEvent {
			continue
		}
		var ex map[string]any
		if err := json.Unmarshal([]byte(r.Extra), &ex); err != nil {
			t.Fatalf("unreadable event row: %v", err)
		}
		if ex["state"] == "closed" {
			notes[r.Task()], _ = ex["note"].(string)
		}
	}
	return notes
}

// TestDispatchWorktreeMakesTheReviewsCheckout is 10.2's acceptance test: --worktree
// on a review reads the developer's dev-done digest, makes the checkout at that tree,
// records that fs made it, and starts the reviewer there.
func TestDispatchWorktreeMakesTheReviewsCheckout(t *testing.T) {
	r := newReviewWorktreeRound(t)

	want := filepath.Join(filepath.Dir(r.repo), filepath.Base(r.repo)+"-review-"+r.head[:7])
	if r.dir != want {
		t.Fatalf("checkout = %q, want the sibling fs mints, %q", r.dir, want)
	}
	if info, err := os.Stat(r.dir); err != nil || !info.IsDir() {
		t.Fatalf("no checkout at %s: %v", r.dir, err)
	}
	if at := gitRun(t, r.dir, "rev-parse", "HEAD"); at != r.head {
		t.Fatalf("checkout is at %s, want the developer's digest %s", at, r.head)
	}
	if n := checkedOut(t, r.repo); n != 2 {
		t.Fatalf("git worktree list has %d entries, want the repository and the review", n)
	}

	// The launch row says fs made the directory, which is what licenses removing it.
	var row map[string]any
	for _, r := range launchRows(t, r.s, "p") {
		if err := json.Unmarshal([]byte(r.Extra), &row); err != nil {
			t.Fatalf("unreadable launch row: %v", err)
		}
	}
	if row["worktree"] != true || row["dir"] != r.dir {
		t.Fatalf("launch row = %v, want worktree true at %s", row, r.dir)
	}
}

// TestDispatchWorktreeRefusesWithoutAReviewOrATree: --worktree is for a review of a
// tree the developer finished. Either fact missing is a refusal before anything is
// created, so a refused dispatch leaves no directory.
func TestDispatchWorktreeRefusesWithoutAReviewOrATree(t *testing.T) {
	gitEnv(t)
	repo, head := newRepo(t)
	s, _ := open(t)

	dev := devTaskIn(t, s, "p", repo)
	if _, err := coord.Dispatch(s, coord.DispatchInput{
		Project: "p", Task: dev.Tid, NoWait: true, Worktree: true,
	}, &fakeLauncher{}); err == nil || !strings.Contains(err.Error(), "review task") {
		t.Fatalf("dispatch --worktree on development work = %v, want a refusal naming the review task", err)
	}
	claim(t, s, "p", "s-dev", dev.Tid)

	// A review whose developer has not reported dev-done names no tree.
	review := addReview(t, s, "p", dev.Tid)
	_, err := coord.Dispatch(s, coord.DispatchInput{
		Project: "p", Task: review.Tid, NoWait: true, Worktree: true,
	}, &fakeLauncher{})
	if err == nil || !strings.Contains(err.Error(), "dev-done digest") {
		t.Fatalf("dispatch --worktree with no dev-done = %v, want a refusal naming the missing digest", err)
	}
	if n := checkedOut(t, repo); n != 1 {
		t.Fatalf("git worktree list has %d entries after two refused dispatches, want the repository alone", n)
	}

	// Once the developer names a tree, the same dispatch is accepted.
	report(t, s, coord.ReportInput{
		Project: "p", Session: "s-dev", Task: dev.Tid, State: "dev-done", Digest: head, Note: "ready",
	})
	if _, err := coord.Dispatch(s, coord.DispatchInput{
		Project: "p", Task: review.Tid, NoWait: true, Worktree: true,
	}, &fakeLauncher{}); err != nil {
		t.Fatalf("dispatch --worktree after dev-done: %v", err)
	}
}

// TestDoneRemovesTheCleanCheckout: a close that finished in the checkout fs made
// leaves nothing behind, and the repository is back to one entry.
func TestDoneRemovesTheCleanCheckout(t *testing.T) {
	r := newReviewWorktreeRound(t)

	if _, err := r.sign(t); err != nil {
		t.Fatalf("done: %v", err)
	}
	if _, err := os.Stat(r.dir); !os.IsNotExist(err) {
		t.Fatalf("checkout %s survived a clean close (stat err = %v), want it removed", r.dir, err)
	}
	if n := checkedOut(t, r.repo); n != 1 {
		t.Fatalf("git worktree list has %d entries after the close, want the repository alone", n)
	}
	// The close still names the directory it was signed from, and says nothing about
	// a checkout that is gone.
	for tid, note := range closeNotes(t, r.s, "p") {
		if !strings.Contains(note, "signed from "+r.dir) {
			t.Fatalf("close note on %s = %q, want the signing directory", tid, note)
		}
		if strings.Contains(note, "kept") || strings.Contains(note, "left in place") {
			t.Fatalf("close note on %s = %q, want no leftover named when the checkout was removed", tid, note)
		}
	}
}

// TestDoneKeepsADirtyCheckout: a checkout with uncommitted work is someone's work.
// It stays on disk, nothing is forced, and the close names it.
func TestDoneKeepsADirtyCheckout(t *testing.T) {
	r := newReviewWorktreeRound(t)
	if err := os.WriteFile(filepath.Join(r.dir, "dirt.txt"), []byte("half a change\n"), 0o644); err != nil {
		t.Fatalf("leave a change in the checkout: %v", err)
	}

	if _, err := r.sign(t); err != nil {
		t.Fatalf("done refused a round whose checkout is dirty: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "dirt.txt")); err != nil {
		t.Fatalf("the checkout fs did not make clean was still changed: %v", err)
	}
	if n := checkedOut(t, r.repo); n != 2 {
		t.Fatalf("git worktree list has %d entries, want the dirty checkout kept", n)
	}
	notes := closeNotes(t, r.s, "p")
	if len(notes) != 2 {
		t.Fatalf("close notes = %v, want one per closed task", notes)
	}
	for tid, note := range notes {
		if !strings.Contains(note, r.dir) || !strings.Contains(note, "uncommitted") {
			t.Fatalf("close note on %s = %q, want the kept checkout %s and its reason", tid, note, r.dir)
		}
	}
}

// TestDoneRefusalLeavesTheCheckout: a refused round may continue in the same
// checkout, so nothing is removed.
func TestDoneRefusalLeavesTheCheckout(t *testing.T) {
	r := newReviewWorktreeRound(t)

	if _, err := coord.Done(r.s, coord.DoneInput{
		Project: "p", Session: r.revSid, Task: r.review.Tid, Dir: r.dir,
	}); err == nil {
		t.Fatal("done passed with no check run, want the gate to refuse")
	}
	if _, err := os.Stat(r.dir); err != nil {
		t.Fatalf("checkout %s is gone after a refusal: %v", r.dir, err)
	}
	if n := checkedOut(t, r.repo); n != 2 {
		t.Fatalf("git worktree list has %d entries after a refusal, want the checkout kept", n)
	}
}

// TestDoneNeverRemovesADirectoryFsDidNotMake: the launch row is the licence. A review
// dispatched into a directory someone else created closes without touching it.
func TestDoneNeverRemovesADirectoryFsDidNotMake(t *testing.T) {
	gitEnv(t)
	repo, head := newRepo(t)
	s, _ := open(t)
	dev := devTaskIn(t, s, "p", repo)
	claim(t, s, "p", "s-dev", dev.Tid)
	review := addReview(t, s, "p", dev.Tid)
	report(t, s, coord.ReportInput{
		Project: "p", Session: "s-dev", Task: dev.Tid, State: "dev-done", Digest: head, Note: "ready",
	})

	mine := t.TempDir() // a checkout fs did not make: no launch row says worktree
	started, err := coord.Dispatch(s, coord.DispatchInput{
		Project: "p", Task: review.Tid, NoWait: true, Dir: mine,
	}, &fakeLauncher{})
	if err != nil {
		t.Fatalf("dispatch into an existing directory: %v", err)
	}
	claim(t, s, "p", started.Session, review.Tid)
	report(t, s, coord.ReportInput{
		Project: "p", Session: started.Session, Task: review.Tid, Role: "review",
		State: "pass", Command: dev.Checks[0], Exit: exit(0), Digest: head,
	})
	if _, err := coord.Done(s, coord.DoneInput{
		Project: "p", Session: started.Session, Task: review.Tid, Dir: t.TempDir(),
	}); err != nil {
		t.Fatalf("done: %v", err)
	}
	if info, err := os.Stat(mine); err != nil || !info.IsDir() {
		t.Fatalf("fs removed a directory it did not make: %v", err)
	}
}
