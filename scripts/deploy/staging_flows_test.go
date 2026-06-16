// Tests for the idempotent staging operator entrypoints
// (scripts/deploy/staging-release.sh + staging-reset.sh, znasllc-io/memql#1524).
//
// These wrap the deploy engine (aks-deploy.sh), the functional post-deploy gate
// (post-deploy-gate.sh, #1519/#1520), and the auth-coherent DB reset
// (staging-db-reset.sh, #1500/#1522) into two single, re-runnable commands. The
// value is in the GUARDS + the fail-loud delegation, so the tests assert: bash
// syntax, a --help contract, a no-cluster --dry-run plan in the right order, and
// (by source inspection) that failures are NOT swallowed into a green result.
// They run from `go test ./...` with no live cluster. Same package as the other
// scripts/deploy tests; names are sf-prefixed to avoid collisions.
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func sfScriptPath(t *testing.T, name string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(thisFile), name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("%s not found at %s: %v", name, p, err)
	}
	return p
}

func sfSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(sfScriptPath(t, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func sfRequireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
}

var sfScripts = []string{"staging-release.sh", "staging-reset.sh"}

// TestStagingFlowsSyntax fails fast on a bash syntax error in either script.
func TestStagingFlowsSyntax(t *testing.T) {
	sfRequireBash(t)
	for _, s := range sfScripts {
		out, err := exec.Command("bash", "-n", sfScriptPath(t, s)).CombinedOutput()
		if err != nil {
			t.Fatalf("bash -n %s failed: %v\n%s", s, err, out)
		}
	}
}

// TestStagingFlowsExecutable asserts both entrypoints are committed +x.
func TestStagingFlowsExecutable(t *testing.T) {
	for _, s := range sfScripts {
		fi, err := os.Stat(sfScriptPath(t, s))
		if err != nil {
			t.Fatalf("stat %s: %v", s, err)
		}
		if fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable (mode %o)", s, fi.Mode().Perm())
		}
	}
}

// TestStagingReleaseHelp asserts --help exits 0 and documents the contract,
// including the recovery (--verify) mode that folds in the 2026-06-16 incident.
func TestStagingReleaseHelp(t *testing.T) {
	sfRequireBash(t)
	out, err := exec.Command("bash", sfScriptPath(t, "staging-release.sh"), "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("--help exited non-zero: %v\n%s", err, out)
	}
	for _, want := range []string{"Usage:", "--env=", "--verify", "--dry-run", "RECOVER"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("staging-release.sh --help missing %q\n%s", want, out)
		}
	}
}

// TestStagingResetHelp asserts --help exits 0 and documents the destructive,
// staging-only, verified contract.
func TestStagingResetHelp(t *testing.T) {
	sfRequireBash(t)
	out, err := exec.Command("bash", sfScriptPath(t, "staging-reset.sh"), "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("--help exited non-zero: %v\n%s", err, out)
	}
	for _, want := range []string{"Usage:", "--env=", "--dry-run", "DESTRUCTIVE"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("staging-reset.sh --help missing %q\n%s", want, out)
		}
	}
}

// TestStagingReleaseDryRunPlan: a full-release --dry-run needs NO cluster and
// plans the coherence pre-flight BEFORE delegating to aks-deploy.sh, and never
// claims success (it ends on the dry-run line).
func TestStagingReleaseDryRunPlan(t *testing.T) {
	sfRequireBash(t)
	out, err := exec.Command("bash", sfScriptPath(t, "staging-release.sh"),
		"--env=staging", "--version=0.0.0-test", "--dry-run").CombinedOutput()
	if err != nil {
		t.Fatalf("--dry-run exited non-zero (should need no cluster): %v\n%s", err, out)
	}
	s := string(out)
	for _, want := range []string{"signing seed", "aks-deploy.sh", "dry-run plan only"} {
		if !strings.Contains(s, want) {
			t.Errorf("release --dry-run plan missing %q\n%s", want, s)
		}
	}
	seed := strings.Index(s, "signing seed")
	deploy := strings.Index(s, "aks-deploy.sh")
	if seed < 0 || deploy < 0 || seed > deploy {
		t.Errorf("coherence pre-flight must plan BEFORE the aks-deploy delegation (seed@%d, deploy@%d)", seed, deploy)
	}
}

// TestStagingReleaseVerifyDryRunPlan: the recovery (--verify) mode plans the bff
// promote + functional gate (post-deploy-gate.sh) and NOT a build/apply.
func TestStagingReleaseVerifyDryRunPlan(t *testing.T) {
	sfRequireBash(t)
	out, err := exec.Command("bash", sfScriptPath(t, "staging-release.sh"),
		"--verify", "--dry-run").CombinedOutput()
	if err != nil {
		t.Fatalf("--verify --dry-run exited non-zero: %v\n%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "post-deploy-gate.sh") {
		t.Errorf("recovery plan must delegate to post-deploy-gate.sh\n%s", s)
	}
	if strings.Contains(s, "aks-deploy.sh") {
		t.Errorf("recovery (--verify) must NOT build/apply via aks-deploy.sh\n%s", s)
	}
}

// TestStagingResetDryRunPlan: a reset --dry-run needs NO cluster and plans the
// auth-coherent wipe then the post-reset functional verification, in order.
func TestStagingResetDryRunPlan(t *testing.T) {
	sfRequireBash(t)
	out, err := exec.Command("bash", sfScriptPath(t, "staging-reset.sh"),
		"--env=staging", "--dry-run").CombinedOutput()
	if err != nil {
		t.Fatalf("--dry-run exited non-zero (should need no cluster): %v\n%s", err, out)
	}
	s := string(out)
	reset := strings.Index(s, "staging-db-reset.sh")
	gate := strings.Index(s, "post-deploy-gate.sh")
	if reset < 0 || gate < 0 {
		t.Fatalf("reset --dry-run must plan staging-db-reset.sh then post-deploy-gate.sh\n%s", s)
	}
	if reset > gate {
		t.Errorf("the wipe must plan BEFORE the post-reset verification (reset@%d, gate@%d)", reset, gate)
	}
}

// TestStagingFlowsRefuseNonStaging: both entrypoints are staging-only and must
// refuse a non-staging --env outright (a prod guard), exiting non-zero with no
// cluster access.
func TestStagingFlowsRefuseNonStaging(t *testing.T) {
	sfRequireBash(t)
	for _, s := range sfScripts {
		out, err := exec.Command("bash", sfScriptPath(t, s), "--env=prod", "--dry-run").CombinedOutput()
		if err == nil {
			t.Errorf("%s accepted --env=prod (must refuse, staging-only)\n%s", s, out)
		}
		if !strings.Contains(string(out), "staging") {
			t.Errorf("%s prod-refusal should mention staging-only\n%s", s, out)
		}
	}
}

// TestStagingFlowsFailLoud asserts (by source inspection) that a non-zero from a
// delegated script aborts with exit 1 rather than falling through to the success
// line -- the false-green class #1518/#1519/#1524 exist to kill.
func TestStagingFlowsFailLoud(t *testing.T) {
	rel := sfSource(t, "staging-release.sh")
	for _, want := range []string{
		`if ! bash "$DEPLOY_SCRIPT"`,
		`if ! bash "$GATE_SCRIPT"`,
	} {
		if !strings.Contains(rel, want) {
			t.Errorf("staging-release.sh must fail loud on a delegate error; missing %q", want)
		}
	}
	reset := sfSource(t, "staging-reset.sh")
	for _, want := range []string{
		`if ! bash "$RESET_SCRIPT"`,
		`if ! bash "$GATE_SCRIPT"`,
	} {
		if !strings.Contains(reset, want) {
			t.Errorf("staging-reset.sh must fail loud on a delegate error; missing %q", want)
		}
	}
}
