package main

import (
	"encoding/json"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

// A release reuses what the run that tested its tree executed, and only when the
// check is the same check. This holds that against this repository's own manifest:
// what a pull request and a release compile for each reusable step differs only in
// what orders the plan, so a change to the manifest or the compiler that made them
// differ would quietly end the saving rather than fail anything
// (docs/superpowers/specs/2026-10-29-release-same-tree-reuse-design.md, D2).
func TestEngineVerificationStepsCompileTheSameForAPullRequestAndARelease(t *testing.T) {
	spec := engineManifest(t).Pipeline
	graph, _ := engineImportGraph(t)
	// Changing the manifest selects full coverage on a pull request, so its package
	// steps cover what a release's do.
	pull, refusal := compileEngineOpening(spec, graph, pipelines.EventPullRequest, []string{pipelines.ManifestPath})
	if refusal != nil {
		t.Fatal(refusal)
	}
	release, refusal := compileEngineOpening(spec, graph, pipelines.EventRelease, nil)
	if refusal != nil {
		t.Fatal(refusal)
	}
	fromPull := map[string]pipelines.Step{}
	for _, step := range pull.Steps() {
		fromPull[step.Key] = step
	}
	fromRelease := map[string]pipelines.Step{}
	for _, step := range release.Steps() {
		fromRelease[step.Key] = step
	}
	encoded := func(step pipelines.Step) string {
		t.Helper()
		// What the fingerprint leaves out (treeFingerprintOf).
		step.DependsOn, step.Skip = nil, nil
		body, err := json.Marshal(step)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	reusable := 0
	for key, step := range fromRelease {
		if !step.Verification {
			continue
		}
		reusable++
		// Pure by the compiler's contract (pipelines.validateVerification): nothing
		// here makes an artifact or runs natively on a machine.
		if len(step.Artifacts) > 0 || step.ImageBuild != nil || step.Placement != pipelines.PlacementCluster || step.Execution != pipelines.ExecutionContainer {
			t.Errorf("%s declares verification and is not a pure check: %+v", key, step)
		}
		other, ok := fromPull[key]
		if !ok {
			t.Errorf("%s is planned for a release and not for a pull request, so it can never be reused", key)
			continue
		}
		if a, b := encoded(step), encoded(other); a != b {
			t.Errorf("%s compiles differently for a release:\n%s\n%s", key, a, b)
		}
	}
	// The steps the decision was made for. Fewer means the saving shrank.
	if reusable < 27 {
		t.Errorf("a release can reuse %d steps; the design counted 27", reusable)
	}

	// The owner's decision and D4: these keep running in a release. A release-only
	// build has no pull request to stand on, and a scan's report is the release's own.
	for _, key := range []string{
		"vulnerabilities.go-vulnerabilities", "vulnerabilities.npm-vulnerabilities", "inventory.sbom",
		"checks.go-checks", "tests.fuzz", "tests.mcp-conformance", "cockpit.docker-integration", "cockpit.computeruse",
		"tests.os-checks", "images.bff", "cockpit-release.headless", "release-files.packages", "build-tools.toolchain",
	} {
		step, ok := fromRelease[key]
		if !ok {
			t.Errorf("the release plan no longer holds %s, so this test no longer says what it means", key)
			continue
		}
		if step.Verification {
			t.Errorf("%s may now be reused; the design keeps it running in the release", key)
		}
	}
}
