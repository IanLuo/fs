package launch

import (
	"errors"
	"strings"
	"testing"

	"github.com/flagship-dev/flagship/internal/coord"
)

// stub replaces the herdr binary with a recorder, so the test checks the argv fs
// actually builds rather than the wording of herdr's answer.
func stub(t *testing.T, answers map[string]string, fail map[string]error) *[][]string {
	t.Helper()
	var calls [][]string
	old := run
	run = func(bin string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{bin}, args...))
		key := strings.Join(args, " ")
		if err, ok := fail[key]; ok {
			return nil, err
		}
		for k, v := range answers {
			if strings.HasPrefix(key, k) {
				return []byte(v), nil
			}
		}
		return []byte("{}"), nil
	}
	t.Cleanup(func() { run = old })
	return &calls
}

// TestStartSplitsStartsThenPrompts: three calls, in that order, with the directory
// and the prompt carried through. The prompt is last because an agent that is not
// yet running cannot be told anything.
func TestStartSplitsStartsThenPrompts(t *testing.T) {
	calls := stub(t, map[string]string{
		"pane split": `{"result":{"pane":{"pane_id":"w1:p9"}}}`,
	}, nil)

	h, err := Herdr{}.Start(coord.LaunchSpec{Name: "fs-abc-120000", Kind: "pi", Dir: "/tmp/x", Prompt: "do the work"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if h.Pane != "w1:p9" || h.Name != "fs-abc-120000" || h.Dir != "/tmp/x" {
		t.Errorf("handle = %+v, want the pane, name and directory fs asked for", h)
	}
	if len(*calls) != 3 {
		t.Fatalf("made %d calls, want 3 (split, start, prompt)", len(*calls))
	}
	split := strings.Join((*calls)[0], " ")
	if !strings.Contains(split, "pane split") || !strings.Contains(split, "--cwd /tmp/x") || !strings.Contains(split, "--no-focus") {
		t.Errorf("split = %q", split)
	}
	start := strings.Join((*calls)[1], " ")
	if !strings.Contains(start, "agent start fs-abc-120000") || !strings.Contains(start, "--kind pi") || !strings.Contains(start, "--pane w1:p9") {
		t.Errorf("start = %q", start)
	}
	prompt := (*calls)[2]
	if len(prompt) < 5 || prompt[3] != "fs-abc-120000" || prompt[4] != "do the work" {
		t.Errorf("prompt call does not carry the name and the prompt: %q", prompt)
	}
}

// TestStartClosesThePaneItCouldNotStart: a pane fs opened and then failed to fill
// is fs's to clean up, or the failure leaves a shell sitting in a pane forever.
func TestStartClosesThePaneItCouldNotStart(t *testing.T) {
	calls := stub(t, map[string]string{
		"pane split": `{"result":{"pane":{"pane_id":"w1:p9"}}}`,
	}, map[string]error{
		"agent start fs-abc-120000 --kind pi --pane w1:p9": errors.New("no such kind"),
	})

	if _, err := (Herdr{}).Start(coord.LaunchSpec{Name: "fs-abc-120000", Kind: "pi", Dir: "/tmp/x", Prompt: "p"}); err == nil {
		t.Fatal("a failed agent start was not reported")
	}
	last := strings.Join((*calls)[len(*calls)-1], " ")
	if !strings.Contains(last, "pane close w1:p9") {
		t.Errorf("last call = %q, want the pane closed", last)
	}
}

// TestListReadsHerdsAnswerOnce: working is working, anything else is idle, a pane
// herdr no longer lists is absent (gone, not an error), and one call answers for
// every agent.
func TestListReadsHerdsAnswerOnce(t *testing.T) {
	stub(t, map[string]string{"agent list": `{"result":{"agents":[
		{"name":"fs-a","pane_id":"w1:p9","agent_status":"working"},
		{"name":"fs-b","pane_id":"w1:pA","agent_status":"idle"},
		{"name":"fs-c","pane_id":"w1:pB","agent_status":"done"}]}}`}, nil)

	got, err := Herdr{}.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got["w1:p9"] != coord.StatusWorking {
		t.Errorf("w1:p9 = %q, want working", got["w1:p9"])
	}
	for _, pane := range []string{"w1:pA", "w1:pB"} {
		if got[pane] != coord.StatusIdle {
			t.Errorf("%s = %q, want idle: only working is working", pane, got[pane])
		}
	}
	if _, ok := got["w1:pZ"]; ok {
		t.Error("a pane herdr never listed is not in the map")
	}
}

// TestCloseToleratesAGonePane: closing twice is not an error, so reap can run as
// often as anyone likes.
func TestCloseToleratesAGonePane(t *testing.T) {
	stub(t, nil, map[string]error{"pane close w1:p9": errors.New("pane not found")})
	if err := (Herdr{}).Close(coord.Handle{Pane: "w1:p9"}); err != nil {
		t.Errorf("closing a gone pane: %v", err)
	}
}
