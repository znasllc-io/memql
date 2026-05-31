// Tests for the Azure cluster deploy shell script
// (.claude/scripts/deploy.sh). The script itself is function-based bash
// per the Skills+Scripts architecture (znasllc-io/memql#495, epic #491);
// these tests exercise its dry-run / validation contract from
// `go test ./...` so CI catches regressions without needing a live Azure
// subscription. They mirror deploy_setup_test.go (same package).
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// deployScriptPath resolves .claude/scripts/deploy.sh relative to the
// repo root. This test file lives at scripts/deploy/, so the root is two
// directories up.
func deployScriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	p := filepath.Join(root, ".claude", "scripts", "deploy.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("deploy.sh not found at %s: %v", p, err)
	}
	return p
}

// runDeploy executes deploy.sh with the given args and returns combined
// output + the error (nil on exit 0). ENV is forced empty so the
// dry-run plan is deterministic regardless of the developer's shell.
func runDeploy(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell deploy test")
	}
	cmd := exec.Command("bash", append([]string{deployScriptPath(t)}, args...)...)
	cmd.Env = append(os.Environ(), "ENV=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestDeploySyntax fails fast on a bash syntax error (`bash -n`).
func TestDeploySyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out, err := exec.Command("bash", "-n", deployScriptPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %v\n%s", err, out)
	}
}

// TestDeployHelp checks that --help exits 0 and documents the flags.
func TestDeployHelp(t *testing.T) {
	out, err := runDeploy(t, "--help")
	if err != nil {
		t.Fatalf("--help exited non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Usage:") {
		t.Errorf("--help missing 'Usage:' banner:\n%s", out)
	}
	for _, want := range []string{"--dry-run", "--env=", "--carrier-version=", "--skip-build", "deploy.sh"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help output missing %q", want)
		}
	}
}

// TestDeployDryRunStagingPlan asserts the staging dry-run exits 0,
// mutates nothing (every az/build call is a [plan] line), deploys one
// internal-ingress app per engine node-type from the memql image and the
// external-ingress BFF carrier from the carrier image, and wires the
// Key Vault DSN secret ref.
func TestDeployDryRunStagingPlan(t *testing.T) {
	out, err := runDeploy(t, "--env=staging", "--dry-run")
	if err != nil {
		t.Fatalf("staging dry-run exited non-zero: %v\n%s", err, out)
	}

	// Side-effect-free guarantee: in dry-run every mutating az / build
	// invocation must be prefixed with the [plan] marker. A bare
	// "az containerapp" or "docker push" without [plan] means the gate leaked.
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, mut := range []string{"az ", "docker push", "docker build"} {
			if strings.HasPrefix(trimmed, mut) {
				t.Errorf("dry-run appears to execute %q (no [plan] marker): %q", mut, trimmed)
			}
		}
	}

	mustContain := []string{
		// one app per engine node-type, internal ingress, memql image
		"ca-memql-cognition-staging",
		"ca-memql-voice-staging",
		"ca-memql-agent-staging",
		"ca-memql-planner-staging",
		"ca-memql-identity-staging",
		"ca-memql-workbench-staging",
		"--ingress internal",
		"MEMQL_NODE_TYPE=cognition",
		"acrmemql.azurecr.io/memql:0.9.0",
		// the BFF carrier, external ingress, carrier image, bff node-type
		"ca-copresent-bff-staging",
		"--ingress external",
		"MEMQL_NODE_TYPE=bff",
		"acrmemql.azurecr.io/memql-bff-copresent:0.9.0",
		// A2 genesis-envelope model: all three secrets via Key Vault
		// managed-identity refs, surfaced as secretref env vars.
		"keyvaultref:https://kv-memql-staging.vault.azure.net/secrets/memory-nodes-database-dsn,identityref:system",
		"keyvaultref:https://kv-memql-staging.vault.azure.net/secrets/memql-master-key,identityref:system",
		"keyvaultref:https://kv-memql-staging.vault.azure.net/secrets/memql-genesis-b64,identityref:system",
		"MEMORY_NODES_DATABASE_DSN=secretref:memory-nodes-database-dsn",
		"MEMQL_MASTER_KEY=secretref:memql-master-key",
		"MEMQL_GENESIS_B64=secretref:memql-genesis-b64",
		// in-process envelope auto-load turned on
		"MEMQL_GENESIS_AUTOLOAD=true",
		// per-node overrides from service.yaml that WIN set-if-absent
		"SERVER_ADDRESS=0.0.0.0:8085",
		"SERVER_ALLOWED_ORIGINS=*",
		"IDENTITY_VERIFIER_AUDIENCE=memql",
		"--target-port 8085",
		"cae-memql-staging",
		// each app's managed identity gets read access to the vault
		`az role assignment create --role "Key Vault Secrets User"`,
		"DRY RUN complete",
		"State report",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("staging dry-run output missing %q\n---\n%s", want, out)
		}
	}
}

// TestDeployDryRunProductionParameterized proves the production path is
// parameterized (not hardcoded to staging): app + resource names carry
// the production suffix and the production stub warning fires, while the
// shared ACR stays env-independent.
func TestDeployDryRunProductionParameterized(t *testing.T) {
	out, err := runDeploy(t, "--env=production", "--dry-run")
	if err != nil {
		t.Fatalf("production dry-run exited non-zero: %v\n%s", err, out)
	}
	for _, want := range []string{
		"ca-memql-cognition-production",
		"ca-copresent-bff-production",
		"cae-memql-production",
		"rg-memql-production",
		"kv-memql-production",
		"parameterized STUB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("production dry-run output missing %q\n---\n%s", want, out)
		}
	}
	// The shared ACR must NOT gain a production suffix.
	if strings.Contains(out, "acrmemql-production") || strings.Contains(out, "acr-memql-production") {
		t.Errorf("ACR should be shared (no per-env suffix), got production-suffixed name:\n%s", out)
	}
}

// TestDeployCarrierVersionPinned proves the BFF carrier can be deployed
// at a pinned version distinct from the engine version (copresent pins
// the carrier later). --skip-build keeps it side-effect-free.
func TestDeployCarrierVersionPinned(t *testing.T) {
	out, err := runDeploy(t, "--env=staging", "--skip-build",
		"--version=1.2.3", "--carrier-version=0.9.5", "--dry-run")
	if err != nil {
		t.Fatalf("pinned-carrier dry-run exited non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(out, "acrmemql.azurecr.io/memql:1.2.3") {
		t.Errorf("expected engine image at the engine version 1.2.3:\n%s", out)
	}
	if !strings.Contains(out, "acrmemql.azurecr.io/memql-bff-copresent:0.9.5") {
		t.Errorf("expected carrier image at the pinned carrier version 0.9.5:\n%s", out)
	}
}

// TestDeployInvalidEnvRejected asserts an unknown environment is
// rejected with a non-zero exit.
func TestDeployInvalidEnvRejected(t *testing.T) {
	out, err := runDeploy(t, "--env=nope", "--dry-run")
	if err == nil {
		t.Fatalf("expected non-zero exit for invalid env, got success:\n%s", out)
	}
	if !strings.Contains(out, "Invalid --env") {
		t.Errorf("expected 'Invalid --env' error, got:\n%s", out)
	}
}

// TestDeployInvalidVersionRejected asserts a non-semver --version fails.
func TestDeployInvalidVersionRejected(t *testing.T) {
	out, err := runDeploy(t, "--version=1.2", "--dry-run")
	if err == nil {
		t.Fatalf("expected non-zero exit for bad version, got success:\n%s", out)
	}
	if !strings.Contains(out, "clean semver") {
		t.Errorf("expected 'clean semver' error, got:\n%s", out)
	}
}
