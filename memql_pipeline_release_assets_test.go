package main

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

func TestEnginePipelineRetainsVerifiedReleasePackages(t *testing.T) {
	spec := engineManifest(t).Pipeline
	graph, _ := engineImportGraph(t)
	for _, event := range []pipelines.Event{pipelines.EventRelease, pipelines.EventPullRequest, pipelines.EventMergeGroup, pipelines.EventPush, pipelines.EventSchedule} {
		plan, refusal := compileEngineOpening(spec, graph, event, nil)
		if refusal != nil {
			t.Fatal(refusal)
		}
		found := 0
		for _, step := range plan.Steps() {
			if step.Stage != "release-files" {
				continue
			}
			found++
			if step.Key != "release-files.packages" || step.RunAfterFailure || step.Skip != nil || step.RequiresFleet() || len(step.Secrets) != 0 || !slices.Contains(step.Artifacts, ".memql-release") || !strings.Contains(step.Run, "python3 scripts/ci/pipeline-release-assets.py") {
				t.Fatal("release file step lost source, retained output or authority boundaries", step)
			}
		}
		if (event == pipelines.EventRelease && found != 1) || (event != pipelines.EventRelease && found != 0) {
			t.Fatal("release file builds have incorrect event coverage", event, found)
		}
	}
	output, err := exec.Command("python3", "-m", "unittest", "discover", "-s", "scripts/ci", "-p", "pipeline_release_assets_test.py").CombinedOutput()
	if err != nil {
		t.Fatalf("release file validation: %v\n%s", err, output)
	}
}
