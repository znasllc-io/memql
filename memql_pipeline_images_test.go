package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

// Read the actual node build files, so retiring or adding a node cannot leave
// a successful release with a stale or missing engine image.
func TestEnginePipelineBuildsEveryReleaseImage(t *testing.T) {
	files, err := filepath.Glob("app/build_*.go")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, file := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file), "build_"), ".go")
		if name != "default" && !strings.HasSuffix(name, "_test") {
			want[name] = true
		}
	}
	if len(want) == 0 {
		t.Fatal("no engine build files")
	}
	spec := engineManifest(t).Pipeline
	graph, _ := engineImportGraph(t)
	for _, event := range []pipelines.Event{pipelines.EventRelease, pipelines.EventPullRequest, pipelines.EventPush, pipelines.EventMergeGroup, pipelines.EventManual, pipelines.EventSchedule} {
		plan, refusal := compileEngineOpening(spec, graph, event, nil)
		if refusal != nil {
			t.Fatal(refusal)
		}
		got := map[string]bool{}
		for _, step := range plan.Steps() {
			if step.ImageBuild == nil || step.Stage != "images" {
				continue
			}
			if event != pipelines.EventRelease {
				t.Fatalf("%s unexpectedly builds release images", event)
			}
			build := step.ImageBuild
			if !want[step.Name] || got[step.Name] || step.Key != "images."+step.Name || build.Args["BUILD_TAGS"] != step.Name {
				t.Fatalf("release image does not match a unique node: %+v", step)
			}
			got[step.Name] = true
			target, spa := "runtime", "spa-skip"
			if step.Name == "workbench" {
				target = "workbench-runtime"
			}
			if step.Name == "edge" {
				spa = "spa-build"
			}
			if build.Context != "." || build.Dockerfile != "Dockerfile" || build.Target != target || build.Args["SPA_DIST_STAGE"] != spa {
				t.Fatalf("%s lost its engine/runtime/site build mapping: %+v", step.Key, build)
			}
			if step.RunAfterFailure || step.Skip != nil || step.RequiresFleet() || len(step.Services) != 0 || len(step.Caches) != 0 || len(step.Secrets) != 0 || step.Image != "" || step.Run != "" {
				t.Fatalf("%s bypasses release validation or carries execution/publication authority: %+v", step.Key, step)
			}
			if !slices.Equal(step.Artifacts, pipelines.ImageBuildArtifacts()) || step.CPUMilli != 3000 || step.MemoryMiB != 8192 || step.TimeoutSeconds != 90*60 || step.Platform != "linux/amd64" {
				t.Fatalf("%s lost its retained provenance or capacity bound: %+v", step.Key, step)
			}
			if build.Args["GOFLAGS"] != "-p=2" || build.Args["GOMEMLIMIT"] != "1GiB" || build.Args["GOGC"] != "75" {
				t.Fatalf("%s lost the compiler bound", step.Key)
			}
		}
		if event == pipelines.EventRelease && len(got) != len(want) {
			t.Fatalf("release builds %v, node files declare %v", got, want)
		}
	}
	dockerfile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"GOFLAGS", "GOMEMLIMIT", "GOGC"} {
		if !strings.Contains(string(dockerfile), "\nARG "+name+"\n") {
			t.Errorf("builder does not accept optional %s", name)
		}
	}
}

func TestToolchainBuildIsReleaseOnlyAndRetainsUnpublishedOCI(t *testing.T) {
	spec := engineManifest(t).Pipeline
	graph, _ := engineImportGraph(t)
	for _, event := range []pipelines.Event{pipelines.EventRelease, pipelines.EventPullRequest, pipelines.EventPush, pipelines.EventMergeGroup, pipelines.EventManual, pipelines.EventSchedule} {
		plan, refusal := compileEngineOpening(spec, graph, event, nil)
		if refusal != nil {
			t.Fatal(refusal)
		}
		found := 0
		for _, step := range plan.Steps() {
			if step.Stage != "build-tools" {
				continue
			}
			found++
			if step.Key != "build-tools.toolchain" || step.ImageBuild == nil || step.ImageBuild.Context != "." || step.ImageBuild.Dockerfile != "deploy/toolchain-image/Dockerfile" || step.ImageBuild.Target != "verified" || len(step.ImageBuild.Args) != 0 {
				t.Fatalf("toolchain build lost its committed Dockerfile: %+v", step)
			}
			if step.RunAfterFailure || step.Skip != nil || step.RequiresFleet() || len(step.Services) != 0 || len(step.Caches) != 0 || len(step.Secrets) != 0 || step.Run != "" || step.Image != "" {
				t.Fatalf("toolchain build bypasses qualification or carries publication authority: %+v", step)
			}
			if !slices.Equal(step.Artifacts, pipelines.ImageBuildArtifacts()) || step.CPUMilli != 3000 || step.MemoryMiB != 8192 || step.TimeoutSeconds != 45*60 || step.Platform != "linux/amd64" {
				t.Fatalf("toolchain build lost provenance or capacity bounds: %+v", step)
			}
		}
		if (event == pipelines.EventRelease && found != 1) || (event != pipelines.EventRelease && found != 0) {
			t.Fatalf("%s selected %d toolchain builds", event, found)
		}
	}
}

func TestDeployGateImageFollowsBuildInputsAndStaysUnpublished(t *testing.T) {
	spec := engineManifest(t).Pipeline
	graph, _ := engineImportGraph(t)
	for _, tc := range []struct {
		path string
		runs bool
	}{
		{"cmd/deploy-gate-check/main.go", true},
		{"cmd/deploy-gate-check/Dockerfile", true},
		{"component/grpc/gen/memql.pb.go", true},
		{"core/id/id.go", true},
		{"component/grpc/gen/go.mod", true},
		{"go.mod", true},
		{"go.sum", true},
		{"go.work", true},
		{"memql-package.yaml", true},
		{"docs/public/overview/quickstart.md", false},
		{"clients/os/src/main.tsx", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			plan, refusal := compileEngineOpening(spec, graph, pipelines.EventPullRequest, []string{tc.path})
			if refusal != nil {
				t.Fatal(refusal)
			}
			for _, step := range plan.Steps() {
				if step.Key != "tests.deploy-gate-image" {
					continue
				}
				if (step.Skip == nil) != tc.runs {
					t.Fatalf("build selected=%v for %s, want %v", step.Skip == nil, tc.path, tc.runs)
				}
				build := step.ImageBuild
				if build == nil || build.Context != "." || build.Dockerfile != "cmd/deploy-gate-check/Dockerfile" {
					t.Fatalf("build does not exercise the deployed gate Dockerfile: %+v", build)
				}
				if step.RequiresFleet() || len(step.Secrets) != 0 || len(step.Caches) != 0 || step.Run != "" || step.RunAfterFailure || !slices.Equal(step.Artifacts, pipelines.ImageBuildArtifacts()) {
					t.Fatalf("build lost its isolated unpublished OCI contract: %+v", step)
				}
				return
			}
			t.Fatal("deploy-gate image validation is absent")
		})
	}
}

func TestDatabaseImageBuildRunsTheVerifiedOperandTarget(t *testing.T) {
	spec := engineManifest(t).Pipeline
	graph, _ := engineImportGraph(t)
	for _, tc := range []struct {
		path string
		runs bool
	}{
		{"deploy/db-image/Dockerfile", true},
		{"deploy/db-image/smoke-in-container.sh", true},
		{"deploy/db-image/smoke-test.sh", true},
		{"scripts/db-image/build.sh", true},
		{".github/workflows/build-db-image.yml", true},
		{"memql-package.yaml", true},
		{"docs/public/overview/quickstart.md", false},
		{"clients/os/src/main.tsx", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			plan, refusal := compileEngineOpening(spec, graph, pipelines.EventPullRequest, []string{tc.path})
			if refusal != nil {
				t.Fatal(refusal)
			}
			for _, step := range plan.Steps() {
				if step.Key != "tests.database-image" {
					continue
				}
				if (step.Skip == nil) != tc.runs {
					t.Fatalf("database validation selected=%v for %s, want %v", step.Skip == nil, tc.path, tc.runs)
				}
				build := step.ImageBuild
				if build == nil || build.Context != "deploy/db-image" || build.Dockerfile != "deploy/db-image/Dockerfile" || build.Target != "verified" {
					t.Fatalf("database build does not require the operand smoke target: %+v", build)
				}
				if step.RequiresFleet() || len(step.Secrets) != 0 || len(step.Caches) != 0 || step.Run != "" || step.RunAfterFailure || !slices.Equal(step.Artifacts, pipelines.ImageBuildArtifacts()) {
					t.Fatalf("database build lost its isolated unpublished OCI contract: %+v", step)
				}
				return
			}
			t.Fatal("native database image validation is absent")
		})
	}
}
