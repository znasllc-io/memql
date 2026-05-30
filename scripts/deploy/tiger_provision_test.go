// Tests for the Tiger Cloud provisioning shell script
// (scripts/deploy/tiger-provision.sh). The script itself is
// function-based bash per the Skills+Scripts architecture
// (znasllc-io/memql#494, epic #491); these tests exercise its dry-run /
// validation contract from `go test ./...` so CI catches regressions
// without needing a live Tiger Cloud account or Azure subscription. They
// mirror deploy_setup_test.go (same package).
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// tigerScriptPath resolves scripts/deploy/tiger-provision.sh. This test
// file lives at scripts/deploy/, so the script is a sibling.
func tigerScriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(thisFile), "tiger-provision.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("tiger-provision.sh not found at %s: %v", p, err)
	}
	return p
}

// runTiger executes tiger-provision.sh with the given args. ENV is
// forced empty so the dry-run plan is deterministic regardless of the
// developer's shell.
func runTiger(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell provisioning test")
	}
	cmd := exec.Command("bash", append([]string{tigerScriptPath(t)}, args...)...)
	cmd.Env = append(os.Environ(), "ENV=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestTigerSyntax fails fast on a bash syntax error (`bash -n`).
func TestTigerSyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out, err := exec.Command("bash", "-n", tigerScriptPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %v\n%s", err, out)
	}
}

// TestTigerHelp checks that --help exits 0 and documents the flags.
func TestTigerHelp(t *testing.T) {
	out, err := runTiger(t, "--help")
	if err != nil {
		t.Fatalf("--help exited non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Usage:") {
		t.Errorf("--help missing 'Usage:' banner:\n%s", out)
	}
	for _, want := range []string{"--dry-run", "--env=", "tiger-provision.sh"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help output missing %q", want)
		}
	}
}

// TestTigerDryRunStagingPlan asserts the staging dry-run exits 0,
// mutates nothing (every tiger/az call is a [plan] line), provisions the
// memql-staging service in Azure East US, confirms the timescaledb +
// vector extensions, and plans the DSN -> Key Vault write under the same
// secret name the bootstrap uses.
func TestTigerDryRunStagingPlan(t *testing.T) {
	out, err := runTiger(t, "--env=staging", "--dry-run")
	if err != nil {
		t.Fatalf("staging dry-run exited non-zero: %v\n%s", err, out)
	}

	// Side-effect-free guarantee: in dry-run every mutating tiger / az
	// call must be prefixed with the [plan] marker.
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, mut := range []string{"tiger service create", "tiger service exec", "az keyvault"} {
			if strings.HasPrefix(trimmed, mut) {
				t.Errorf("dry-run appears to execute %q (no [plan] marker): %q", mut, trimmed)
			}
		}
	}

	mustContain := []string{
		"memql-staging",        // per-env service name
		"azure-eastus",         // Azure East US region slug
		"timescaledb",          // Timescale Community extension
		"vector",               // pgvector extension
		"kv-memql-staging",     // per-env Key Vault
		"memory-nodes-database-dsn", // DSN secret name (matches bootstrap)
		"DRY RUN complete",
		"State report",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("staging dry-run output missing %q\n---\n%s", want, out)
		}
	}
}

// TestTigerDryRunProductionParameterized proves the production path is
// parameterized (not hardcoded to staging): the service + Key Vault
// names carry the production suffix and the production stub warning fires.
func TestTigerDryRunProductionParameterized(t *testing.T) {
	out, err := runTiger(t, "--env=production", "--dry-run")
	if err != nil {
		t.Fatalf("production dry-run exited non-zero: %v\n%s", err, out)
	}
	for _, want := range []string{
		"memql-production",
		"kv-memql-production",
		"parameterized STUB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("production dry-run output missing %q\n---\n%s", want, out)
		}
	}
}

// TestTigerInvalidEnvRejected asserts an unknown environment is rejected
// with a non-zero exit.
func TestTigerInvalidEnvRejected(t *testing.T) {
	out, err := runTiger(t, "--env=nope", "--dry-run")
	if err == nil {
		t.Fatalf("expected non-zero exit for invalid env, got success:\n%s", out)
	}
	if !strings.Contains(out, "Invalid --env") {
		t.Errorf("expected 'Invalid --env' error, got:\n%s", out)
	}
}
