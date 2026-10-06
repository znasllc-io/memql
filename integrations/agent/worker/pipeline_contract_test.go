//go:build agent

package worker

import (
	"context"
	"errors"
	"fmt"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"testing"
)

func TestPipelineContractsCannotBeInventedByLabels(t *testing.T) {
	for _, version := range []int{0, 1, 3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			candidate := machine("ci-box", withLabels(pipelineLabels()))
			candidate.ActionContracts = workerservice.ActionContracts{"workerHost.pipeline_step": version}
			candidate.Labels["workerHost.pipeline_step"] = "2"
			fleet := newPipelineFleet(t, candidate)
			result, err := fleet.d.Dispatch(asPipelineExecutor(), pipelineRequest())
			if err != nil || result.OK || !result.RefusedBeforeStart || fleet.total() != 0 {
				t.Fatalf("incompatible worker dispatched: %+v %v", result, err)
			}
		})
	}
}

func TestPipelineReceivingReplicaRechecksBinaryContract(t *testing.T) {
	for _, version := range []int{0, 1, 3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			h := pipelineHop(t, func(context.Context, *memqlv1.ToolDispatch, func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
				t.Fatal("stale database metadata authorized an incompatible live connection")
				return nil, nil
			})
			h.registry.WorkerById("laptop").CapabilityDescriptor.ActionContracts["workerHost.pipeline_step"] = version
			result := receive(t, h, forwardedStep(t, h, PurposePipeline, ""))
			if !result.GetRefusedBeforeStart() || result.GetErrorCode() != codePipelinesNotAllowed {
				t.Fatalf("reply: %+v", result)
			}
		})
	}
}

func TestNativePlatformUsesBuildMetadataAndLiveConnection(t *testing.T) {
	matching := machine("matching")
	matching.NativePlatform = "darwin/arm64"
	wrong := machine("wrong", withLabels(map[string]string{"platform": "darwin/arm64"}))
	wrong.NativePlatform = "linux/arm64"
	unknown := machine("unknown")
	plan := RoutePlan{Candidates: []Candidate{wrong, unknown, matching}}
	plan.RequireNativePlatform("darwin/arm64")
	if len(plan.Candidates) != 1 || plan.Candidates[0].RegistrationId != "matching" {
		t.Fatalf("platform selection: %+v", plan)
	}
	w := &workerservice.Worker{Labels: pipelineLabels(), CapabilityDescriptor: &workerservice.CapabilityDescriptor{Platform: "darwin", Architecture: "amd64", ActionContracts: workerservice.ActionContracts{"workerHost.pipeline_step": 2}}}
	args := map[string]any{"execution": "native", "platform": "darwin/arm64"}
	if machineAllowsPipelines(w, args) {
		t.Fatal("stale host architecture admitted")
	}
	w.CapabilityDescriptor.Architecture = "arm64"
	if !machineAllowsPipelines(w, args) {
		t.Fatal("matching native worker refused")
	}
	args["execution"], args["platform"] = "container", "linux/arm64"
	if !machineAllowsPipelines(w, args) {
		t.Fatal("Docker host OS confused with container OS")
	}
}

func TestOnlyConfirmedV2RefusalsAllowAnotherMachine(t *testing.T) {
	for _, code := range []string{"pipeline_capacity_busy", "pipeline_capacity_unavailable", "pipeline_runtime_unavailable", "denied_by_policy"} {
		result := Result{ErrorCode: code}
		if !pipelineRefusedBeforeStart(PurposePipeline, result, nil) {
			t.Errorf("confirmed refusal %s lost", code)
		}
		if pipelineRefusedBeforeStart(PurposePipeline, result, errors.New("connection lost")) || pipelineRefusedBeforeStart("", result, nil) {
			t.Errorf("uncertain or agent result %s replayable", code)
		}
	}
	for _, code := range []string{"timeout", "pipeline_recovery_required", "pipeline_cleanup_uncertain", "exec_failed", "worker_disconnected", ""} {
		if pipelineRefusedBeforeStart(PurposePipeline, Result{ErrorCode: code}, nil) {
			t.Errorf("uncertain %s became replayable", code)
		}
	}
}

func TestV2PreStartProofSurvivesTheReplicaHop(t *testing.T) {
	for _, tc := range []struct {
		code    string
		reroute bool
	}{
		{"pipeline_capacity_busy", true}, {"pipeline_runtime_unavailable", true},
		{"denied_by_policy", true}, {"timeout", false}, {"pipeline_cleanup_uncertain", false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			h, dispatcher, ran := siblingThenLocal(t)
			h.link.mu.Lock()
			h.link.reachable = true
			h.link.mu.Unlock()
			h.registry.WorkerById("laptop").SetDispatchFunc(func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
				return &memqlv1.ToolResult{CallId: d.GetCallId(), Payload: &memqlv1.ToolResult_Failure{Failure: &memqlv1.Failure{ErrorCode: tc.code}}}, nil
			}, func() {})
			result, err := dispatcher.Dispatch(asPipelineAcrossTheMesh(t, h), ownersPipelineRequest(h))
			if err != nil || result.OK != tc.reroute || (*ran == 1) != tc.reroute {
				t.Fatalf("result: %+v, error: %v, local executions: %d", result, err, *ran)
			}
			if !tc.reroute && (result.RefusedBeforeStart || result.ErrorCode != tc.code) {
				t.Fatalf("lost uncertainty: %+v", result)
			}
		})
	}
}
