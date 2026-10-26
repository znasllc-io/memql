//go:build agent

package pipelinesteps

import (
	"context"
	"encoding/json"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	pl "github.com/znasllc-io/memql/component/pipelines"
	worker "github.com/znasllc-io/memql/integrations/agent/worker"
	"github.com/znasllc-io/memql/integrations/workbench"
	"reflect"
	"strings"
	"testing"
)

func verificationRequest() pl.StepRequest {
	req := poolRequest()
	req.Secrets = nil
	req.Step = pl.Step{Kind: pl.StepCommand, Key: req.StepKey, Stage: "tests", Name: "image", Placement: pl.PlacementPool, Execution: pl.ExecutionContainer, Platform: "linux/amd64", MemoryMiB: 1024, CPUMilli: 1000, TimeoutSeconds: 900, ImageBuild: &pl.ImageBuild{VerificationOnly: true, Context: ".", Dockerfile: "Dockerfile", Target: "verified"}}
	req.Step.Artifacts = pl.ImageBuildArtifactsFor(req.Step.ImageBuild)
	return req
}

func TestImageVerificationFleetDispatchAndArtifactRoundTrip(t *testing.T) {
	req := verificationRequest()
	run := fleetRun(req)
	receipt := pl.ImageVerificationReceipt{SchemaVersion: 1, Kind: "image-verification", SourceSHA: req.SHA, Platform: run.Platform, Context: run.ImageBuild.Context, Dockerfile: run.ImageBuild.Dockerfile, Target: run.ImageBuild.Target, Args: run.ImageBuild.Args, SuccessfulBuild: true, RuntimeProfile: ImageBuilderRootlessV1, OCISHA256: strings.Repeat("a", 64), OCIBytes: 256, BuildMetadata: json.RawMessage(`{}`)}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDispatcher{answer: fleetArtifactAnswer(extractTestTgz(t, []extractTestEntry{{name: pl.ImageVerificationPath, body: string(raw)}}))}
	f, _, _, _ := newTestFleet(t, d)
	result, err := f.RunStep(context.Background(), req, run)
	if err != nil || result.Status != pl.OutcomeSucceeded || len(result.ArtifactIntentIDs) != 1 {
		t.Fatalf("round trip failed: %+v %v", result, err)
	}
	got, _ := d.only(t)
	if got.Purpose != worker.PurposePipeline || got.OwnerUserId != req.OwnerUserID || !reflect.DeepEqual(got.Args["imageBuild"], run.ImageBuild) {
		t.Fatalf("lost exact image contract: %+v", got)
	}
	run.SHA = strings.Repeat("b", 40)
	result, err = (*Fleet)(nil).RunStep(context.Background(), req, run)
	if err != nil || result.Failure == nil || result.Failure.Code != pl.CodeStepInvalid {
		t.Fatal("different source reached worker", result, err)
	}
}

func TestImageVerificationPoolFallbackRetainsFullBuildAndNoReplay(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "prestart capacity", true: "uncertain"}[uncertain], func(t *testing.T) {
			req := verificationRequest()
			result := refusedResult(pl.CodeNoMachineForNeed, "no eligible capacity")
			if uncertain {
				result = failedResult(pl.CodeExecutionUncertain, "reply lost")
			}
			fleet := &poolFleet{result: result}
			wb := &scriptedWorkbench{step: func(_ int, f *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
				var run StepRun
				if err := json.Unmarshal(f.req.GetArgsJson(), &run); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(run.ImageBuild, imageBuildWithProvenance(req.Step.ImageBuild, req.SHA, req.Event, req.Version)) || !reflect.DeepEqual(run.Artifacts, []string{pl.ImageVerificationPath}) {
					t.Fatal("fallback changed verification", run)
				}
				if err := checkImageBuildRun(func() Config { c, _ := imageBuildRun(); return c }(), run); err != nil {
					t.Fatal(err)
				}
				return &nodev1.WorkbenchForwardResponse{PayloadJson: mustMarshal(pl.StepResult{Status: pl.OutcomeSucceeded})}, "workbench-b", nil
			}}
			_, _ = newTestExecutor(wb, fleet).Execute(context.Background(), req)
			expected := 1
			if uncertain {
				expected = 0
			}
			if fleet.calls != 1 || len(wb.sent(workbench.PipelineStepAction)) != expected {
				t.Fatal("unsafe fallback", fleet.calls, len(wb.sent(workbench.PipelineStepAction)))
			}
		})
	}
}
