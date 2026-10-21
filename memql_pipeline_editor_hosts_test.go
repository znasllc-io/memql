package main

import (
	"maps"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

// Keep both supported desktop versions and both host types on their real
// platforms. A portable unit/package lane cannot replace a host case.
func TestNativeEditorHostsCoverSupportedMatrixAndChangedSources(t *testing.T) {
	want := map[string]bool{}
	for _, version := range []string{"1.91.0", "stable"} {
		want["vscode-desktop/linux/amd64/"+version] = true
		for _, platform := range []string{"linux/amd64", "darwin/arm64"} {
			want["productivity-desktop/"+platform+"/"+version] = true
		}
	}
	for _, platform := range []string{"linux/amd64", "darwin/arm64"} {
		want["productivity-web/"+platform+"/stable"] = true
	}
	spec := engineManifest(t).Pipeline
	graph, _ := engineImportGraph(t)
	for _, tc := range []struct {
		name    string
		event   pipelines.Event
		changed []string
		run     bool
	}{
		{"editor source", pipelines.EventPullRequest, []string{"editors/productivity/test/host.ts"}, true},
		{"runner source", pipelines.EventPullRequest, []string{"scripts/ci/editor-host.sh"}, true},
		{"shared host profile", pipelines.EventPullRequest, []string{"scripts/vscode/host-profile.cjs"}, true},
		{"manifest", pipelines.EventPullRequest, []string{"memql-package.yaml"}, true},
		{"documentation", pipelines.EventPullRequest, []string{"docs/public/overview/quickstart.md"}, false},
		{"exact merge candidate", pipelines.EventMergeGroup, nil, true},
		{"release", pipelines.EventRelease, nil, true},
		{"daily audit", pipelines.EventSchedule, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, refusal := compileEngineOpening(spec, graph, tc.event, tc.changed)
			if refusal != nil {
				t.Fatal(refusal)
			}
			got := map[string]bool{}
			for _, step := range plan.Steps() {
				if !strings.HasPrefix(step.Run, "bash scripts/ci/editor-host.sh ") || step.Skip != nil {
					continue
				}
				args := strings.Fields(step.Run)
				if len(args) != 4 || step.TimeoutSeconds != 25*60 || len(step.Artifacts) != 1 || step.Artifacts[0] != ".memql-host-reports" {
					t.Fatalf("host case lost its execution or retained diagnostics contract: %+v", step)
				}
				key := args[2] + "/" + step.Platform + "/" + args[3]
				if got[key] {
					t.Fatalf("host case is duplicated: %s", key)
				}
				got[key] = true
			}
			if tc.run && !maps.Equal(got, want) || !tc.run && len(got) != 0 {
				t.Fatalf("host selection = %v; expected matrix=%v, selected=%v", got, want, tc.run)
			}
		})
	}
}
