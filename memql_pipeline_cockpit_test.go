package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

func engineCockpitHost(stage, name string) bool {
	return stage == "cockpit" && slices.Contains([]string{"docker-integration", "computeruse"}, name) || stage == "cockpit-release" && slices.Contains([]string{"computeruse-darwin-arm64", "computeruse-darwin-amd64"}, name)
}

func checkCockpitHost(t *testing.T, step pipelines.Step) {
	t.Helper()
	platform, need := "darwin/arm64", "macos_tooling"
	if step.Stage == "cockpit" && step.Name == "docker-integration" {
		platform, need = "linux/amd64", "docker"
	}
	if step.Placement != pipelines.PlacementFleet || step.Execution != pipelines.ExecutionNative || step.Platform != platform || len(step.Caches) != 0 || len(step.Services) != 0 || !slices.Equal(step.Needs, []string{need}) {
		t.Errorf("Cockpit host lost its exact platform and isolation contract: %+v", step)
	}
	if step.Stage == "cockpit-release" && !slices.Equal(step.Artifacts, []string{"cockpit/bin", "cockpit/dist", "cockpit/sbom"}) {
		t.Errorf("Cockpit release lost independent artifacts: %+v", step)
	}
}

func TestCockpitManifestUsesAffectedGraphWithoutDuplicateUnitLane(t *testing.T) {
	spec := engineManifest(t).Pipeline
	step, ok := engineDeclaredStep(spec, "docker-integration")
	if !ok || step.Packages != pipelines.PackagesAffected || !slices.Equal(step.PackageRoots, []string{"cockpit"}) || !strings.Contains(step.Run, "go test -race -count=1 -timeout=600s $MEMQL_PACKAGES") {
		t.Fatalf("Docker lane must intersect the affected graph with Cockpit: %+v", step)
	}
	mac, ok := engineDeclaredStep(spec, "computeruse")
	if !ok || mac.Packages != pipelines.PackagesAffected || !slices.Equal(mac.PackageRoots, []string{"cockpit"}) || mac.Platform != "darwin/arm64" || !strings.Contains(mac.Run, "go test -race -tags computeruse -count=1 -timeout=600s $MEMQL_PACKAGES") || !strings.Contains(mac.Run, `GOWORK="$PWD/go.work"`) {
		t.Fatalf("Mac lane must execute affected Cockpit packages in the checked-out workspace: %+v", mac)
	}
	for _, stage := range spec.Stages {
		if stage.Name == "cockpit" && len(stage.Steps) != 2 {
			t.Fatalf("generic Go lanes already own build/vet/unit checks: %v", stage.Steps)
		}
	}
	graph, _ := engineImportGraph(t)
	// The imported Cockpit module must participate in the actual repository
	// graph, including the engine dependencies its source imports.
	for _, tc := range []struct {
		path string
		want bool
	}{
		// Documentation is embedded into engine libraries consumed by Cockpit.
		{"docs/public/overview/quickstart.md", true},
		{"component/pipelines/compile.go", false},
		{"cockpit/internal/access/cmd.go", true},
		{"core/id/id.go", true},
		{"memql-package.yaml", true},
	} {
		plan, refusal := compileEngineOpening(spec, graph, pipelines.EventPullRequest, []string{tc.path})
		if refusal != nil {
			t.Fatal(refusal)
		}
		for _, compiled := range plan.Steps() {
			if compiled.Stage != "cockpit" {
				continue
			}
			if (compiled.Skip == nil) != tc.want {
				t.Errorf("%s: Docker lane skip = %v, packages = %v", tc.path, compiled.Skip, compiled.Packages)
			}
			for _, pkg := range compiled.Packages {
				if !strings.HasPrefix(pkg, "github.com/znasllc-io/memql/cockpit/") {
					t.Errorf("Docker lane included unrelated package %s", pkg)
				}
			}
		}
	}
}
