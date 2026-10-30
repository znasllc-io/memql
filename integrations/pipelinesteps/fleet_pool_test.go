//go:build agent

package pipelinesteps

import (
	"context"
	"errors"
	"testing"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
	worker "github.com/znasllc-io/memql/integrations/agent/worker"
)

// The pool step's first attempt goes on to the cloud inside the attempt
// (fleetPreferenceRanOut) only after the preference ran out on capacity. A
// machine's own policy refusing is nothing ran, names that machine, and ends
// the attempt for the driver to route around; the engine's gate, an unknown
// execution, a lost reply and a runtime failure never reach the cloud this way.
func TestFleetPoolWaitOnlyEndsAfterConfirmedCapacityRefusal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		result   worker.Result
		err      error
		fallback bool
	}{
		{"busy", worker.Result{ErrorCode: "pipeline_capacity_busy", RefusedBeforeStart: true}, nil, true},
		{"no compatible machine", worker.Result{ErrorCode: "no_worker_available", RefusedBeforeStart: true}, nil, true},
		{"engine gate", worker.Result{ErrorCode: "no_worker_available", RefusedBeforeStart: true, RefusedByGate: true}, nil, false},
		{"worker policy", worker.Result{ErrorCode: "denied_by_policy", RefusedBeforeStart: true, WorkerId: "reg-1"}, nil, false},
		{"unknown execution", worker.Result{ErrorCode: "worker_disconnected"}, nil, false},
		{"transport", worker.Result{ErrorCode: "pipeline_capacity_busy", RefusedBeforeStart: true}, errors.New("lost response"), false},
		{"runtime failure", worker.Result{ErrorCode: "timeout"}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := exNow
			calls := 0
			dispatch := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
				calls++
				now = now.Add(31 * time.Second)
				return tc.result, tc.err
			}}
			fleet, _, _, _ := newTestFleet(t, dispatch)
			fleet.now = func() time.Time { return now }
			fleet.capacityRetry = time.Millisecond
			req := exRequest()
			req.Step.Execution = pl.ExecutionContainer
			req.Step.Needs = nil
			req.Step.Placement = pl.PlacementPool
			req.ComputePlacement = &pl.ComputePlacementPolicy{FleetWaitSeconds: 30, CloudFallback: true}
			run := fleetRun(req)
			run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
			result, err := fleet.RunStep(context.Background(), req, run)
			fallback := err == nil && fleetPreferenceRanOut(result)
			if fallback != tc.fallback || calls != 1 {
				t.Fatalf("%s result=%+v error=%v calls=%d", tc.name, result, err, calls)
			}
			if tc.name == "worker policy" && (result.AttemptClass != pl.AttemptNothingRan || result.Where.WorkerID != "reg-1") {
				t.Errorf("a machine's own policy refusal: class %q on %q, want nothing_ran naming the machine", result.AttemptClass, result.Where.WorkerID)
			}
		})
	}
}

func TestFleetPoolPreferenceDoesNotShortenStartedExecution(t *testing.T) {
	now := exNow
	calls := 0
	completed := fleetArtifactAnswer(extractTestTgz(t, []extractTestEntry{{name: "coverage.out", body: "proof"}}))
	dispatch := &fakeDispatcher{answer: func(ctx context.Context, req worker.Request) (worker.Result, error) {
		calls++
		now = now.Add(5 * time.Minute)
		if ctx.Err() != nil || req.Timeout != 900*time.Second+fleetCloneSlack {
			t.Fatal("preference wait became a command deadline")
		}
		// Preserve the declared artifact contract as well as successful command
		// completion; a missing required archive is correctly a failed step.
		return completed(ctx, req)
	}}
	fleet, _, _, _ := newTestFleet(t, dispatch)
	fleet.now = func() time.Time { return now }
	req := exRequest()
	req.Step.Execution = pl.ExecutionContainer
	req.Step.Needs = nil
	req.Step.Placement = pl.PlacementPool
	req.ComputePlacement = &pl.ComputePlacementPolicy{FleetWaitSeconds: 0, CloudFallback: true}
	run := fleetRun(req)
	run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
	result, err := fleet.RunStep(context.Background(), req, run)
	if err != nil || result.Status != pl.OutcomeSucceeded || calls != 1 || len(result.ArtifactFileIDs) != 1 || result.FinishedAt != exNow.Add(5*time.Minute).Format(time.RFC3339) {
		t.Fatalf("started result=%+v failure=%+v error=%v calls=%d", result, result.Failure, err, calls)
	}
}

// A pool step whose installation allows no cloud waits for a busy or an
// offline machine up to its run's ceiling, but not for one that does not
// exist: with no candidate at all and none offline (no_worker_available
// without WaitForConnection) its first attempt ends at once, nothing ran, as a
// fleet step's does, and the driver's retry order ends the step. With the cloud
// allowed, the Fleet preference still waits for a machine before the cluster
// (TestFleetPoolWaitOnlyEndsAfterConfirmedCapacityRefusal).
func TestANoCloudPoolStepWithNoMachineAtAllEndsAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result worker.Result
		atOnce bool
	}{
		{"no machine at all", worker.Result{ErrorCode: "no_worker_available", RefusedBeforeStart: true}, true},
		{"an eligible machine offline", worker.Result{ErrorCode: "no_worker_available", RefusedBeforeStart: true, WaitForConnection: true}, false},
		{"every machine busy", worker.Result{ErrorCode: "pipeline_capacity_busy", RefusedBeforeStart: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			now := exNow
			dispatch := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
				calls++
				now = now.Add(time.Minute)
				return tc.result, nil
			}}
			fleet, _, _, _ := newTestFleet(t, dispatch)
			fleet.now = func() time.Time { return now }
			fleet.capacityRetry = time.Millisecond
			req := exRequest()
			req.Step.Execution = pl.ExecutionContainer
			req.Step.Needs = nil
			req.Step.Placement = pl.PlacementPool
			req.ComputePlacement = &pl.ComputePlacementPolicy{FleetWaitSeconds: 0, CloudFallback: false}
			run := fleetRun(req)
			run.RunDeadline = exNow.Add(10 * time.Minute).Format(time.RFC3339)
			result, err := fleet.RunStep(context.Background(), req, run)
			if err != nil {
				t.Fatalf("RunStep: %v", err)
			}
			if !tc.atOnce {
				if calls < 2 || result.Failure == nil || result.Failure.Code != pl.CodeRunCeiling {
					t.Fatalf("dispatched %d time(s), ending %+v; want a wait for the machine up to the run's ceiling", calls, result.Failure)
				}
				return
			}
			if calls != 1 || result.Status != pl.OutcomeRefused || result.Failure == nil || result.Failure.Code != pl.CodeNoMachineForNeed ||
				result.AttemptClass != pl.AttemptNothingRan || result.Where.WorkerID != "" {
				t.Fatalf("dispatched %d time(s), ending %s %+v class %q on %q; want one dispatch, refused %s, nothing ran, naming no machine",
					calls, result.Status, result.Failure, result.AttemptClass, result.Where.WorkerID, pl.CodeNoMachineForNeed)
			}
		})
	}
}

func TestFleetPoolNoFallbackKeepsOriginalRunBound(t *testing.T) {
	calls := 0
	now := exNow
	dispatch := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
		calls++
		now = now.Add(time.Minute)
		return worker.Result{ErrorCode: "pipeline_capacity_busy", RefusedBeforeStart: true}, nil
	}}
	fleet, _, _, _ := newTestFleet(t, dispatch)
	fleet.now = func() time.Time { return now }
	fleet.capacityRetry = time.Millisecond
	req := exRequest()
	req.Step.Execution = pl.ExecutionContainer
	req.Step.Needs = nil
	req.Step.Placement = pl.PlacementPool
	req.ComputePlacement = &pl.ComputePlacementPolicy{FleetWaitSeconds: 0, CloudFallback: false}
	run := fleetRun(req)
	run.RunDeadline = exNow.Add(30 * time.Second).Format(time.RFC3339)
	result, err := fleet.RunStep(context.Background(), req, run)
	if err != nil || result.Failure == nil || result.Failure.Code != pl.CodeRunCeiling || calls != 1 {
		t.Fatalf("result=%+v %v calls=%d", result, err, calls)
	}
}
