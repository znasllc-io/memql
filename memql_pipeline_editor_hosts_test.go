package main

import (
	"maps"
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
	"gopkg.in/yaml.v3"
)

// Compare with the existing host matrix while both paths remain enabled. A
// portable unit/package lane cannot replace a desktop or browser host case.
func TestNativeEditorHostsCoverExistingMatrixAndChangedSources(t *testing.T) {
	raw, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var legacy struct {
		Jobs map[string]yaml.Node `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	type matrix struct {
		VSCode  []string                             `yaml:"vscode"`
		Include []struct{ OS, Host, Version string } `yaml:"include"`
	}
	readMatrix := func(name string) matrix {
		node, ok := legacy.Jobs[name]
		if !ok {
			t.Fatalf("legacy host job %q is missing", name)
		}
		var job struct {
			Strategy struct {
				Matrix matrix `yaml:"matrix"`
			} `yaml:"strategy"`
		}
		if err := node.Decode(&job); err != nil {
			t.Fatal(err)
		}
		return job.Strategy.Matrix
	}
	want := map[string]bool{}
	for _, version := range readMatrix("vscode-extension-host").VSCode {
		want["vscode-desktop/linux/amd64/"+version] = true
	}
	for _, item := range readMatrix("productivity-extension-host").Include {
		platform := map[string]string{"ubuntu-latest": "linux/amd64", "macos-latest": "darwin/arm64"}[item.OS]
		if platform == "" {
			t.Fatalf("unported legacy host platform %q", item.OS)
		}
		want["productivity-"+item.Host+"/"+platform+"/"+item.Version] = true
	}
	if len(want) != 8 {
		t.Fatalf("legacy host matrix changed: %v", want)
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
