package pipelinesteps

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/workbench"
)

type poolFleet struct {
	calls  int
	result pl.StepResult
	err    error
}

func (f *poolFleet) RunStep(_ context.Context, _ pl.StepRequest, _ StepRun) (pl.StepResult, error) {
	f.calls++
	return f.result, f.err
}
func poolRequest() pl.StepRequest {
	r := exRequest()
	r.Step.Placement, r.Step.Execution, r.Step.Platform = pl.PlacementPool, pl.ExecutionContainer, "linux/amd64"
	r.Compute = pl.ComputeClusterAndFleet
	r.ComputePlacement = &pl.ComputePlacementPolicy{FleetWaitSeconds: 30, CloudFallback: true}
	return r
}

func TestPoolFallsBackOnlyAfterConfirmedPrestartCapacityRefusal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   pl.Outcome
		code     string
		err      error
		fallback bool
		machine  string
	}{
		{"unavailable before start", pl.OutcomeRefused, pl.CodeNoMachineForNeed, nil, true, ""},
		// One machine refused (its policy, its caps, its owner's switch): the
		// attempt ends nothing ran, and the driver routes around that machine.
		{"a machine refused before start", pl.OutcomeRefused, pl.CodeNoMachineForNeed, nil, false, "reg-1"},
		{"command completed", pl.OutcomeSucceeded, "", nil, false, ""},
		{"machine failed after start", pl.OutcomeFailed, pl.CodeNoMachineForNeed, nil, false, ""},
		{"authority refused", pl.OutcomeRefused, pl.CodeFleetDisabled, nil, false, ""},
		{"gate failed", pl.OutcomeRefused, pl.CodeExecutorError, nil, false, ""},
		{"uncertain execution", pl.OutcomeFailed, pl.CodeExecutionUncertain, nil, false, ""},
		{"transport error", pl.OutcomeRefused, pl.CodeNoMachineForNeed, errors.New("lost response"), false, ""},
		{"cancelled", pl.OutcomeCancelled, pl.CodeStepCancelled, nil, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fleet := &poolFleet{result: pl.StepResult{Status: tc.status, Failure: &pl.Failure{Code: tc.code}, Where: pl.Where{WorkerID: tc.machine}}, err: tc.err}
			// A second host receives only the serialized request: no originating
			// dispatcher state is available to grant or recover cloud execution.
			wb := &scriptedWorkbench{step: func(_ int, f *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
				return &nodev1.WorkbenchForwardResponse{PayloadJson: mustMarshal(pl.StepResult{Status: pl.OutcomeSucceeded, Where: pl.Where{Surface: "cluster"}})}, "workbench-b", nil
			}}
			_, _ = newTestExecutor(wb, fleet).Execute(context.Background(), poolRequest())
			if fleet.calls != 1 || (len(wb.sent(workbench.PipelineStepAction)) != 0) != tc.fallback {
				t.Fatalf("wrong routing: Fleet=%d cluster=%d", fleet.calls, len(wb.sent(workbench.PipelineStepAction)))
			}
		})
	}
}

func TestPoolRequiresInstalledPolicyAndPreservesNoFallback(t *testing.T) {
	for _, kind := range []string{"missing policy", "bad wait", "no consent", "no fallback"} {
		t.Run(kind, func(t *testing.T) {
			r := poolRequest()
			switch kind {
			case "missing policy":
				r.ComputePlacement = nil
			case "bad wait":
				r.ComputePlacement.FleetWaitSeconds = 3601
			case "no consent":
				r.Compute = pl.ComputeCluster
			case "no fallback":
				r.ComputePlacement.CloudFallback = false
			}
			fleet := &poolFleet{result: refusedResult(pl.CodeNoMachineForNeed, "busy before start")}
			wb := &scriptedWorkbench{}
			_, _ = newTestExecutor(wb, fleet).Execute(context.Background(), r)
			if len(wb.sent(workbench.PipelineStepAction)) != 0 {
				t.Fatal("cloud fallback escaped policy")
			}
			if kind != "no fallback" && fleet.calls != 0 {
				t.Fatal("invalid request reached Fleet")
			}
		})
	}
}

func TestPoolRecoveryOnlyAdoptsNeverRedispatchesFleet(t *testing.T) {
	r := poolRequest()
	r.RecoverOnly = true
	fleet := &poolFleet{}
	wb := &scriptedWorkbench{step: func(_ int, f *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
		var run StepRun
		if err := json.Unmarshal(f.req.GetArgsJson(), &run); err != nil {
			t.Fatal(err)
		}
		if !run.RecoverOnly {
			t.Fatal("recovery became a new cloud command")
		}
		return &nodev1.WorkbenchForwardResponse{PayloadJson: mustMarshal(failedResult(pl.CodeExecutionUncertain, "no existing job"))}, "workbench-b", nil
	}}
	_, _ = newTestExecutor(wb, fleet).Execute(context.Background(), r)
	if fleet.calls != 0 || len(wb.sent(workbench.PipelineStepAction)) != 1 {
		t.Fatalf("recovery rerouted work: Fleet=%d", fleet.calls)
	}
}
