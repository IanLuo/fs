// Package scripts tests the commands fs ships as scripts. It has no non-test Go
// code: the package exists so `go test ./...` — the check a round declares — runs
// the tests of the tools beside fs, not only of fs itself.
package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// script is the file under test, resolved once from the package directory: the test
// runs in scripts/, and the reviewer runs it the same way.
func scriptPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("changed-needs-test.sh")
	if err != nil {
		t.Fatalf("resolve the script: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("no script at %s: %v", abs, err)
	}
	return abs
}

// gitEnv pins what git needs and removes what a machine could otherwise decide:
// one identity, no user or system configuration.
func gitEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "fs test")
	t.Setenv("GIT_AUTHOR_EMAIL", "fs@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "fs test")
	t.Setenv("GIT_COMMITTER_EMAIL", "fs@example.test")
}

// git runs one git command and fails the test on a non-zero exit.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repo makes a repository with one commit and returns it with that commit's id.
func repo(t *testing.T) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make repo dir: %v", err)
	}
	git(t, dir, "init", "-q")
	write(t, dir, "a.go", "package a\n")
	git(t, dir, "add", "a.go")
	git(t, dir, "commit", "-q", "-m", "one")
	return dir, git(t, dir, "rev-parse", "HEAD")
}

// write puts one file in the repository.
func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// commit records the given files in one commit.
func commit(t *testing.T, dir, msg string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		write(t, dir, name, body)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", msg)
}

// ask runs the script in a repository and returns its exit code and its two streams.
func ask(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{scriptPath(t)}, args...)...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stdout.String(), stderr.String()
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), stdout.String(), stderr.String()
	}
	t.Fatalf("run the script: %v", err)
	return 0, "", ""
}

// oneLine asserts the answer is exactly one line, which is what a reviewer pastes
// into the tests slot's --note.
func oneLine(t *testing.T, out string) string {
	t.Helper()
	line := strings.TrimSuffix(out, "\n")
	if line == "" || strings.Contains(line, "\n") {
		t.Fatalf("answer = %q, want exactly one line", out)
	}
	return line
}

// TestCodeChangedWithoutATestFails is the rule's acceptance test: behaviour changed
// and no test moved with it. The line names the file and says FAIL.
func TestCodeChangedWithoutATestFails(t *testing.T) {
	gitEnv(t)
	dir, parent := repo(t)
	commit(t, dir, "behaviour", map[string]string{"a.go": "package a\n\nfunc F() {}\n"})

	code, out, _ := ask(t, dir, parent)
	if code != 1 {
		t.Fatalf("exit %d, want 1 for code changed with no test: %s", code, out)
	}
	line := oneLine(t, out)
	for _, want := range []string{"FAIL", "a.go", "non-test"} {
		if !strings.Contains(line, want) {
			t.Fatalf("answer %q does not name %q", line, want)
		}
	}
}

// TestACodeChangeWithATestPasses: a test file changed with the code is the answer
// the rule asks for, whatever else changed.
func TestACodeChangeWithATestPasses(t *testing.T) {
	gitEnv(t)
	dir, parent := repo(t)
	commit(t, dir, "both", map[string]string{
		"a.go":      "package a\n\nfunc F() {}\n",
		"a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {}\n",
	})

	code, out, _ := ask(t, dir, parent)
	if code != 0 {
		t.Fatalf("exit %d, want 0 for a test moving with the code: %s", code, out)
	}
	if line := oneLine(t, out); !strings.Contains(line, "PASS") || !strings.Contains(line, "a_test.go") {
		t.Fatalf("answer %q does not say a test moved with the code", line)
	}
}

// TestOnlyDocsConfigOrTestsPass: a change that alters no non-test code is not a
// change that needs a test, so the question is answered without failing the round.
func TestOnlyDocsConfigOrTestsPass(t *testing.T) {
	cases := map[string]map[string]string{
		"docs":    {"README.md": "two\n"},
		"config":  {"flake.nix": "{}\n"},
		"tests":   {"a_test.go": "package a\n"},
		"nothing": {},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			gitEnv(t)
			dir, parent := repo(t)
			if len(files) > 0 {
				commit(t, dir, name, files)
			}

			code, out, _ := ask(t, dir, parent)
			if code != 0 {
				t.Fatalf("exit %d, want 0 for a %s-only change: %s", code, name, out)
			}
			oneLine(t, out)
		})
	}
}

// TestAnUnaskableQuestionExitsTwo: a missing argument or an unknown commit is not a
// pass. It is refused, so a reviewer cannot record a green answer it never got.
func TestAnUnaskableQuestionExitsTwo(t *testing.T) {
	gitEnv(t)
	dir, _ := repo(t)

	if code, out, errOut := ask(t, dir); code != 2 || !strings.Contains(errOut, "usage") {
		t.Fatalf("no argument: exit %d, want 2 with a usage line: %s%s", code, out, errOut)
	}
	if code, _, errOut := ask(t, dir, "nosuchcommit"); code != 2 || !strings.Contains(errOut, "no commit") {
		t.Fatalf("unknown commit: exit %d, want 2 naming the commit: %s", code, errOut)
	}
	// Nothing changed between a commit and itself: answered, not refused.
	head := git(t, dir, "rev-parse", "HEAD")
	if code, out, _ := ask(t, dir, head); code != 0 {
		t.Fatalf("an empty diff was refused: exit %d: %s", code, out)
	}
}
