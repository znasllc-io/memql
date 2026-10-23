package main

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/pipelines"
)

// Bootstrap is a conservative execution of the reviewed pipeline, not a
// smaller test matrix. The old installed parser cannot read the new optional
// metadata. Compare the complete manifest after ONLY those declared changes.
func deferredDeliveryManifest(t *testing.T) *packages.Manifest {
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

func TestDeliveryBootstrapManifestPreservesEveryCheckAndNativeBoundary(t *testing.T) {
	expected := deferredDeliveryManifest(t)
	var stages []pipelines.StageSpec
	fullLanes, deferredReleases := 0, 0
	for _, stage := range expected.Pipeline.Stages {
		if stage.Name == "cockpit-release" {
			deferredReleases++
			continue
		}
		for n := range stage.Steps {
			step := &stage.Steps[n]
			step.Verification = false
			if stage.Name == "cockpit" || engineDeliveryHost(stage.Name, step.Name) {
				fullLanes++
				step.Packages, step.PackageRoots = "", nil
				if stage.Name == "cockpit" {
					step.Run = strings.ReplaceAll(step.Run, "$MEMQL_PACKAGES", "./cockpit/...")
				}
			}
		}
		stages = append(stages, stage)
	}
	expected.Pipeline.Stages = stages
	if fullLanes != 3 || deferredReleases != 1 {
		t.Fatalf("unexpected bootstrap adaptation: full lanes=%d, deferred release stages=%d", fullLanes, deferredReleases)
	}
	if !reflect.DeepEqual(expected, engineManifest(t)) {
		t.Fatal("bootstrap changed a command, check, selector, credential, artifact or execution boundary outside the declared conservative adaptation")
	}
}

func TestDeliveryBootstrapDeferredCockpitReleaseKeepsItsIsolationAndArtifacts(t *testing.T) {
	spec := deferredDeliveryManifest(t).Pipeline
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
			t.Errorf("deferred release changed its command or authority contract: %+v", step)
		}
		if engineCockpitHost(step.Stage, step.Name) {
			checkCockpitHost(t, step)
		} else if step.Placement != pipelines.PlacementCluster || step.Execution != pipelines.ExecutionContainer || step.Image != spec.Image || !slices.Equal(step.Caches, []string{"go"}) {
			t.Errorf("deferred release changed its isolated build contract: %+v", step)
		}
		artifacts := slices.Clone(step.Artifacts)
		slices.Sort(artifacts)
		if !slices.Equal(artifacts, []string{"cockpit/bin", "cockpit/dist", "cockpit/sbom"}) {
			t.Errorf("deferred release lost its component artifacts: %v", step.Artifacts)
		}
	}
	if !reflect.DeepEqual(seen, map[string]bool{"headless": true, "computeruse-darwin-arm64": true, "computeruse-darwin-amd64": true, "computeruse-linux-arm64": true, "computeruse-linux-amd64": true}) {
		t.Fatalf("deferred release changed its platform coverage: %v", seen)
	}
}
