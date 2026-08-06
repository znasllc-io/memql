// Static guard: no workflow that produces a required status check may carry a
// trigger-level path filter (znasllc-io/memql#3159).
//
// # The failure being guarded against
//
// On 2026-08-06 the `default` ruleset required three status checks by name.
// During the platform incident they never reported at all, and the ruleset
// reported `3 of 3 required status checks are expected` -- a permanent block
// with no failing job to look at. The gate had to be deleted to merge
// anything.
//
// A trigger-level `paths:`/`paths-ignore:` filter reproduces that state
// deliberately and silently. If `on.pull_request.paths` excludes a PR, the
// WORKFLOW DOES NOT RUN. No job is created, so no check is reported, so a
// required check sits "expected" forever. There is no red job to click.
//
// # Why job-level `if:` is safe and this is not
//
// These are not the same risk, and conflating them leads to the wrong design:
//
//   - Job-level `if:` -> the workflow runs, the job reports `skipped`, and
//     `ci-required` already treats `skipped` as a pass. Nothing hangs. This is
//     the mechanism ci.yml uses today via the `changes` job, and it is
//     deliberately left alone -- a skipped lane also costs zero runner slots,
//     which mattered when 45% of jobs were starved during the incident.
//   - Trigger-level `paths:` -> nothing runs, nothing reports, the gate hangs.
//
// So this guard constrains ONLY the trigger level. It says nothing about
// job-level conditions, and must not grow to.
//
// # Scope, and the assumption this test makes
//
// The required-check set on ruleset 16630577 is currently exactly one context,
// `ci-required`, produced by `.github/workflows/ci.yml`. That is hard-coded
// here on purpose: a test that queried the GitHub API would need network and
// credentials to run, which would make it useless in exactly the offline and
// degraded conditions this repo's guards exist for.
//
// The assumption is made falsifiable instead: TestCIRequiredIsStillDefinedHere
// fails if ci.yml stops defining the `ci-required` job, which is the event
// that would silently make this guard point at the wrong workflow.
//
// # Note on the needs-completeness invariant
//
// The sibling invariant -- every ci.yml lane appears in `ci-required.needs` --
// is NOT implemented here. It already ships as TestEveryCILaneGatesTheMerge in
// scripts/dev/ci_required_lane_gate_test.go, which additionally guards the
// exemption list itself. A second copy would fork the `advisoryLanes` opt-out
// across two files, which is worse than the gap it would close.
package ci

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"
)

// gateWorkflow is the subset of a workflow file this guard reads.
//
// `On` is map[string]any because its members have three different shapes --
// `merge_group:` is null, `pull_request:` is a mapping, `schedule:` is a
// sequence -- and the question here is only whether a mapping member carries a
// path filter.
//
// Parsed with yaml.v3 rather than scanned line by line, following the
// convention in scripts/dev: a line scanner is defeated by putting the
// expected pattern in a step's `name:` or a trailing comment. Reading
// structure makes that unrepresentable rather than merely tested for.
type gateWorkflow struct {
	On   map[string]any `yaml:"on"`
	Jobs map[string]any `yaml:"jobs"`
}

// gateRepoRoot resolves the repository root from this file's location.
//
// Deliberately file-prefixed and unexported. scripts/ci also hosts
// workflow_timeout_test.go (memql#3158); the two land as independent tasks
// touching no common file, and a shared helper would turn that independence
// into a compile-level conflict.
func gateRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// scripts/ci/ -> repo root is two directories up.
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// gateLoadWorkflow reads and parses a workflow file under .github/workflows.
func gateLoadWorkflow(t *testing.T, name string) gateWorkflow {
	t.Helper()
	path := filepath.Join(gateRepoRoot(t), ".github", "workflows", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf gateWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return wf
}

// gateRequiredCheckWorkflows names the workflow files that produce a context
// required by ruleset 16630577. See the package header for why this is a
// literal rather than an API call.
var gateRequiredCheckWorkflows = []string{"ci.yml"}

// TestNoTriggerLevelPathFilterOnRequiredCheckWorkflows is the guard.
//
// It checks every trigger member that is a mapping (`push`, `pull_request`,
// ...) for `paths` or `paths-ignore`. Both spellings are checked, and every
// trigger is checked rather than just `pull_request` -- a filter on `push`
// starves the required check on merges to main, which is how a broken main
// goes unnoticed.
func TestNoTriggerLevelPathFilterOnRequiredCheckWorkflows(t *testing.T) {
	for _, name := range gateRequiredCheckWorkflows {
		wf := gateLoadWorkflow(t, name)
		if len(wf.On) == 0 {
			t.Fatalf("%s: parsed no `on:` triggers at all -- the guard cannot "+
				"verify what it was written to verify, so it fails closed", name)
		}
		for trigger, spec := range wf.On {
			mapping, ok := spec.(map[string]any)
			if !ok {
				// `merge_group:` (null) and `schedule:` (sequence) cannot
				// carry a path filter.
				continue
			}
			for _, key := range []string{"paths", "paths-ignore"} {
				if _, found := mapping[key]; found {
					t.Errorf("%s: `on.%s.%s` is a TRIGGER-LEVEL path filter, and %s "+
						"produces a required status check.\n"+
						"A filtered workflow does not run, so no job reports, so the "+
						"required check sits \"expected\" forever -- the exact state that "+
						"forced the gate to be deleted on 2026-08-06.\n"+
						"Use a job-level `if:` driven by the `changes` job instead: a "+
						"skipped job reports `skipped`, which ci-required treats as a pass.",
						name, trigger, key, name)
				}
			}
		}
	}
}

// TestCIRequiredIsStillDefinedHere falsifies the assumption baked into
// gateRequiredCheckWorkflows.
//
// If the `ci-required` job moves out of ci.yml, the guard above silently
// starts checking a workflow that no longer produces the required check --
// passing while guarding nothing. That is the same shape as the fail-open this
// repo already had to close twice (memql#2972, memql#3019), so it is asserted
// rather than assumed.
func TestCIRequiredIsStillDefinedHere(t *testing.T) {
	const job = "ci-required"
	wf := gateLoadWorkflow(t, "ci.yml")
	if _, ok := wf.Jobs[job]; !ok {
		t.Fatalf("ci.yml no longer defines the %q job.\n"+
			"gateRequiredCheckWorkflows assumes ci.yml is the workflow producing the "+
			"required check. Update that list to name whichever workflow produces it "+
			"now, or this guard protects nothing.", job)
	}
}
