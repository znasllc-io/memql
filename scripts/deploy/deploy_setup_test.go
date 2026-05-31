// Package deploy holds tests for the Azure deploy bootstrap shell
// script (.claude/scripts/deploy-setup.sh). The script itself is
// function-based bash per the Skills+Scripts architecture (#492);
// these tests exercise its dry-run contract from `go test ./...` so
// CI catches regressions without needing a live Azure subscription.
//
// There is no Go production code here -- this directory exists purely
// as a test harness for the shell bootstrap, mirroring the existing
// scripts/<area>/*_test.go pattern (e.g. scripts/dedupe-peruser-seeds).
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// scriptPath resolves .claude/scripts/deploy-setup.sh relative to the
// repo root. This test file lives at scripts/deploy/, so the root is
// two directories up.
func scriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	p := filepath.Join(root, ".claude", "scripts", "deploy-setup.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("deploy-setup.sh not found at %s: %v", p, err)
	}
	return p
}

// run executes the script with the given args and returns combined
// output + the error (nil on exit 0).
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell bootstrap test")
	}
	cmd := exec.Command("bash", append([]string{scriptPath(t)}, args...)...)
	// Force a clean env w.r.t. the secret vars so the dry-run plan is
	// deterministic regardless of the developer's shell.
	cmd.Env = append(os.Environ(), "ENV=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestSyntax fails fast if the script has a bash syntax error.
// Equivalent to `bash -n` but wired into `go test`.
func TestSyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out, err := exec.Command("bash", "-n", scriptPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %v\n%s", err, out)
	}
}

// TestHelp checks that --help exits 0 and prints usage.
func TestHelp(t *testing.T) {
	out, err := run(t, "--help")
	if err != nil {
		t.Fatalf("--help exited non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Usage:") {
		t.Errorf("--help missing 'Usage:' banner:\n%s", out)
	}
	for _, want := range []string{"--dry-run", "--env=", "deploy-setup.sh"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help output missing %q", want)
		}
	}
}

// TestDryRunStagingPlan asserts the staging dry-run exits 0, mutates
// nothing (every Azure call is a [plan] line, never an executed az
// command), names the four core resources with the right per-env
// suffixes + shared ACR, and plans exactly the THREE A2 genesis-envelope
// Key Vault secrets (DSN + master key + sealed envelope base64).
func TestDryRunStagingPlan(t *testing.T) {
	out, err := run(t, "--env=staging", "--dry-run")
	if err != nil {
		t.Fatalf("staging dry-run exited non-zero: %v\n%s", err, out)
	}

	// Side-effect-free guarantee: in dry-run every mutating az call
	// must be prefixed with the plan marker. If we ever see a bare
	// "az group create" without the "[plan]" marker on the same line,
	// the run-gate leaked.
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "az ") {
			t.Errorf("dry-run appears to execute an az command (no [plan] marker): %q", trimmed)
		}
	}

	mustContain := []string{
		"rg-memql-staging",
		"kv-memql-staging",
		"cae-memql-staging",
		"acrmemql", // shared registry, env-independent name
		// A2 genesis-envelope model: exactly three Key Vault secrets.
		"memory-nodes-database-dsn",
		"memql-master-key",
		"memql-genesis-b64",
		"DRY RUN complete",
		"State report",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("staging dry-run output missing %q\n---\n%s", want, out)
		}
	}

	// The old "6 individual secrets" model is gone: none of the stale
	// per-secret names should appear anywhere in the plan now (they live
	// inside the sealed genesis envelope).
	for _, gone := range []string{
		"memql-si-openai",
		"contentid-salt",
		"discord",
	} {
		if strings.Contains(out, gone) {
			t.Errorf("staging dry-run still references retired secret %q (should be inside the envelope now)\n---\n%s", gone, out)
		}
	}
}

// TestDryRunProductionParameterized proves the production path is
// parameterized (not hardcoded to staging): resource names carry the
// production suffix, and the production stub warning fires.
func TestDryRunProductionParameterized(t *testing.T) {
	out, err := run(t, "--env=production", "--dry-run")
	if err != nil {
		t.Fatalf("production dry-run exited non-zero: %v\n%s", err, out)
	}
	for _, want := range []string{
		"rg-memql-production",
		"kv-memql-production",
		"cae-memql-production",
		"parameterized STUB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("production dry-run output missing %q\n---\n%s", want, out)
		}
	}
	// The shared ACR name must NOT gain a production suffix.
	if strings.Contains(out, "acrmemql-production") || strings.Contains(out, "acr-memql-production") {
		t.Errorf("ACR should be shared (no per-env suffix), got production-suffixed name:\n%s", out)
	}
}

// TestInvalidEnvRejected asserts an unknown environment is rejected
// with a non-zero exit.
func TestInvalidEnvRejected(t *testing.T) {
	out, err := run(t, "--env=nope", "--dry-run")
	if err == nil {
		t.Fatalf("expected non-zero exit for invalid env, got success:\n%s", out)
	}
	if !strings.Contains(out, "Invalid --env") {
		t.Errorf("expected 'Invalid --env' error, got:\n%s", out)
	}
}
