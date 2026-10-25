package main

import (
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/pipelines"
)

// The retained fixture is the complete reviewed active contract. Exact parsed
// equality prevents activation from dropping a check, credential boundary or
// artifact declaration while preserving focused execution assertions below.
func reviewedDeliveryManifest(t *testing.T) *packages.Manifest {
	t.Helper()
	raw, err := os.ReadFile("test/fixtures/pipelines/delivery-after-bootstrap.yaml")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := packages.ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return expected
}

func TestActivatedDeliveryManifestMatchesReviewedContract(t *testing.T) {
	expected := reviewedDeliveryManifest(t)
	if !reflect.DeepEqual(expected, engineManifest(t)) {
		t.Fatal("active delivery manifest differs from its complete reviewed contract")
	}
}

func TestDeliveryCockpitReleaseKeepsItsIsolationAndArtifacts(t *testing.T) {
	spec := engineManifest(t).Pipeline
	graph, _ := engineImportGraph(t)
	plan, refusal := compileEngineOpening(spec, graph, pipelines.EventRelease, nil)
	if refusal != nil {
		t.Fatal(refusal)
	}
	seen := map[string]bool{}
	for _, step := range plan.Steps() {
		if step.Stage != "cockpit-release" {
			continue
		}
		seen[step.Name] = true
		if step.Kind != pipelines.StepCommand || step.Skip != nil || step.RunAfterFailure || len(step.Secrets) != 0 || step.GitHubReadToken || len(step.Services) != 0 {
			t.Errorf("Cockpit release changed its command or authority contract: %+v", step)
		}
		if engineCockpitHost(step.Stage, step.Name) {
			checkCockpitHost(t, step)
		} else if step.Placement != pipelines.PlacementCluster || step.Execution != pipelines.ExecutionContainer || step.Image != spec.Image || !slices.Equal(step.Caches, []string{"go"}) {
			t.Errorf("Cockpit release changed its isolated build contract: %+v", step)
		}
		artifacts := slices.Clone(step.Artifacts)
		slices.Sort(artifacts)
		if !slices.Equal(artifacts, []string{"cockpit/bin", "cockpit/dist", "cockpit/sbom"}) {
			t.Errorf("Cockpit release lost its component artifacts: %v", step.Artifacts)
		}
	}
	if !reflect.DeepEqual(seen, map[string]bool{"headless": true, "computeruse-darwin-arm64": true, "computeruse-darwin-amd64": true, "computeruse-linux-arm64": true, "computeruse-linux-amd64": true}) {
		t.Fatalf("Cockpit release changed its platform coverage: %v", seen)
	}
}
