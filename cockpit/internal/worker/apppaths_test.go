package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSessionWorkerPathsCoverTheTokensAndTheConsentFile: what no app session
// may touch is the worker's own -- the directory of worker.yaml and
// workers.yaml (the worker tokens) and policy.yaml (apps.allow, the app
// consent gate), its state, a --token-file wherever it lives, and the consent
// socket. Absolute, each once, whatever the flags were spelled as.
func TestSessionWorkerPathsCoverTheTokensAndTheConsentFile(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".memql")
	got := sessionWorkerPaths(
		filepath.Join(dir, "worker.yaml"),
		filepath.Join(dir, "workers.yaml"),
		[]string{filepath.Join(dir, "state"), filepath.Join(dir, "state"), ""},
		filepath.Join(home, "secure", "token"),
		filepath.Join(home, "run", "worker.sock"),
	)
	want := []string{
		dir,
		filepath.Join(dir, "state"),
		filepath.Join(home, "secure", "token"),
		filepath.Join(home, "run", "worker.sock"),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("sessionWorkerPaths = %q, want %q", got, want)
	}
}

// TestSessionWorkerPathsAreAbsolute: a relative --config is read relative to
// where the worker started, so that is where its directory is -- and the app
// is handed the path as an absolute one it can deny.
func TestSessionWorkerPathsAreAbsolute(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	got := sessionWorkerPaths("conf/worker.yaml", "", nil, "", "")
	if len(got) != 1 || got[0] != filepath.Join(cwd, "conf") {
		t.Errorf("sessionWorkerPaths = %q, want the absolute %q", got, filepath.Join(cwd, "conf"))
	}
}
