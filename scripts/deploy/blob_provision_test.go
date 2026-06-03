// Tests for the Azure Blob Storage provisioning shell script
// (scripts/deploy/blob-provision.sh). The script is function-based bash per
// the Skills+Scripts architecture (znasllc-io/memql#807, epic #805); these
// tests exercise its dry-run / validation contract from `go test ./...` so
// CI catches regressions without needing a live Azure subscription.
// Mirrors tiger_provision_test.go (same package).
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// blobScriptPath resolves scripts/deploy/blob-provision.sh.  This test file
// lives at scripts/deploy/, so the script is a sibling.
func blobScriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(thisFile), "blob-provision.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("blob-provision.sh not found at %s: %v", p, err)
	}
	return p
}

// runBlob executes blob-provision.sh with the given args.  ENV is forced
// empty so the dry-run plan is deterministic regardless of the developer's
// shell environment.
func runBlob(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell provisioning test")
	}
	cmd := exec.Command("bash", append([]string{blobScriptPath(t)}, args...)...)
	cmd.Env = append(os.Environ(), "ENV=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestBlobSyntax fails fast on a bash syntax error (`bash -n`).
func TestBlobSyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out, err := exec.Command("bash", "-n", blobScriptPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %v\n%s", err, out)
	}
}

// TestBlobHelp checks that --help exits 0 and documents the key flags.
func TestBlobHelp(t *testing.T) {
	out, err := runBlob(t, "--help")
	if err != nil {
		t.Fatalf("--help exited non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Usage:") {
		t.Errorf("--help missing 'Usage:' banner:\n%s", out)
	}
	for _, want := range []string{"--dry-run", "--env=", "blob-provision.sh", "--account-name", "--container"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help output missing %q", want)
		}
	}
}

// TestBlobDryRunStagingPlan asserts the staging dry-run exits 0, mutates
// nothing (every az call is a [plan] line), names the correct per-env
// resources, and prints instructions to add vars to staging.genesis.env
// (NOT local.genesis.env -- the env-specificity guard).
func TestBlobDryRunStagingPlan(t *testing.T) {
	out, err := runBlob(t, "--env=staging", "--dry-run")
	if err != nil {
		t.Fatalf("staging dry-run exited non-zero: %v\n%s", err, out)
	}

	// Side-effect-free guarantee: in dry-run every mutating az call must be
	// prefixed with the [plan] marker. Bare "az storage" or "az keyvault"
	// without the marker means the gate leaked.
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, mutPrefix := range []string{"az storage account create", "az storage container create", "az storage account show-connection-string"} {
			if strings.HasPrefix(trimmed, mutPrefix) {
				t.Errorf("dry-run appears to execute %q (no [plan] marker): %q", mutPrefix, trimmed)
			}
		}
	}

	mustContain := []string{
		"rg-memql-staging",      // per-env resource group
		"stmemqlstaging",        // per-env storage account name
		"attachments",           // default container
		"staging.genesis.env",   // env-specificity guard: must reference staging file
		"MEMQL_AZURE_STORAGE_CONNECTION_STRING",
		"MEMQL_AZURE_BLOB_CONTAINER",
		"DRY RUN complete",
		"State report",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("staging dry-run output missing %q\n---\n%s", want, out)
		}
	}

	// Env-specificity guard: the staging connection string must NOT be
	// directed to local.genesis.env.  That file is for shared local-dev
	// secrets; staging uses Azurite locally and the real account on the
	// cluster.
	if strings.Contains(out, "local.genesis.env") {
		t.Errorf("staging dry-run output references local.genesis.env (must use staging.genesis.env instead):\n%s", out)
	}
}

// TestBlobDryRunProductionParameterized proves the production path is
// parameterized (not hardcoded to staging): resource names carry the
// production suffix and the production stub warning fires.
func TestBlobDryRunProductionParameterized(t *testing.T) {
	out, err := runBlob(t, "--env=production", "--dry-run")
	if err != nil {
		t.Fatalf("production dry-run exited non-zero: %v\n%s", err, out)
	}
	for _, want := range []string{
		"rg-memql-production",
		"stmemqlproduction",
		"parameterized stub",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("production dry-run output missing %q\n---\n%s", want, out)
		}
	}
	// The staging account name must NOT appear in the production plan.
	if strings.Contains(out, "stmemqlstaging") {
		t.Errorf("production dry-run references staging account name 'stmemqlstaging':\n%s", out)
	}
}

// TestBlobInvalidEnvRejected asserts an unknown environment is rejected with
// a non-zero exit.
func TestBlobInvalidEnvRejected(t *testing.T) {
	out, err := runBlob(t, "--env=dev", "--dry-run")
	if err == nil {
		t.Fatalf("expected non-zero exit for invalid env, got success:\n%s", out)
	}
	if !strings.Contains(out, "Invalid --env") {
		t.Errorf("expected 'Invalid --env' error, got:\n%s", out)
	}
}

// TestBlobCustomAccountName asserts that --account-name overrides the default
// and the override name appears in the dry-run plan.
func TestBlobCustomAccountName(t *testing.T) {
	out, err := runBlob(t, "--env=staging", "--account-name=stmemqlcustom", "--dry-run")
	if err != nil {
		t.Fatalf("custom account-name dry-run exited non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(out, "stmemqlcustom") {
		t.Errorf("dry-run output missing custom account name 'stmemqlcustom':\n%s", out)
	}
}

// TestBlobInvalidAccountNameRejected asserts that an account name with
// hyphens (not allowed by Azure) is rejected before any az call.
func TestBlobInvalidAccountNameRejected(t *testing.T) {
	out, err := runBlob(t, "--env=staging", "--account-name=st-memql-staging", "--dry-run")
	if err == nil {
		t.Fatalf("expected non-zero exit for invalid account name, got success:\n%s", out)
	}
	if !strings.Contains(out, "Invalid storage account name") {
		t.Errorf("expected 'Invalid storage account name' error, got:\n%s", out)
	}
}
