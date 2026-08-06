// Static guard: every CI job declares a `timeout-minutes`
// (znasllc-io/memql#3158).
//
// # What this bounds, and what it does not
//
// Before this guard, exactly ONE `timeout-minutes` existed in the entire
// .github/workflows tree (publish-docs-bundle.yml). Every CI job inherited
// GitHub's 360-minute default, so a hung job held a runner slot for six hours.
//
// Be precise about the benefit: this bounds a HANG. It does NOT address the
// starvation measured in the 2026-08-06 audit, where 27 of 60 jobs (45%) were
// cancelled at exactly 15m00s having never been assigned a runner. A job with
// no runner is not yet timing against its own budget, so no value here would
// have changed that outcome. Nor does it touch dispatch failure, where no run
// is created at all. Do not cite this guard as incident mitigation.
//
// # Why a guard rather than just the values
//
// The values are the easy half and the half that stays correct on its own. The
// failure mode is the NEXT job: someone adds a lane, forgets the timeout, and
// it silently inherits 360 minutes. That is invisible in review precisely
// because it is an absence, which is the same shape as the ci-required
// needs-completeness gap (memql#3019) -- absences are what static guards are
// for.
//
// # Why the bound is asserted as a floor, not an equality
//
// TestCITimeoutsAreNotAbsurdlyTight checks each job's timeout against the
// audit's measured median for that lane, requiring at least 2x headroom. A
// timeout tight enough to cancel a healthy run converts a slow day into a red
// gate, which is strictly worse than the 360-minute default it replaced. The
// medians are the audit's, recorded inline so a future tightening has to
// argue with a number rather than a vibe.
package ci

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"
)

// timeoutWorkflow is the subset of ci.yml this guard reads.
type timeoutWorkflow struct {
	Jobs map[string]struct {
		Name           string `yaml:"name"`
		TimeoutMinutes *int   `yaml:"timeout-minutes"`
	} `yaml:"jobs"`
}

// timeoutRepoRoot resolves the repository root from this file's location.
//
// Deliberately file-prefixed and unexported. scripts/ci also hosts
// workflow_gate_test.go (memql#3159); the two land as independent tasks
// touching no common file, and a shared helper would turn that independence
// into a compile-level conflict.
func timeoutRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// scripts/ci/ -> repo root is two directories up.
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

func timeoutLoadCI(t *testing.T) timeoutWorkflow {
	t.Helper()
	path := filepath.Join(timeoutRepoRoot(t), ".github", "workflows", "ci.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf timeoutWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return wf
}

// TestEveryCIJobHasATimeout is the guard.
func TestEveryCIJobHasATimeout(t *testing.T) {
	wf := timeoutLoadCI(t)
	if len(wf.Jobs) == 0 {
		t.Fatal("ci.yml: parsed no jobs at all -- the guard cannot verify what it " +
			"was written to verify, so it fails closed")
	}
	for job, spec := range wf.Jobs {
		if spec.TimeoutMinutes == nil {
			t.Errorf("ci.yml job %q has no `timeout-minutes`.\n"+
				"It therefore inherits GitHub's 360-minute default, so a hang holds a "+
				"runner slot for six hours. Add one: 20 for the heavy lanes "+
				"(go-checks / db-tests / conformance), 10 for the rest (design D4.1).", job)
			continue
		}
		if *spec.TimeoutMinutes <= 0 {
			t.Errorf("ci.yml job %q has a non-positive `timeout-minutes` (%d)",
				job, *spec.TimeoutMinutes)
		}
	}
}

// timeoutMedianMinutes are the per-lane medians measured in the 2026-08-06
// audit (section 1.3). Keys are job ids; a job absent from this map is not
// checked for headroom, only for presence.
var timeoutMedianMinutes = map[string]float64{
	"changes":          0.1,
	"go-checks":        6.3,
	"db-tests":         5.8,
	"vscode-extension": 4.0,
	"build-voice":      2.5,
	"build-node-tags":  1.9,
	"conformance":      1.6,
	"sdk-ts-typecheck": 0.3,
	"ci-required":      0.1,
}

// TestCITimeoutsAreNotAbsurdlyTight keeps the values honest in the other
// direction: a timeout below 2x a lane's measured median turns a slow-but-
// healthy run into a red required check.
func TestCITimeoutsAreNotAbsurdlyTight(t *testing.T) {
	wf := timeoutLoadCI(t)
	for job, spec := range wf.Jobs {
		median, known := timeoutMedianMinutes[job]
		if !known || spec.TimeoutMinutes == nil {
			continue
		}
		if float64(*spec.TimeoutMinutes) < 2*median {
			t.Errorf("ci.yml job %q: timeout-minutes=%d is under 2x its measured "+
				"median of %.1fm (audit 1.3). A timeout that can cancel a healthy run "+
				"converts a slow day into a red gate, which is worse than the default "+
				"it replaced.", job, *spec.TimeoutMinutes, median)
		}
	}
}
