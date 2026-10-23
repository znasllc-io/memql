package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

func engineDeliveryHost(stage, name string) bool {
	return stage == "delivery-artifacts" && name == "provider-qualification"
}

func checkDeliveryHost(t *testing.T, step pipelines.Step) {
	t.Helper()
	if step.Placement != pipelines.PlacementFleet || step.Execution != pipelines.ExecutionNative || step.Platform != "linux/amd64" || len(step.Caches) != 0 || len(step.Services) != 0 || len(step.Secrets) != 0 || step.GitHubReadToken || !slices.Equal(step.Needs, []string{"docker"}) || !slices.Equal(step.Artifacts, []string{".memql-artifact-evidence"}) {
		t.Errorf("artifact qualification lost its bounded native provider contract: %+v", step)
	}
}

func TestDeliveryArtifactsManifestBootstrapRunsAllProvidersAndUnknownChanges(t *testing.T) {
	spec := engineManifest(t).Pipeline
	step, ok := engineDeclaredStep(spec, "provider-qualification")
	if !ok || step.Packages != "" || len(step.PackageRoots) != 0 || !strings.Contains(step.Run, "bash scripts/ci/delivery-artifacts.sh") || strings.Count(step.Run, "@sha256:") != 4 || strings.Contains(step.Run, "$MEMQL_PACKAGES") {
		t.Fatalf("bootstrap provider qualification must run its full immutable fixture matrix: %+v", step)
	}
	graph, _ := engineImportGraph(t)
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"app/integrations_release.go", true},
		{"integrations/release/candidate_publish.go", true},
		{"integrations/ociregistry/publish.go", true},
		{"integrations/azureblob/verified_stream.go", true},
		{"integrations/pipelinesteps/runfiles.go", true},
		{"integrations/installation/selection.go", true},
		{"component/pipelinerun/driver.go", true},
		{"component/workjournal/journal.go", true},
		{"component/database/database.go", true},
		{"scripts/ci/delivery-artifacts.sh", true},
		{"scripts/ci/delivery_artifacts.py", true},
		{"scripts/ci/delivery_artifacts_test.py", true},
		{"scripts/ci/delivery_artifacts_test.go", true},
		{"scripts/lib/capability.sh", true},
		{"scripts/identity/build-css.sh", true},
		{"memql-package.yaml", true},
		// An unavailable change list or unsafe path cannot narrow coverage.
		{"", true},
		{"../unknown-source.go", true},
		{"cockpit/internal/access/cmd.go", true},
	} {
		changed := []string{tc.path}
		if tc.path == "" {
			changed = nil
		}
		plan, refusal := compileEngineOpening(spec, graph, pipelines.EventPullRequest, changed)
		if refusal != nil {
			t.Fatal(refusal)
		}
		found := false
		for _, compiled := range plan.Steps() {
			if !engineDeliveryHost(compiled.Stage, compiled.Name) {
				continue
			}
			found = true
			checkDeliveryHost(t, compiled)
			if (compiled.Skip == nil) != tc.want {
				t.Errorf("%s: provider qualification skip=%v packages=%v", tc.path, compiled.Skip, compiled.Packages)
			}
		}
		if !found {
			t.Fatal("provider qualification missing from compiled plan")
		}
	}
}
