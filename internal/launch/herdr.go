// Package launch is fs's only contact with processes. It starts one agent in one
// pane, asks whether that agent is still working, and closes the pane again.
//
// It is deliberately one file and one implementation. fs is not a process
// supervisor: there is no retry, no restart, no supervision loop, and no fallback
// launcher. A launcher that fails is reported, because retrying a broken launcher
// is how a single instruction becomes a room full of agents.
package launch

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/flagship-dev/flagship/internal/coord"
)

// Herdr starts agents in herdr panes. It is the only launcher fs ships.
type Herdr struct {
	// Bin is the herdr binary; empty means `herdr` from PATH.
	Bin string
}

// run is the seam the tests replace. It returns stdout.
var run = func(bin string, args ...string) ([]byte, error) {
	cmd := exec.Command(bin, args...) // #nosec G204 -- bin is fs's own launcher, args are fs's own words
	return cmd.Output()
}

func (h Herdr) bin() string {
	if h.Bin != "" {
		return h.Bin
	}
	return "herdr"
}

// Start splits a pane, starts the agent in it, and hands it the prompt.
//
// The prompt is delivered last and its failure is not fatal: herdr reports
// `agent_prompt_stalled` when an agent has not visibly started within a few
// seconds, and that is a hint, not a verdict — whether the agent worked is decided
// in fs, by whether the task moved. Killing the pane here would discard a
// reviewer that was merely slow to draw its first frame.
func (h Herdr) Start(spec coord.LaunchSpec) (coord.Handle, error) {
	bin := h.bin()
	out, err := run(bin, "pane", "split", "--current", "--direction", "down", "--cwd", spec.Dir, "--no-focus")
	if err != nil {
		return coord.Handle{}, fmt.Errorf("herdr pane split: %w", err)
	}
	var split struct {
		Result struct {
			Pane struct {
				PaneID string `json:"pane_id"`
			} `json:"pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &split); err != nil {
		return coord.Handle{}, fmt.Errorf("herdr pane split: unreadable answer: %w", err)
	}
	pane := split.Result.Pane.PaneID
	if pane == "" {
		return coord.Handle{}, fmt.Errorf("herdr pane split returned no pane")
	}
	handle := coord.Handle{Pane: pane, Name: spec.Name, Dir: spec.Dir}

	if _, err := run(bin, "agent", "start", spec.Name, "--kind", spec.Kind, "--pane", pane); err != nil {
		_ = h.Close(handle) // do not leave a pane holding a bare shell
		return coord.Handle{}, fmt.Errorf("herdr agent start: %w", err)
	}
	// A prompt that herdr reports as failed is not treated as fatal, and the pane
	// is not closed. herdr exits non-zero when the agent has not visibly started
	// within the timeout (`agent_prompt_stalled`), which a real agent almost always
	// is — and an agent that truly never read the prompt never claims, so `reap`
	// ends it once the round's lease lapses. Deciding here would kill reviewers
	// that were merely slow to draw.
	_, _ = run(bin, "agent", "prompt", spec.Name, spec.Prompt, "--wait", "--timeout", "30000")
	return handle, nil
}

// Status reports whether the agent is still working. An agent herdr no longer
// lists is gone; that is not an error, it is the answer.
func (h Herdr) Status(handle coord.Handle) (string, error) {
	out, err := run(h.bin(), "agent", "list")
	if err != nil {
		return "", fmt.Errorf("herdr agent list: %w", err)
	}
	var list struct {
		Result struct {
			Agents []struct {
				Name        string `json:"name"`
				PaneID      string `json:"pane_id"`
				AgentStatus string `json:"agent_status"`
			} `json:"agents"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return "", fmt.Errorf("herdr agent list: unreadable answer: %w", err)
	}
	for _, a := range list.Result.Agents {
		match := handle.Name != "" && a.Name == handle.Name
		if !match && handle.Pane != "" && a.PaneID == handle.Pane {
			match = true
		}
		if !match {
			continue
		}
		if strings.EqualFold(a.AgentStatus, coord.StatusWorking) {
			return coord.StatusWorking, nil
		}
		return coord.StatusIdle, nil
	}
	return coord.StatusGone, nil
}

// Close closes the pane. A pane that is already gone is not an error: closing is
// idempotent, which is what lets reap run as often as anyone likes.
func (h Herdr) Close(handle coord.Handle) error {
	if handle.Pane == "" {
		return nil
	}
	if _, err := run(h.bin(), "pane", "close", handle.Pane); err != nil {
		if isGone(err) {
			return nil
		}
		return fmt.Errorf("herdr pane close %s: %w", handle.Pane, err)
	}
	return nil
}

// isGone recognises the "no such pane" family without parsing herdr's wording
// more finely than needed: a close that fails because the pane is absent has
// already achieved what it was asked to do.
func isGone(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "no such") || strings.Contains(msg, "unknown pane")
}
