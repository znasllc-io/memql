//go:build agent

package worker

import (
	"context"
	"errors"
	"reflect"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

func TestPipelineCapacityCandidateSurvivesOtherPreStartRefusalsAcrossReplicas(t *testing.T) {
	for _, capacityCode := range []string{"pipeline_capacity_busy", "pipeline_capacity_fenced"} {
		for _, refusal := range []string{"pipeline_runtime_unavailable", "pipeline_capacity_unavailable", "denied_by_policy"} {
			for _, busyFirst := range []bool{true, false} {
				name := capacityCode + "/" + refusal + "/waiting-local"
				if busyFirst {
					name = capacityCode + "/" + refusal + "/waiting-remote"
				}
				t.Run(name, func(t *testing.T) {
					h, dispatcher, _ := siblingThenLocal(t)
					h.link.reachable = true
					remoteCode, localCode := refusal, capacityCode
					workerID, nodeID, selectedBy, reroutedFrom := "desktop", nodeA, SelectedByReroute, "worker:laptop"
					if busyFirst {
						remoteCode, localCode = localCode, remoteCode
						workerID, nodeID, selectedBy, reroutedFrom = "laptop", nodeB, SelectedByPolicy, ""
					}
					h.registry.WorkerById("laptop").SetDispatchFunc(pipelineCapacityReply(remoteCode, nil), func() {})
					dispatcher.registry.WorkerById("desktop").SetDispatchFunc(pipelineCapacityReply(localCode, nil), func() {})

					result, err := dispatcher.Dispatch(asPipelineAcrossTheMesh(t, h), ownersPipelineRequest(h))
					if err != nil || result.OK || !result.RefusedBeforeStart || result.RefusedByGate || result.ErrorCode != capacityCode || result.ErrorMessage != capacityCode+": fixture recovery instruction" {
						t.Fatalf("capacity candidate was lost: result=%+v error=%v", result, err)
					}
					if result.WorkerId != workerID || result.NodeId != nodeID || !reflect.DeepEqual(result.Labels, pipelineLabels()) {
						t.Fatalf("refusal credited to the wrong machine: %+v", result)
					}
					row := h.store.lastInvocation(t)
					if row.WorkerId != workerID || row.Routing["attempts"] != 2 || row.Routing["selectedBy"] != selectedBy {
						t.Fatalf("wrong recorded candidate or attempt count: %+v", row)
					}
					if from, _ := row.Routing["reroutedFrom"].(string); from != reroutedFrom {
						t.Fatalf("wrong routing history: %+v", row.Routing)
					}
				})
			}
		}
	}
}

func TestPipelineCapacityCandidateNeverOverridesAnExecutionAcrossReplicas(t *testing.T) {
	for _, capacityCode := range []string{"pipeline_capacity_busy", "pipeline_capacity_fenced"} {
		for _, tc := range []struct {
			name string
			code string
			err  error
		}{
			{name: "success"},
			{name: "command failed", code: "exec_failed"},
			{name: "timeout", code: "timeout"},
			{name: "cleanup uncertain", code: "pipeline_cleanup_uncertain"},
			{name: "reply lost", err: errors.New("stream lost after dispatch")},
			{name: "busy with lost reply", code: "pipeline_capacity_busy", err: errors.New("stream lost after dispatch")},
			{name: "fence with lost reply", code: "pipeline_capacity_fenced", err: errors.New("stream lost after dispatch")},
			{name: "prior attempt recovery", code: "pipeline_recovery_required"},
		} {
			for _, executionRemote := range []bool{true, false} {
				name := capacityCode + "/" + tc.name + "/local"
				if executionRemote {
					name = capacityCode + "/" + tc.name + "/remote"
				}
				t.Run(name, func(t *testing.T) {
					h, dispatcher, _ := siblingThenLocal(t)
					h.link.reachable = true
					busy, execution := h.registry.WorkerById("laptop"), dispatcher.registry.WorkerById("desktop")
					workerID, nodeID := "desktop", nodeA
					if executionRemote {
						// Busy local machine first, then execution on the sibling.
						h.store.machines[0], h.store.machines[1] = h.store.machines[1], h.store.machines[0]
						busy, execution = execution, busy
						workerID, nodeID = "laptop", nodeB
					}
					busy.SetDispatchFunc(pipelineCapacityReply(capacityCode, nil), func() {})
					execution.SetDispatchFunc(pipelineCapacityReply(tc.code, tc.err), func() {})
					result, err := dispatcher.Dispatch(asPipelineAcrossTheMesh(t, h), ownersPipelineRequest(h))
					wantOK := tc.code == "" && tc.err == nil
					if err != nil || result.OK != wantOK || result.RefusedBeforeStart || result.WorkerId != workerID || result.NodeId != nodeID {
						t.Fatalf("execution replaced with a retryable refusal: %+v, error=%v", result, err)
					}
					if tc.err == nil && result.ErrorCode != tc.code {
						t.Fatalf("execution error lost: %+v", result)
					}
					row := h.store.lastInvocation(t)
					if row.WorkerId != workerID || row.Routing["attempts"] != 2 {
						t.Fatalf("execution not recorded: %+v", row)
					}
				})
			}
		}
	}
}

func TestPipelinePrefersOrdinaryBusyCapacityOverAnOperatorFenceAcrossReplicas(t *testing.T) {
	for _, busyRemote := range []bool{true, false} {
		t.Run(map[bool]string{true: "busy-remote", false: "busy-local"}[busyRemote], func(t *testing.T) {
			h, dispatcher, _ := siblingThenLocal(t)
			h.link.reachable = true
			remoteCode, localCode := "pipeline_capacity_fenced", "pipeline_capacity_busy"
			workerID, nodeID := "desktop", nodeA
			if busyRemote {
				remoteCode, localCode = localCode, remoteCode
				workerID, nodeID = "laptop", nodeB
			}
			h.registry.WorkerById("laptop").SetDispatchFunc(pipelineCapacityReply(remoteCode, nil), func() {})
			dispatcher.registry.WorkerById("desktop").SetDispatchFunc(pipelineCapacityReply(localCode, nil), func() {})
			result, err := dispatcher.Dispatch(asPipelineAcrossTheMesh(t, h), ownersPipelineRequest(h))
			if err != nil || result.OK || !result.RefusedBeforeStart || result.ErrorCode != "pipeline_capacity_busy" || result.WorkerId != workerID || result.NodeId != nodeID {
				t.Fatalf("ordinary busy candidate lost to operator fence: %+v %v", result, err)
			}
		})
	}
}

func pipelineCapacityReply(code string, err error) workerservice.DispatchFunc {
	return func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		if code == "" {
			return okResult(d.GetCallId()), err
		}
		return &memqlv1.ToolResult{CallId: d.GetCallId(), Payload: &memqlv1.ToolResult_Failure{Failure: &memqlv1.Failure{ErrorCode: code, ErrorMessage: code + ": fixture recovery instruction"}}}, err
	}
}
