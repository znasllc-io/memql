//go:build agent

package worker

import (
	"context"
	"encoding/json"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func TestImageVerificationRequiresRealCapabilityOnBothCandidateSets(t *testing.T) {
	good := machine("good")
	good.NativePlatform, good.PipelineImageVerification = "linux/amd64", 1
	bad := machine("bad")
	bad.NativePlatform = "linux/amd64"
	bad.Labels = map[string]string{"pipelineImageVerification": "1"}
	plan := RoutePlan{Candidates: []Candidate{bad, good}, OfflineCandidates: []Candidate{bad, good}}
	plan.RequireImageVerification()
	if len(plan.Candidates) != 1 || plan.Candidates[0].RegistrationId != "good" || len(plan.OfflineCandidates) != 1 || plan.OfflineCandidates[0].RegistrationId != "good" {
		t.Fatalf("unqualified machine survived: %+v", plan)
	}
}

func TestImageVerificationForwardedReceiverRechecksLiveCapability(t *testing.T) {
	for _, state := range []string{"absent", "unsupported", "wrong platform", "qualified"} {
		t.Run(state, func(t *testing.T) {
			h := pipelineHop(t, func(_ context.Context, dispatch *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
				if state != "qualified" {
					t.Fatal("unqualified receiver ran image verification")
				}
				return &memqlv1.ToolResult{CallId: dispatch.GetCallId(), Payload: &memqlv1.ToolResult_Success{Success: &memqlv1.Success{ResultJson: []byte(`{"exitCode":0}`)}}}, nil
			})
			d := h.registry.WorkerById("laptop").CapabilityDescriptor
			d.Platform, d.Architecture = "linux", "amd64"
			if state == "qualified" {
				d.PipelineImageVerification = 1
			}
			if state == "unsupported" {
				d.PipelineImageVerification = 2
			}
			if state == "wrong platform" {
				d.PipelineImageVerification = 1
				d.Architecture = "arm64"
			}
			request := forwardedStep(t, h, PurposePipeline, "")
			var args map[string]any
			if err := json.Unmarshal(request.ArgsJson, &args); err != nil {
				t.Fatal(err)
			}
			args["imageBuild"] = map[string]any{"verificationOnly": true}
			args["platform"] = "linux/amd64"
			args["execution"] = "container"
			request.ArgsJson, _ = json.Marshal(args)
			result := receive(t, h, request)
			if state == "qualified" {
				if !result.GetOk() {
					t.Fatalf("qualified receiver refused: %+v", result)
				}
				return
			}
			if !result.GetRefusedBeforeStart() || result.GetErrorCode() != codePipelinesNotAllowed {
				t.Fatalf("wrong refusal: %+v", result)
			}
		})
	}
}
