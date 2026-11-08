package launch_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestCoordDoesNotImportTheLauncher is the seam, mechanized.
//
// The claim protocol and the completion gate are correct only while they know
// nothing about processes. internal/coord says what to start and why a round is
// over; internal/launch is the one place that knows how a process is started.
// A coord that imported launch would be able to start an agent from the middle of
// the gate, which is how a refusal becomes a side effect.
func TestCoordDoesNotImportTheLauncher(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	root := filepath.Join(filepath.Dir(here), "..", "..")
	coord := filepath.Join(root, "internal", "coord")

	entries, err := os.ReadDir(coord)
	if err != nil {
		t.Fatalf("read internal/coord: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(coord, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if strings.Contains(string(src), `"github.com/flagship-dev/flagship/internal/launch"`) {
			t.Errorf("internal/coord/%s imports internal/launch: the protocol names what to start, it does not start it", e.Name())
		}
	}
}
