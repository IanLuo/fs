package coord

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/flagship-dev/flagship/internal/memory"
)

// pathAttempts bounds the append loop. A retry exists for one reason: two writers
// computed the same index, and a fresher read shows the row that won.
const pathAttempts = 3

// PathInput is one project-level directory binding: where this project's work runs
// from now on.
type PathInput struct {
	Project string
	Path    string
}

// PathBinding is one binding as a reader sees it.
type PathBinding struct {
	Project string     `json:"project"`
	Path    string     `json:"path"`
	At      *time.Time `json:"at,omitempty"`
}

// pathExtra is a path row's extra: which project, and the directory it names.
type pathExtra struct {
	Project string `json:"project"`
	Path    string `json:"path"`
}

// SetPath records that a project's work runs in dir, by appending one row. Nothing
// is updated, because a row is a fact and a new fact is a new row: a repository
// that moved is one command, and every task row it already has keeps its own
// recorded path untouched.
//
// The row is project-level, in the project's own scope, so dispatch and status can
// read it with the listing they already take.
func SetPath(s *memory.Store, in PathInput) (PathBinding, error) {
	if err := memory.ValidProject(in.Project); err != nil {
		return PathBinding{}, err
	}
	if strings.TrimSpace(in.Path) == "" {
		return PathBinding{}, errors.New("no path: --path is required")
	}
	// Absolute at write time, by the same rule `add --path` follows: a relative
	// path records the directory the writer happened to stand in, which is not a
	// fact a later reader can use.
	dir, err := cleanPath(in.Path)
	if err != nil {
		return PathBinding{}, err
	}
	scope := memory.Scope(in.Project)
	extra, err := json.Marshal(pathExtra{Project: in.Project, Path: dir})
	if err != nil {
		return PathBinding{}, err
	}
	text := fmt.Sprintf("project %s runs in %s", in.Project, dir)

	// Appended, never updated: the next free index is the next row. Two writers
	// that compute the same index are separated by memstore's refusal of the
	// duplicate key, so the loser re-reads rather than overwriting a winner.
	for attempt := 0; attempt < pathAttempts; attempt++ {
		rows, err := s.Rows(scope)
		if err != nil {
			return PathBinding{}, err
		}
		now := s.Now()
		if err := s.PutPath(scope, in.Project, maxPathIndex(rows)+1, text, string(extra)); err != nil {
			if errors.Is(err, memory.ErrDuplicate) {
				continue
			}
			return PathBinding{}, err
		}
		at := now
		return PathBinding{Project: in.Project, Path: dir, At: &at}, nil
	}
	return PathBinding{}, fmt.Errorf("project %s: lost three times to another writer; the path was not recorded", in.Project)
}

// maxPathIndex is the highest binding index a project has, or 0 when it has none.
func maxPathIndex(rows []memory.Row) int {
	best := 0
	for _, r := range rows {
		if n, ok := memory.ParsePathIndex(r.OriginID); ok && n > best {
			best = n
		}
	}
	return best
}

// projectDir is where a project dispatches into: its newest binding, or "" when the
// project never recorded one. Newest is the row's timestamp and then its append
// index, so two bindings written under one clock reading still have an order and
// the answer never depends on how the listing came back.
func projectDir(rows []memory.Row) string {
	var (
		best  pathExtra
		at    time.Time
		index int
		found bool
	)
	for _, r := range rows {
		if r.Kind != memory.KindPath {
			continue
		}
		var ex pathExtra
		if err := json.Unmarshal([]byte(r.Extra), &ex); err != nil || strings.TrimSpace(ex.Path) == "" {
			continue
		}
		n, _ := memory.ParsePathIndex(r.OriginID)
		if found && !newerPath(r.Ts, n, at, index) {
			continue
		}
		best, at, index, found = ex, r.Ts, n, true
	}
	return best.Path
}

// newerPath orders two bindings by when they were written, then by the index that
// appends them.
func newerPath(at1 time.Time, n1 int, at2 time.Time, n2 int) bool {
	if !at1.Equal(at2) {
		return at1.After(at2)
	}
	return n1 > n2
}

// resolveDir names where an agent works, and refuses a directory that is not one.
//
// The order is the user's, and each step exists for a reason:
//
//  1. --dir, a one-off: this run, this directory.
//  2. the project's newest `fs path` binding: the repository moved, and one
//     command moved every task with it.
//  3. the task's own recorded path: where the work was written down.
//
// A refusal here is the point of the order. A dead path used to reach exec and
// come back as "fork/exec <shell>: no such file or directory", which names neither
// the task nor the directory the reader must fix.
func resolveDir(flagDir string, rows []memory.Row, task Task, project string) (string, error) {
	dir := strings.TrimSpace(flagDir)
	if dir == "" {
		dir = projectDir(rows)
	}
	if dir == "" {
		dir = task.ProjectPath
	}
	if dir == "" {
		return "", fmt.Errorf("%s records no path; pass --dir or run fs path --project %s --path DIR", task.Tid, project)
	}
	abs, err := cleanPath(dir)
	if err != nil {
		return "", err
	}
	// Absolute before the check: whether a directory exists is a question about
	// the user's path, and the launcher would resolve a relative one against its
	// own working directory.
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s: %s is not an existing directory; run fs path --project %s --path DIR, or pass --dir",
			task.Tid, abs, project)
	}
	return abs, nil
}
