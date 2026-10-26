package pipelines

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestImageVerificationCompilesWithExplicitPortableEvidence(t *testing.T) {
	for _, placement := range []string{PlacementCluster, PlacementFleet, PlacementPool} {
		t.Run(placement, func(t *testing.T) {
			spec := imageBuildSpec()
			spec.Platform = "linux/amd64"
			spec.Stages[0].Steps[0].Placement = placement
			spec.Stages[0].Steps[0].ImageBuild.VerificationOnly = true
			plan, err := Compile(spec, CompileInput{Mode: ModeFull, Event: EventMergeGroup, Compute: ComputeClusterAndFleet})
			if err != nil {
				t.Fatal(err)
			}
			step := plan.Steps()[0]
			wire, _ := json.Marshal(step)
			var received Step
			if json.Unmarshal(wire, &received) != nil || !received.ImageBuild.VerificationOnly || !reflect.DeepEqual(received.Artifacts, []string{ImageVerificationPath}) {
				t.Fatalf("lost verification semantics: %s", wire)
			}
			if placement == PlacementPool {
				if err := CheckPoolStep(received); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	for name, mutate := range map[string]func(*StepSpec){
		"source command":           func(s *StepSpec) { s.Run = "true" },
		"read secret":              func(s *StepSpec) { s.GitHubReadToken = true },
		"history":                  func(s *StepSpec) { s.CheckoutHistory = "full" },
		"wrong Fleet architecture": func(s *StepSpec) { s.Platform = "linux/arm64" },
		"source output":            func(s *StepSpec) { s.Artifacts = []string{"image.oci.tar"} },
		"native command":           func(s *StepSpec) { s.Execution = ExecutionNative },
	} {
		t.Run(name, func(t *testing.T) {
			spec := imageBuildSpec()
			spec.Platform = "linux/amd64"
			s := &spec.Stages[0].Steps[0]
			s.Placement = PlacementPool
			s.ImageBuild.VerificationOnly = true
			mutate(s)
			if r := Validate(spec); r == nil {
				t.Fatal("unsafe verification admitted")
			}
		})
	}
}

func TestImageVerificationReceiptBindsActualCompletedTarget(t *testing.T) {
	sha := strings.Repeat("a", 40)
	build := &ImageBuild{VerificationOnly: true, Context: ".", Dockerfile: "Dockerfile", Target: "verified", Args: map[string]string{"MEMQL_COMMIT": sha, "MEMQL_RELEASE": ""}}
	good := ImageVerificationReceipt{SchemaVersion: 1, Kind: "image-verification", SourceSHA: sha, Platform: "linux/amd64", Context: ".", Dockerfile: "Dockerfile", Target: "verified", Args: build.Args, SuccessfulBuild: true, RuntimeProfile: "rootless-buildkit-v1", OCISHA256: strings.Repeat("b", 64), OCIBytes: 1024, BuildMetadata: json.RawMessage(`{"containerimage.digest":"sha256:fixture"}`)}
	for name, mutate := range map[string]func(*ImageVerificationReceipt){
		"valid":            func(*ImageVerificationReceipt) {},
		"wrong source":     func(r *ImageVerificationReceipt) { r.SourceSHA = strings.Repeat("c", 40) },
		"wrong target":     func(r *ImageVerificationReceipt) { r.Target = "unverified" },
		"wrong platform":   func(r *ImageVerificationReceipt) { r.Platform = "linux/arm64" },
		"failed export":    func(r *ImageVerificationReceipt) { r.SuccessfulBuild = false },
		"wrong args":       func(r *ImageVerificationReceipt) { r.Args = map[string]string{} },
		"no output":        func(r *ImageVerificationReceipt) { r.OCIBytes = 0 },
		"malformed digest": func(r *ImageVerificationReceipt) { r.OCISHA256 = "image:latest" },
		"wrong runtime":    func(r *ImageVerificationReceipt) { r.RuntimeProfile = "host-docker" },
		"unknown metadata": func(r *ImageVerificationReceipt) { r.BuildMetadata = json.RawMessage(`null`) },
	} {
		t.Run(name, func(t *testing.T) {
			r := good
			mutate(&r)
			raw, _ := json.Marshal(r)
			err := ValidateImageVerificationReceipt(raw, sha, "linux/amd64", build)
			if (err == nil) != (name == "valid") {
				t.Fatal(err)
			}
		})
	}
	raw, _ := json.Marshal(good)
	for _, malformed := range [][]byte{append(append([]byte{}, raw...), []byte(` {}`)...), []byte(strings.Repeat(" ", ImageVerificationMaxBytes+1))} {
		if ValidateImageVerificationReceipt(malformed, sha, "linux/amd64", build) == nil {
			t.Fatal("unbounded or trailing receipt accepted")
		}
	}
}
