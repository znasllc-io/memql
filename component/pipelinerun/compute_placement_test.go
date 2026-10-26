package pipelinerun

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

func TestComputePlacementUsesCapturedPolicyAcrossReplicas(t *testing.T) {
	a := workflowSource(t, `@template
automation pipelineComputePlacementPolicy {
 args { repository string! pipelineId string! ownerUserId string! event string! }
 return {fleetWaitSeconds: 60, cloudFallback: true}
}`)
	d := Deps{}
	bindTestWorkflow(&d, a)
	p := testPipeline(DeliveryWebhook)
	snapshot, err := capturePipelinePolicy(context.Background(), d, p, prOpening())
	if err != nil {
		t.Fatal(err)
	}
	// The second replica cannot load the first replica's installed definitions.
	// Its only source of placement policy is the run's durable snapshot.
	d.WorkflowSource = func(string, string) (string, error) { return "", errors.New("bundle changed") }
	dr := &runDriver{d: d, run: Run{PolicySnapshot: snapshot.Map(), Repository: p.Repository,
		PipelineID: p.ID, OwnerUserID: p.OwnerUserID, Event: pipelines.EventPullRequest}}
	if err := dr.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	policy, err := dr.selectComputePlacement(context.Background())
	if err != nil || policy == nil || policy.FleetWaitSeconds != 60 || !policy.CloudFallback {
		t.Fatalf("captured placement lost: %+v %v", policy, err)
	}
	dr.computePlacement = policy
	track := &stepTrack{step: pipelines.Step{Placement: pipelines.PlacementPool}}
	request := dr.request(track, nil)
	request.ComputePlacement.CloudFallback = false
	if !dr.request(track, nil).ComputePlacement.CloudFallback {
		t.Fatal("one executor changed another step's policy")
	}
	track.step.Placement = pipelines.PlacementCluster
	if dr.request(track, nil).ComputePlacement != nil {
		t.Fatal("policy leaked into a dedicated step")
	}
	old := &runDriver{}
	if policy, err := old.selectComputePlacement(context.Background()); err != nil || policy.CloudFallback {
		t.Fatalf("legacy snapshot gained cloud permission: %+v %v", policy, err)
	}
}

func TestComputePlacementRejectsUnboundedOrAmbiguousPolicy(t *testing.T) {
	for _, value := range []any{
		nil, true, map[string]any{},
		map[string]any{"fleetWaitSeconds": 60},
		map[string]any{"fleetWaitSeconds": -1, "cloudFallback": true},
		map[string]any{"fleetWaitSeconds": 3601, "cloudFallback": true},
		map[string]any{"fleetWaitSeconds": 1.5, "cloudFallback": true},
		map[string]any{"fleetWaitSeconds": 1e99, "cloudFallback": true},
		map[string]any{"fleetWaitSeconds": 60, "cloudFallback": "true"},
		map[string]any{"fleetWaitSeconds": 60, "cloudFallback": nil},
		map[string]any{"fleetWaitSeconds": 60, "cloudFallback": true, "machineId": "untrusted"},
	} {
		if got, err := parseComputePlacement(value); err == nil || got != nil {
			t.Fatalf("accepted %+v", value)
		}
	}
	for _, seconds := range []int{0, 3600} {
		if got, err := parseComputePlacement(map[string]any{"fleetWaitSeconds": seconds, "cloudFallback": false}); err != nil || got.FleetWaitSeconds != seconds {
			t.Fatalf("valid boundary refused: %+v %v", got, err)
		}
	}
}

func TestComputePlacementDecisionIsPartOfPoolRecoveryDefinition(t *testing.T) {
	dr := &runDriver{d: Deps{EngineRevision: func() string { return "immutable-engine" }},
		p: testPipeline(DeliveryWebhook), computePlacement: &pipelines.ComputePlacementPolicy{FleetWaitSeconds: 60, CloudFallback: true}}
	pool := pipelines.Step{Key: "checks.vet", Placement: pipelines.PlacementPool}
	dedicated := pool
	dedicated.Placement = pipelines.PlacementCluster
	poolBefore, dedicatedBefore := dr.definitionOf(pool), dr.definitionOf(dedicated)
	dr.computePlacement.CloudFallback = false
	if poolBefore == "" || poolBefore == dr.definitionOf(pool) {
		t.Fatal("changed placement retained recovery permission")
	}
	if dedicatedBefore == "" || dedicatedBefore != dr.definitionOf(dedicated) {
		t.Fatal("pool policy changed a dedicated step definition")
	}
}

func TestComputePlacementDriverBindsPolicyBeforePoolEffects(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "captured policy", true: "invalid policy"}[invalid], func(t *testing.T) {
			manifest := strings.Replace(driveManifest, "          run: go vet ./...", "          placement: pool\n          platform: linux/amd64\n          run: go vet ./...", 1)
			dh := newDriveHarness(t, manifest)
			dh.p.Compute = pipelines.ComputeClusterAndFleet
			dh.store.addPipeline(dh.p)
			result := "{fleetWaitSeconds: 60, cloudFallback: true}"
			if invalid {
				result = "{fleetWaitSeconds: 3601, cloudFallback: true}"
			}
			a := workflowSource(t, "@template\nautomation pipelineComputePlacementPolicy { args { repository string! pipelineId string! ownerUserId string! event string! } return "+result+" }")
			dh.integ.Configure(func(d *Deps) { bindTestWorkflow(d, a) })
			run := dh.openRun(t, prOpening())
			deliver(t, dh.integ, run)
			got, _ := dh.store.run(run.ID)
			if invalid {
				if got.Conclusion == ConclusionSuccess || len(dh.exec.sent()) != 0 || got.WorkRunID != "" {
					t.Fatalf("invalid policy reached effects: %+v", got)
				}
				return
			}
			if got.Conclusion != ConclusionSuccess {
				t.Fatalf("pool run failed: %+v", got)
			}
			found := false
			for _, req := range dh.exec.sent() {
				if req.Step.Placement != pipelines.PlacementPool {
					if req.ComputePlacement != nil {
						t.Fatal("dedicated step acquired preference policy")
					}
					continue
				}
				found = true
				if req.ComputePlacement == nil || !req.ComputePlacement.CloudFallback || req.ComputePlacement.FleetWaitSeconds != 60 {
					t.Fatalf("pool request lost policy: %+v", req)
				}
			}
			if !found {
				t.Fatal("pool step did not reach executor")
			}
		})
	}
}
