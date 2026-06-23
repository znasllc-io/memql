package scan

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestNoEnvRegistryDrift is the CI drift gate for the env-var registry
// (7.2 / memql#2105). It runs the shared classifier over the repo root
// and fails on drift in EITHER direction:
//
//   - forward  -- a var read in code but missing from
//     scripts/secrets/manifest.yaml.
//   - reverse  -- a registry entry that appears nowhere in the repo.
//
// Because the CI go-checks job runs `GOWORK=off go test ./...`, this
// test IS the gate automatically -- a drift in either direction turns
// the lane red.
func TestNoEnvRegistryDrift(t *testing.T) {
	root := repoRoot(t)

	res, err := CheckDrift(root)
	if err != nil {
		t.Fatalf("drift check failed: %v", err)
	}

	for _, k := range res.Unregistered {
		t.Errorf("forward drift: env var %q is read in code but NOT registered in scripts/secrets/manifest.yaml", k)
	}
	for _, k := range res.Stale {
		t.Errorf("reverse drift: registry entry %q appears nowhere in the repo (stale registration)", k)
	}

	if res.OK() {
		t.Logf("env registry clean: %d reads, %d registry entries, no drift", res.ReadCount, res.RegistrySize)
	}
}

// repoRoot walks up from this test file's directory until it finds the
// dir containing go.mod -- robust to the working directory `go test`
// happens to run in.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate go.mod walking up from %s", filepath.Dir(file))
		}
		dir = parent
	}
}
