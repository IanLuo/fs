package coord

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/flagship-dev/flagship/internal/memory"
)

// git is the one program fs runs for itself, and it is bounded on purpose: one
// repository, two commands (worktree add, worktree remove), one query (status
// --porcelain), the path fs minted itself, and no --force anywhere. fs otherwise
// executes nothing a task declares; a check is the reviewer's to run and report.

// runGit runs one git command against dir and returns its trimmed output. A
// non-zero exit is an error carrying what git said, so the caller can read the
// reason without a transcript.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return text, fmt.Errorf("git %s: %s", strings.Join(args, " "), text)
	}
	return text, nil
}

// mintReviewWorktree creates the review's own checkout at the tree the developer
// reported finished, and returns the directory it made and the repository it came
// from.
//
// The directory is a sibling of the repository, named after it and the digest, so
// a human can see which round it belongs to and which repository it came from. A
// caller that named a directory gets that one instead. A review task is the only
// task this answers for: development work runs in the repository itself.
func mintReviewWorktree(rows []memory.Row, ix *memory.Index, now time.Time, review Task, want string) (string, string, error) {
	if review.ParentDev == "" {
		return "", "", fmt.Errorf("--worktree is for a review task: %s is development work, which runs in its own checkout", review.Tid)
	}
	dev, err := loadTask(rows, review.ParentDev)
	if err != nil {
		return "", "", err
	}
	if dev.ProjectPath == "" {
		return "", "", fmt.Errorf("%s records no path, so there is no repository to make the review's checkout from: add the next development task with --path", dev.Tid)
	}
	// The tree under review is the one the developer finished, read from the same
	// record the completion gate compares against: a worktree at any other commit
	// would be a review of something nobody asked about.
	digest := newestDevDoneDigest(eventsFor(ix, dev.Tid, now))
	if digest == "" {
		return "", "", fmt.Errorf("%s has no dev-done digest, so there is no tree to review: %s reports dev-done with --digest first", dev.Tid, dev.Tid)
	}

	dir, err := cleanPath(want)
	if err != nil {
		return "", "", err
	}
	if dir == "" {
		repo := filepath.Clean(dev.ProjectPath)
		dir = filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-review-"+digestSlug(digest))
	}
	if _, err := runGit(dev.ProjectPath, "worktree", "add", dir, digest); err != nil {
		return "", "", fmt.Errorf("cannot make the review's checkout at %s from %s: %w", dir, digest, err)
	}
	return dir, dev.ProjectPath, nil
}

// digestSlug is the digest as a directory name: letters and digits only, at most
// seven of them, so a short commit reads as itself and anything else still names
// a path.
func digestSlug(digest string) string {
	var b strings.Builder
	for _, r := range digest {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		}
		if b.Len() == 7 {
			break
		}
	}
	if b.Len() == 0 {
		return "tree"
	}
	return b.String()
}

// worktreeClean reports whether a worktree holds no change of its own. A dirty
// worktree is someone's work in progress, which is why it is a question and not a
// command.
func worktreeClean(dir string) (bool, error) {
	out, err := runGit(dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return out == "", nil
}

// removeWorktree deletes one checkout fs made. A directory already gone is not a
// failure: a retry must not trip over its own success.
func removeWorktree(repo, dir string) error {
	if !exists(dir) {
		return nil
	}
	_, err := runGit(repo, "worktree", "remove", dir)
	return err
}

// exists reports whether a directory is there at all.
func exists(dir string) bool {
	_, err := os.Stat(dir)
	return err == nil
}

// launchedWorktree is the launch row where fs started this session for this task
// in a directory it made itself. A hand-run reviewer, or one dispatched into a
// directory someone else created, has no such row: fs removes only what it made.
func launchedWorktree(rows []memory.Row, tid, sid string) (launchExtra, bool) {
	var best launchExtra
	var bestAt time.Time
	for _, r := range rows {
		ex, ok := decodeLaunch(r)
		if !ok || ex.Task != tid || ex.Session != sid || !ex.Worktree || ex.Dir == "" {
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
