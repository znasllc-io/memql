package main

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

func TestEnginePipelineValidatesTheShellDockerStage(t *testing.T) {
	spec := engineManifest(t).Pipeline
	graph, _ := engineImportGraph(t)
	for _, path := range []string{"Dockerfile", "clients/os/src/App.tsx", "sdk/ts/src/index.ts"} {
		plan, refusal := compileEngineOpening(spec, graph, pipelines.EventPullRequest, []string{path})
		if refusal != nil {
			t.Fatal(refusal)
		}
		found := false
		for _, step := range plan.Steps() {
			if step.Name != "os-image" {
				continue
			}
			found = true
			b := step.ImageBuild
			if step.Skip != nil || b == nil || b.Context != "." || b.Dockerfile != "Dockerfile" || b.Target != "spa-build" || step.RequiresFleet() || step.Run != "" || step.RunAfterFailure || len(step.Caches) != 0 || len(step.Secrets) != 0 || !slices.Equal(step.Artifacts, pipelines.ImageBuildArtifacts()) {
				t.Fatalf("%s lost isolated shell image validation: %+v", path, step)
			}
		}
		if !found {
			t.Fatalf("%s has no shell image validation", path)
		}
	}
}

func TestEnginePipelineRunsAndRetainsEditorPackaging(t *testing.T) {
	spec := engineManifest(t).Pipeline
	step, found := engineDeclaredStep(spec, "editor-packages")
	if !found {
		t.Fatal("editor packages have no validation")
	}
	for _, want := range []string{
		"go test -count=1 ./cmd/memql-lsp/...",
		"make vscode-test",
		"node editors/browser/build.mjs",
		"npm --prefix editors/browser run typecheck",
		"npm --prefix editors/browser test",
		"npm --prefix editors/productivity test",
		`scripts/ci/retry.sh -- bash scripts/vscode/package.sh --out="$PWD/.memql-editor/memql.vsix"`,
		"test -s .memql-editor/memql.vsix",
	} {
		if !slices.Contains(portableCommands(step.Run), want) {
			t.Errorf("editor validation omits %q", want)
		}
	}
	if !slices.Contains(step.Artifacts, ".memql-editor") || strings.Contains(step.Run, "|| true") || step.Execution == "native" {
		t.Fatal("editor package evidence lost")
	}
	graph, _ := engineImportGraph(t)
	for _, path := range []string{"editors/productivity/package.json", "editors/browser/build.mjs", "editors/vscode/package.json", "sdk/ts/src/index.ts", "component/compose/emaildoc/testdata/markdown-links.json"} {
		plan, refusal := compileEngineOpening(spec, graph, pipelines.EventPullRequest, []string{path})
		if refusal != nil {
			t.Fatal(refusal)
		}
		selected := false
		for _, s := range plan.Steps() {
			if s.Name == "editor-packages" {
				selected = s.Skip == nil
			}
		}
		if !selected {
			t.Errorf("editor consumer tests skip %s", path)
		}
	}
}

func TestPipelineDatabaseBudgetsAndFailurePropagation(t *testing.T) {
	output, err := exec.Command("python3", "-m", "unittest", "discover", "-s", "scripts/ci", "-p", "pipeline_db_tests_test.py").CombinedOutput()
	if err != nil {
		t.Fatalf("native DB budget runner: %v\n%s", err, output)
	}
	step, found := engineDeclaredStep(engineManifest(t).Pipeline, "db-tests")
	if !found || !slices.Contains(portableCommands(step.Run), "python3 scripts/ci/pipeline-db-tests.py") {
		t.Fatal("DB lane does not use the tested budget runner")
	}
}

func TestEditorSiteBrowserValidationSelectionAndIsolation(t *testing.T) {
	spec := engineManifest(t).Pipeline
	graph, _ := engineImportGraph(t)
	for _, event := range []pipelines.Event{pipelines.EventPullRequest, pipelines.EventPush, pipelines.EventMergeGroup} {
		for _, tc := range []struct {
			path string
			want bool
		}{
			{"editors/vscode/site/test-browser.mjs", true},
			{"editors/vscode/themes/memql-dark-color-theme.json", true},
			{"examples/research-desk/research/brief.memql", true},
			{"brand/tokens.css", true},
			{"scripts/ci/editor-site.sh", true},
			{"scripts/ci/retry.sh", true},
			{"memql-package.yaml", true},
			{"docs/public/overview/quickstart.md", false},
			{"component/grpc/server.go", false},
		} {
			plan, refusal := compileEngineOpening(spec, graph, event, []string{tc.path})
			if refusal != nil {
				t.Fatal(refusal)
			}
			found := false
			for _, step := range plan.Steps() {
				if step.Key != "tests.editor-site" {
					continue
				}
				found = true
				// PRs select affected inputs; push/queue qualification runs the
				// full functional suite, regardless of the changed path.
				want := tc.want || event != pipelines.EventPullRequest
				if (step.Skip == nil) != want {
					t.Errorf("%s %s: selected=%v, want %v", event, tc.path, step.Skip == nil, want)
				}
				if step.RequiresFleet() || len(step.Secrets) != 0 || len(step.Services) != 0 || step.ImageBuild != nil || step.RunAfterFailure || step.Run != "bash scripts/ci/editor-site.sh" || !slices.Equal(step.Artifacts, []string{"editors/vscode/site/dist", "editors/vscode/site/artifacts"}) {
					t.Fatal("site verification lost isolated browser execution or retained evidence")
				}
				if step.CPUMilli != 2000 || step.MemoryMiB != 4096 || step.TimeoutSeconds != 15*60 {
					t.Fatal("site verification lost its bounded resource request")
				}
			}
			if !found {
				t.Fatal("site browser check is absent")
			}
		}
	}
}
