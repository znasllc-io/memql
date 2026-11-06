package pipelines

import (
	"strings"
	"testing"
)

// fleet_verification_test.go -- a verification step may run Fleet first (the
// owner's decision, 2026-11-06), and a release stands only on a pass the cluster
// produced. The driver half (a release runs its pool steps on the cluster, a push
// carries a Fleet pass forward) is component/pipelinerun's
// fleet_evidence_test.go.

// A verification step compiles on the pool, for a push and a release alike, and
// keeps its pool placement in both plans: the plan is what every fingerprint
// binds, so the two must agree for a release to reuse a push's pass. A fleet step
// is still refused: it is native or needs a host, so its pass is not a function
// of its image.
func TestAVerificationStepMayBeAPoolStep(t *testing.T) {
	image := "registry/toolchain@sha256:" + strings.Repeat("a", 64)
	spec := &Spec{Image: image, Platform: "linux/amd64", Stages: []StageSpec{{Name: "checks", Steps: []StepSpec{
		{Name: "unit", Run: "go test ./unit", Verification: true, Placement: PlacementPool, MemoryMiB: 4096},
	}}}}
	for _, event := range []Event{EventPush, EventRelease} {
		plan, r := Compile(spec, CompileInput{Mode: ModeFull, Event: event, Compute: ComputeClusterAndFleet})
		if r != nil {
			t.Fatalf("%s: a pool verification step was refused: %v", event, r)
		}
		step := plan.Steps()[0]
		if !step.Verification || step.Placement != PlacementPool {
			t.Fatalf("%s: the plan changed the step's contract: %+v", event, step)
		}
	}
	unit := spec.Stages[0].Steps[0]
	if r := validateVerification(spec, unit, "checks/unit"); r != nil {
		t.Fatal(r)
	}
	unit.Placement = PlacementFleet
	if r := validateVerification(spec, unit, "checks/unit"); r == nil {
		t.Fatal("a fleet verification step was admitted")
	}
}

// withAttempts records the step's attempts as the driver's binding keeps them
// (component/pipelinerun, attemptRecord.asMap), one per surface, the last the one
// that passed.
func withAttempts(step map[string]any, surfaces ...string) map[string]any {
	attempts := make([]any, 0, len(surfaces))
	for i, surface := range surfaces {
		attempts = append(attempts, map[string]any{"attempt": i + 1, "surface": surface})
	}
	step["binding"] = map[string]any{"attempts": attempts}
	return step
}

// A release reuses a pass only when the cluster produced it: the attempt that
// passed, which is the last one. A row that records no attempt comes from an
// engine whose verification steps ran on the cluster only, so a candidate
// assembled then stays provable.
func TestAReleaseReusesOnlyAPassTheClusterProduced(t *testing.T) {
	for name, tc := range map[string]struct {
		surfaces []string
		record   bool
		admitted bool
	}{
		"an engine that recorded no attempt":            {record: false, admitted: true},
		"a pass on the cluster":                         {record: true, surfaces: []string{PlacementCluster}, admitted: true},
		"the fleet had no machine, the cluster passed":  {record: true, surfaces: []string{PlacementFleet, PlacementCluster}, admitted: true},
		"a pass on one of the owner's machines":         {record: true, surfaces: []string{PlacementFleet}, admitted: false},
		"the cluster failed, then a machine passed":     {record: true, surfaces: []string{PlacementCluster, PlacementFleet}, admitted: false},
		"a machine failed, another machine then passed": {record: true, surfaces: []string{PlacementFleet, PlacementFleet}, admitted: false},
	} {
		t.Run(name, func(t *testing.T) {
			r, s := reusedSource()
			if tc.record {
				withAttempts(s, tc.surfaces...)
			}
			_, err := InspectReusedReleaseReceipt("owner", "run", "test.full", r, s, reusedSourceRun())
			if tc.admitted && err != nil {
				t.Fatalf("a pass the cluster produced was refused: %v", err)
			}
			if !tc.admitted && (err == nil || !strings.Contains(err.Error(), "produced on the cluster")) {
				t.Fatalf("a pass the Fleet produced was admitted, or refused for another reason: %v", err)
			}
		})
	}
}

// Where the pass ran is no part of its receipt's identity: one step receipt has
// one digest, so a candidate that already reused a cluster pass is proved the
// same way after the rule, and the release digest of a Fleet pass is unchanged.
func TestWhereAPassRanIsNoPartOfItsDigest(t *testing.T) {
	r, plain := reusedSource()
	_, recorded := reusedSource()
	withAttempts(recorded, PlacementFleet)
	a, err := InspectReleaseStepReceipt("owner", "run", "test.full", r, plain)
	if err != nil {
		t.Fatal(err)
	}
	b, err := InspectReleaseStepReceipt("owner", "run", "test.full", r, recorded)
	if err != nil {
		t.Fatal(err)
	}
	if a.ReceiptDigest != b.ReceiptDigest {
		t.Fatalf("the attempt record changed the receipt's digest: %s and %s", a.ReceiptDigest, b.ReceiptDigest)
	}
}

// The candidate proof is the runner's rule (ProveReusedStep calls
// InspectReusedReleaseReceipt), so a proof never accepts a skip the runner would
// not have planned: a Fleet pass named as evidence refuses the candidate.
func TestTheReleaseProofRefusesAPassTheFleetProduced(t *testing.T) {
	c := newReusedProofCase()
	withAttempts(c.sourceStep, PlacementCluster)
	if _, err := c.prove(); err != nil {
		t.Fatalf("the proof refused a cluster pass: %v", err)
	}
	withAttempts(c.sourceStep, PlacementFleet)
	if _, err := c.prove(); err == nil || !strings.Contains(err.Error(), "produced on the cluster") {
		t.Fatalf("the proof accepted a pass the Fleet produced: %v", err)
	}
}

func TestPassingAttemptSurfaceReadsTheLastAttempt(t *testing.T) {
	for name, tc := range map[string]struct {
		step map[string]any
		want string
	}{
		"no binding":        {map[string]any{}, ""},
		"no attempts":       {map[string]any{"binding": map[string]any{}}, ""},
		"an empty list":     {map[string]any{"binding": map[string]any{"attempts": []any{}}}, ""},
		"decoded from JSON": {withAttempts(map[string]any{}, PlacementFleet, PlacementCluster), PlacementCluster},
		"typed": {map[string]any{"binding": map[string]any{"attempts": []map[string]any{
			{"attempt": 1, "surface": PlacementCluster}, {"attempt": 2, "surface": PlacementFleet},
		}}}, PlacementFleet},
	} {
		if got := PassingAttemptSurface(tc.step); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
