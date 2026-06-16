// Tests for the functional post-deploy gate + bff auto-promote shell script
// (scripts/deploy/post-deploy-gate.sh), the keystone of znasllc-io/memql#1519
// (#1520 folds in the bff promote). Function-based bash per the Skills+Scripts
// architecture; these run from `go test ./...` so CI catches regressions
// without a live cluster. Same package as aks_deploy_test.go -- helpers
// (aksScript / runAks / aksSyntax) are reused from there.
//
// No live cluster in CI, so the runtime checks (promote, JWKS poll, auth Job)
// cannot execute here; instead these tests assert, STATICALLY, that:
//   - the script is syntactically valid and --help/--dry-run work;
//   - the gate checks ALL THREE conditions (bff-promoted, jwks-coherence,
//     functional-auth);
//   - the promote step exists and refuses to promote on a failed analysis;
//   - aks-deploy.sh wires BOTH the promote and the gate into the deploy flow.
package deploy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// readDeployScript reads a sibling scripts/deploy script's source for static
// assertions.
func readDeployScript(t *testing.T, name string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(thisFile), name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// The gate script must be valid bash.
func TestPostDeployGateSyntax(t *testing.T) { aksSyntax(t, "post-deploy-gate.sh") }

// --help advertises the gate identity, the three conditions, and the
// promote/gate mode flags.
func TestPostDeployGateHelp(t *testing.T) {
	out, err := runAks(t, "post-deploy-gate.sh", "--help")
	if err != nil {
		t.Fatalf("--help exited non-zero: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Usage:", "--promote-only", "--gate-only", "--no-promote", "--dry-run",
		"bff promoted", "JWKS coherent", "functional auth",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("post-deploy-gate --help missing %q:\n%s", want, out)
		}
	}
}

// A full --dry-run plans the promote AND all three gate conditions and mutates
// nothing. This is the end-to-end "what would run" assertion.
func TestPostDeployGateDryRunPlansPromoteAndThreeConditions(t *testing.T) {
	requireKubectl(t) // check_prerequisites needs kubectl on PATH (it short-circuits in dry-run before cluster-info)
	out, err := runAks(t, "post-deploy-gate.sh", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run exited non-zero: %v\n%s", err, out)
	}
	for _, want := range []string{
		"rollouts promote bff", // #1520 promote
		"Gate 1/3",             // bff promoted
		"Gate 2/3",             // jwks coherence
		"Gate 3/3",             // functional auth
		"selector hash == Rollout stable color",
		"jwks.json",
		"queryActiveSpaceIds", // the BFF->agent auth round-trip
		"VALIDATED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run plan missing %q:\n%s", want, out)
		}
	}
}

// --gate-only plans the three conditions but NOT the promote step.
func TestPostDeployGateGateOnlySkipsPromote(t *testing.T) {
	requireKubectl(t)
	out, err := runAks(t, "post-deploy-gate.sh", "--gate-only", "--dry-run")
	if err != nil {
		t.Fatalf("--gate-only --dry-run exited non-zero: %v\n%s", err, out)
	}
	if strings.Contains(out, "Promote bff blue/green Rollout") {
		t.Errorf("--gate-only must NOT run the promote section:\n%s", out)
	}
	for _, want := range []string{"Gate 1/3", "Gate 2/3", "Gate 3/3"} {
		if !strings.Contains(out, want) {
			t.Errorf("--gate-only dry-run missing %q:\n%s", want, out)
		}
	}
}

// --promote-only plans the promote but NOT the gate conditions.
func TestPostDeployGatePromoteOnlySkipsGate(t *testing.T) {
	requireKubectl(t)
	out, err := runAks(t, "post-deploy-gate.sh", "--promote-only", "--dry-run")
	if err != nil {
		t.Fatalf("--promote-only --dry-run exited non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(out, "rollouts promote bff") {
		t.Errorf("--promote-only must plan the promote:\n%s", out)
	}
	if strings.Contains(out, "Gate 1/3") || strings.Contains(out, "Gate 2/3") {
		t.Errorf("--promote-only must NOT run the gate conditions:\n%s", out)
	}
}

// STATIC: the gate implements all three #1519 conditions as distinct checks.
func TestPostDeployGateChecksAllThreeConditions(t *testing.T) {
	src := readDeployScript(t, "post-deploy-gate.sh")
	for _, want := range []string{
		"function check_bff_promoted",    // 1: bff promoted
		"function check_jwks_coherent",   // 2: jwks coherence
		"function check_functional_auth", // 3: functional auth
		"run_functional_gate",            // orchestrates all three
	} {
		if !strings.Contains(src, want) {
			t.Errorf("post-deploy-gate.sh missing condition wiring %q", want)
		}
	}
	// run_functional_gate must invoke all three checks.
	for _, want := range []string{
		"check_bff_promoted", "check_jwks_coherent", "check_functional_auth",
	} {
		if strings.Count(src, want) < 2 {
			t.Errorf("run_functional_gate should call %q (expected the function def + at least one call)", want)
		}
	}
}

// STATIC: condition 1 asserts the bff-active selector hash == the Rollout's
// release (stable) color -- the check that catches the false-green.
func TestPostDeployGateAssertsActiveColorEqualsReleaseColor(t *testing.T) {
	src := readDeployScript(t, "post-deploy-gate.sh")
	for _, want := range []string{
		"rollouts-pod-template-hash", // the selector hash being compared
		"status.stableRS",            // the release color source
		"argo rollouts status",       // Rollout Healthy assertion
	} {
		if !strings.Contains(src, want) {
			t.Errorf("bff-promoted condition missing %q", want)
		}
	}
}

// STATIC: condition 2 polls the LB JWKS N times AND each identity pod directly,
// and fails on a divergent kid set.
func TestPostDeployGateJWKSPollsLBAndEachPod(t *testing.T) {
	src := readDeployScript(t, "post-deploy-gate.sh")
	for _, want := range []string{
		"poll_lb_jwks_kids",  // LB polling
		"poll_pod_jwks_kids", // per-pod direct read
		"JWKS_POLLS",         // N times
		"DIVERGENCE",         // the failure message
		"kid",                // kid-set comparison
	} {
		if !strings.Contains(src, want) {
			t.Errorf("jwks-coherence condition missing %q", want)
		}
	}
}

// STATIC: condition 3 runs a real BFF->agent authenticated round-trip via the
// service_account deploy-gate path, with a documented MEMQL_SMOKE_TOKEN
// fallback and a graceful (loud) degrade when no headless credential exists.
func TestPostDeployGateFunctionalAuthRoundTrip(t *testing.T) {
	src := readDeployScript(t, "post-deploy-gate.sh")
	for _, want := range []string{
		"service_account",          // the #691 machine-identity path
		"deploy-gate-jwt",          // the JWT Secret
		"MEMQL_SMOKE_TOKEN",        // the fallback token path
		"GATE_FUNCTIONAL_OPTIONAL", // documented graceful-degrade switch
		"bff-active",               // the round-trip targets the active color
	} {
		if !strings.Contains(src, want) {
			t.Errorf("functional-auth condition missing %q", want)
		}
	}
}

// STATIC: promote refuses to flip traffic when the prePromotionAnalysis failed
// (#1520 acceptance: a failed analysis must NOT promote).
func TestPostDeployGatePromoteRefusesOnFailedAnalysis(t *testing.T) {
	src := readDeployScript(t, "post-deploy-gate.sh")
	for _, want := range []string{
		"function promote_bff",
		"analysis_failed", // the failed-analysis guard
		"NOT promoting",   // the loud refusal
		"PROMOTE_RETRIES", // the 'promote twice' handling
	} {
		if !strings.Contains(src, want) {
			t.Errorf("promote step missing %q", want)
		}
	}
}

// STATIC: aks-deploy.sh wires BOTH the bff promote (#1520) and the functional
// gate (#1519) into the deploy flow's health_gate -- the deploy is not green
// until both run.
func TestAksDeployCallsPromoteAndFunctionalGate(t *testing.T) {
	src := readDeployScript(t, "aks-deploy.sh")
	for _, want := range []string{
		"promote_bff_rollout",         // the promote wiring function
		"functional_post_deploy_gate", // the gate wiring function
		"post-deploy-gate.sh --promote-only",
		"post-deploy-gate.sh --gate-only",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("aks-deploy.sh does not wire %q into the deploy flow", want)
		}
	}
	// Both must be invoked from health_gate (the authority that gates the
	// release + drives rollback). Crude but effective: health_gate's body must
	// reference both wiring functions.
	idx := strings.Index(src, "function health_gate")
	if idx < 0 {
		t.Fatal("aks-deploy.sh has no health_gate function")
	}
	body := src[idx:]
	for _, want := range []string{"promote_bff_rollout", "functional_post_deploy_gate"} {
		if !strings.Contains(body, want) {
			t.Errorf("health_gate must invoke %q so the deploy gates on it", want)
		}
	}
}
