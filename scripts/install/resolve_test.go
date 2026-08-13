// Package install holds tests for the local-install capability scripts.
package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// resolveHarness runs a snippet with scripts/lib/resolve.sh sourced.
func resolveHarness(t *testing.T, stubDir, snippet string) string {
	t.Helper()
	lib, err := filepath.Abs(filepath.Join("..", "lib", "resolve.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", "set -euo pipefail; source '"+lib+"'; "+snippet)
	cmd.Env = append(os.Environ(), "MEMQL_RESOLVE_STUB="+stubDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	return string(out)
}

func TestResolveStubReturnsAddresses(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cockpit.memql.localhost"),
		[]byte("127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := strings.TrimSpace(resolveHarness(t, dir, `resolve_addresses cockpit.memql.localhost`))
	if got != "127.0.0.1" {
		t.Errorf("resolve_addresses = %q, want 127.0.0.1", got)
	}
}

// "Did not resolve" is an empty result, not an error. Every caller runs under
// `set -e`, so a non-zero status here would abort the probe instead of being
// one of its three outcomes.
func TestResolveStubUnknownHostIsEmptyAndSucceeds(t *testing.T) {
	got := strings.TrimSpace(resolveHarness(t, t.TempDir(), `resolve_addresses nothing.invalid; echo "rc=$?"`))
	if got != "rc=0" {
		t.Errorf("resolve_addresses output = %q, want just rc=0 for an unknown host", got)
	}
}

// Multiple addresses come back one per line, which is what lets the probe
// refuse a name that answers both 127.0.0.1 and something else.
func TestResolveStubReturnsEveryAddress(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "split.example.com"),
		[]byte("127.0.0.1\n203.0.113.7\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := strings.Fields(resolveHarness(t, dir, `resolve_addresses split.example.com`))
	if len(got) != 2 {
		t.Fatalf("resolve_addresses = %v, want two addresses", got)
	}
}
