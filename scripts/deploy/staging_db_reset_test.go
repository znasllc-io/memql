// Tests for the manual staging DB reset (scripts/deploy/staging-db-reset.sh,
// znasllc-io/memql#1500). The script is DESTRUCTIVE, so most of its value is in
// the guards: staging-only, a kube-context check, a typed confirmation, replica
// restore on exit, and -- critically -- that it is NEVER wired into a deploy.
// These run from `go test ./...` with no live cluster. Same package as the
// other scripts/deploy tests; names are sdbr-prefixed to avoid collisions.
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func sdbrScriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(thisFile), "staging-db-reset.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("staging-db-reset.sh not found at %s: %v", p, err)
	}
	return p
}

func sdbrSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(sdbrScriptPath(t))
	if err != nil {
		t.Fatalf("read staging-db-reset.sh: %v", err)
	}
	return string(b)
}

// TestStagingDBResetSyntax fails fast on a bash syntax error.
func TestStagingDBResetSyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out, err := exec.Command("bash", "-n", sdbrScriptPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %v\n%s", err, out)
	}
}

// TestStagingDBResetHelp asserts --help exits 0 and documents the contract.
func TestStagingDBResetHelp(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out, err := exec.Command("bash", sdbrScriptPath(t), "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("--help exited non-zero: %v\n%s", err, out)
	}
	for _, want := range []string{"Usage:", "--env=", "--dry-run", "NEVER part of a deploy"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("--help missing %q:\n%s", want, out)
		}
	}
}

// TestStagingDBResetRefusesNonStaging: --env=production is rejected before any
// cluster access (the env guard runs first), so this needs no kubectl.
func TestStagingDBResetRefusesNonStaging(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out, err := exec.Command("bash", sdbrScriptPath(t), "--env=production", "--dry-run").CombinedOutput()
	if err == nil {
		t.Fatalf("--env=production should be refused, but exited 0:\n%s", out)
	}
	if !strings.Contains(string(out), "staging") {
		t.Errorf("refusal should mention staging-only:\n%s", out)
	}
}

// TestStagingDBResetGuards statically locks in the safety properties so a future
// edit can't quietly drop them.
func TestStagingDBResetGuards(t *testing.T) {
	src := sdbrSource(t)
	checks := map[string]string{
		`staging-only env gate`:          `ALLOWED_ENV="staging"`,
		`wrong-cluster guard`:            `EXPECTED_CONTEXT_SUBSTR`,
		`typed confirmation`:             `CONFIRM_PHRASE`,
		`replica restore via trap`:       `trap restore_replicas EXIT`,
		`destructive wipe`:               `DROP SCHEMA`,
		`schema rebuilt via migrate Job`: `migrate-job.yaml`,
		`quiesces Argo Rollouts too`:     `get rollout`,
	}
	for name, want := range checks {
		if !strings.Contains(src, want) {
			t.Errorf("missing %s (expected %q in the script)", name, want)
		}
	}
}

// TestStagingDBResetNotWiredIntoDeploy is the load-bearing guarantee: the deploy
// orchestrator must never reference the reset. A DB wipe on deploy would be a
// catastrophe.
func TestStagingDBResetNotWiredIntoDeploy(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	deployDir := filepath.Dir(thisFile)
	for _, f := range []string{"aks-deploy.sh", "aks-apply.sh"} {
		b, err := os.ReadFile(filepath.Join(deployDir, f))
		if err != nil {
			continue // file may not exist in all checkouts
		}
		if strings.Contains(string(b), "staging-db-reset") {
			t.Errorf("%s references staging-db-reset -- the DB reset must NEVER be wired into deploy", f)
		}
	}
}
