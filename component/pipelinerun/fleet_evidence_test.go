package pipelinerun

import (
	"context"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/znasllc-io/memql/component/pipelines"
)

// fleet_evidence_test.go -- a verification step may run Fleet first (the owner's
// decision, 2026-11-06). A pass the Fleet produced counts between pushes, a
// release does not stand on it, and a release runs its own pool steps on the
// cluster (executionStep). The rule a release's reuse and its candidate proof
// share is component/pipelines' fleet_verification_test.go.

// fleetMachine is the machine fleetAnswer says a pool step ran on.
const fleetMachine = "registration-cyberpc"

// fleetAnswer answers a step as the executor would: a request that still says
// pool ran on one of the owner's machines, anything else on the cluster.
// Artifact-free unless the step declares artifacts, as artifactFreeAnswer is.
func fleetAnswer(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
	result, err := artifactFreeAnswer(ctx, req)
	if req.Step.Placement == pipelines.PlacementPool {
		result.Where = pipelines.Where{Surface: pipelines.PlacementFleet, WorkerID: fleetMachine, MachineName: "CyberPC"}
	}
	return result, err
}

// poolStep makes the named step of manifest a pool step, which it must be once.
func poolStep(t *testing.T, manifest, name string) string {
	t.Helper()
	old := "        - name: " + name + "\n"
	at := strings.Index(manifest, old)
	if at < 0 {
		t.Fatalf("the manifest has no step %s", name)
	}
	rest := manifest[at:]
	placement := strings.Index(rest, "          placement: cluster\n")
	if placement < 0 {
		t.Fatalf("step %s declares no cluster placement", name)
	}
	return manifest[:at] + strings.Replace(rest, "          placement: cluster\n", "          placement: pool\n", 1)
}

// consentToTheFleet lets the harness's pipeline use the owner's machines, which
// a pool step requires (pipelines.Consent).
func (dh *driveHarness) consentToTheFleet() {
	dh.p.Compute = pipelines.ComputeClusterAndFleet
	dh.store.addPipeline(dh.p)
}

// lastSurface is where key's last attempt ran in one work run.
func (dh *driveHarness) lastSurface(t *testing.T, workRunID, key string) string {
	t.Helper()
	return pipelines.PassingAttemptSurface(dh.stepRowOf(t, workRunID, key))
}

// requestsFor is every request the runner was handed for key, in order.
func (dh *driveHarness) requestsFor(key string) []pipelines.StepRequest {
	var out []pipelines.StepRequest
	for _, req := range dh.exec.sent() {
		if req.StepKey == key {
			out = append(out, req)
		}
	}
	return out
}

// The rule the owner chose: a pass the Fleet produced is no evidence a release
// stands on. The push that tested the tree ran checks.unit on a machine and
// tests.integration on the cluster; the release reuses the second, runs the first
// itself, and runs it on the cluster. The plan keeps the step a pool step in both
// runs, so the fingerprints a reuse compares still agree.
func TestAReleaseDoesNotStandOnAPassTheFleetProduced(t *testing.T) {
	manifest := poolStep(t, sameTreeManifest, "unit")
	dh := newSameTreeHarness(t, manifest, admitEveryReuse)
	dh.consentToTheFleet()
	dh.exec.answer = fleetAnswer
	from := dh.driveSource(t, pushOpening())
	if got := dh.lastSurface(t, from.WorkRunID, releasedUnit); got != pipelines.PlacementFleet {
		t.Fatalf("the push ran %s on %q, want one of the owner's machines", releasedUnit, got)
	}
	if got := dh.lastSurface(t, from.WorkRunID, releasedIntegration); got != pipelines.PlacementCluster {
		t.Fatalf("the push ran %s on %q, want the cluster", releasedIntegration, got)
	}

	release, sent := dh.drive(t, releaseOpening())
	if release.Conclusion != ConclusionSuccess {
		t.Fatalf("the release did not succeed: %+v", release)
	}
	if want := []string{releasedUnit, releasedVet, releasedImage, releasedAdvisories}; !slices.Equal(sent, want) {
		t.Fatalf("the release executed %v, want %v: it must run the Fleet's pass again and reuse the cluster's", sent, want)
	}
	dh.requireReused(t, release, from, releasedIntegration)
	row := dh.stepRowOf(t, release.WorkRunID, releasedUnit)
	if row["status"] != WorkStepDone || argObject(row, "call")["evidenceRunId"] != nil {
		t.Fatalf("the release did not run %s itself: %v", releasedUnit, row)
	}
	if got := dh.lastSurface(t, release.WorkRunID, releasedUnit); got != pipelines.PlacementCluster {
		t.Fatalf("the release ran %s on %q, want the cluster", releasedUnit, got)
	}
	// One plan, two placements: the journal keeps the manifest's pool step, so
	// the release's tree fingerprint equals the push's.
	if a, b := argObject(row, "call")["treeFingerprint"], argObject(dh.stepRowOf(t, from.WorkRunID, releasedUnit), "call")["treeFingerprint"]; a == nil || a != b {
		t.Fatalf("the release's %s declares fingerprint %v, the push's %v", releasedUnit, a, b)
	}
}

// What the executor is handed: a push's pool step stays pool, so it is placed
// Fleet first; a release's is a cluster step, whatever the installation's cloud
// fallback says, as it was before verification steps could be pool.
func TestAReleaseRunsItsPoolStepsOnTheCluster(t *testing.T) {
	manifest := poolStep(t, sameTreeManifest, "unit")
	dh := newSameTreeHarness(t, manifest, admitEveryReuse)
	dh.consentToTheFleet()
	dh.exec.answer = fleetAnswer
	dh.driveSource(t, pushOpening())
	release, _ := dh.drive(t, releaseOpening())
	if release.Conclusion != ConclusionSuccess {
		t.Fatalf("the release did not succeed: %+v", release)
	}
	requests := dh.requestsFor(releasedUnit)
	if len(requests) != 2 {
		t.Fatalf("%s reached the runner %d times, want once for the push and once for the release", releasedUnit, len(requests))
	}
	if push := requests[0]; push.Event != pipelines.EventPush || push.Step.Placement != pipelines.PlacementPool {
		t.Fatalf("the push handed the runner %s as %q on %s, want a pool step", releasedUnit, push.Step.Placement, push.Event)
	}
	if rel := requests[1]; rel.Event != pipelines.EventRelease || rel.Step.Placement != pipelines.PlacementCluster || rel.Placement != nil {
		t.Fatalf("the release handed the runner %s as %q (attempt placement %+v) on %s, want a cluster step", releasedUnit, rel.Step.Placement, rel.Placement, rel.Event)
	}
	// Only the execution changes. The plan, which every fingerprint binds, keeps
	// the manifest's pool step; TestAReleaseDoesNotStandOnAPassTheFleetProduced
	// holds that through the tree fingerprints the push and the release declare.
}

// Between pushes a Fleet pass is evidence like any other: the owner's consented
// machines ran the step on the same inputs, and a release still executes it
// again. So a push carries tests.docs forward from the push before it, which ran
// it on a machine.
func TestAPushCarriesForwardAPassTheFleetProduced(t *testing.T) {
	manifest := poolStep(t, carryManifest, "docs")
	dh := newDriveHarness(t, manifest)
	dh.consentToTheFleet()
	dh.exec.answer = fleetAnswer
	a := workflowSource(t, carryAncestors)
	dh.integ.Configure(func(d *Deps) { bindTestWorkflow(d, a) })
	for _, sha := range []string{shaA, shaB} {
		dh.setTree(sha, manifest)
	}
	dh.github.mu.Lock()
	for _, sha := range []string{shaA, shaB} {
		tree := dh.github.trees[repoName+"@"+sha]
		tree["a/a.go"] = &fstest.MapFile{Data: []byte("package a\n")}
		tree["b/b.go"] = &fstest.MapFile{Data: []byte("package b\n")}
		tree["c/c.go"] = &fstest.MapFile{Data: []byte("package c\n\nimport _ \"acme.test/shop/a\"\n")}
	}
	dh.github.mu.Unlock()
	dh.setTimings(carryTimings)

	dh.at(0)
	source := dh.driveSource(t, pushOf(shaA, shaBase))
	if got := dh.lastSurface(t, source.WorkRunID, carryDocs); got != pipelines.PlacementFleet {
		t.Fatalf("the first push ran %s on %q, want one of the owner's machines", carryDocs, got)
	}
	dh.github.setAncestor(repoName, shaA, shaB, "b/b.go")
	dh.at(time.Hour)
	push, sent := dh.drive(t, pushOf(shaB, shaA))
	if push.Conclusion != ConclusionSuccess {
		t.Fatalf("the push concluded %q", push.Conclusion)
	}
	if slices.Contains(sent, carryDocs) {
		t.Fatalf("the push ran %s again instead of carrying the Fleet's pass: %v", carryDocs, sent)
	}
	dh.requireCarried(t, push, source, carryDocs, "b/b.go")
}
