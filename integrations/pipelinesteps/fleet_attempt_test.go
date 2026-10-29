//go:build agent

package pipelinesteps

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
	worker "github.com/znasllc-io/memql/integrations/agent/worker"
)

// fleet_attempt_test.go -- what a MACHINE's answer proves about whether the
// command could have run, and where an attempt the driver placed may go
// (spec sections 4 and 5 of
// docs/superpowers/specs/2026-10-28-pipeline-fleet-first-retries-failover-design.md).
//
// Agent-tagged: the dispatcher is. The cluster's rows and the executor's own
// placement rules are in attempt_class_test.go and executor_placement_test.go.

// ---------------------------------------------------------------------------
// The class of an attempt on a machine
// ---------------------------------------------------------------------------

const attemptMachine = "reg-9"

// TestFleetAttemptClassTable: one row per way a machine can answer, read the
// way the driver will read it. The "flagged" rows are the dispatcher's own
// verdict that nothing started on any machine (Result.RefusedBeforeStart);
// the unflagged rows are a machine that was reached, whose raw code is the
// proof Fleet.classify throws away when it turns it into pipeline_executor_error.
func TestFleetAttemptClassTable(t *testing.T) {
	artifactReq := func() pl.StepRequest { return fleetArtifactReq() }
	exit := func(code, extra string) worker.Result {
		out := `{"exitCode":` + code + `,"durationMs":10` + extra + `}`
		return worker.Result{OK: true, OutputJSON: out, WorkerId: attemptMachine, NodeId: "agent-b"}
	}
	machine := func(code string, flagged bool) worker.Result {
		return worker.Result{ErrorCode: code, ErrorMessage: "the machine's own sentence", RefusedBeforeStart: flagged,
			WorkerId: attemptMachine, NodeId: "agent-b"}
	}
	gate := func(code string) worker.Result {
		r := machine(code, true)
		r.RefusedByGate, r.WorkerId, r.NodeId = true, "", ""
		return r
	}
	for _, c := range []struct {
		name       string
		req        func() pl.StepRequest // nil is fleetReq
		answer     worker.Result
		status     pl.Outcome
		class      pl.AttemptClass
		notRetried bool
	}{
		// machine | the fetch failed, or the cache could not be made, or the
		// runtime was not usable: all before the command.
		{"raw pipeline_clone_failed", nil, machine("pipeline_clone_failed", false), pl.OutcomeFailed, pl.AttemptNothingRan, false},
		{"raw pipeline_cache_unavailable", nil, machine("pipeline_cache_unavailable", false), pl.OutcomeFailed, pl.AttemptNothingRan, false},
		{"raw pipeline_runtime_unavailable", nil, machine("pipeline_runtime_unavailable", false), pl.OutcomeFailed, pl.AttemptNothingRan, false},
		{"pipeline_runtime_unavailable the dispatcher flagged", nil, machine("pipeline_runtime_unavailable", true), pl.OutcomeRefused, pl.AttemptNothingRan, false},
		// machine | a capacity refusal that reached the result.
		{"a capacity refusal that reached the result", nil, machine("pipeline_capacity_busy", false), pl.OutcomeFailed, pl.AttemptNothingRan, false},
		{"pipeline_capacity_unavailable", nil, machine("pipeline_capacity_unavailable", true), pl.OutcomeRefused, pl.AttemptNothingRan, false},
		// The machine's own policy refusing the repository is a refusal before
		// anything started. The table does not say to stop retrying it.
		{"denied_by_policy the dispatcher flagged", nil, machine("denied_by_policy", true), pl.OutcomeRefused, pl.AttemptNothingRan, false},
		{"denied_by_policy from a sibling that did not flag it", nil, machine("denied_by_policy", false), pl.OutcomeFailed, pl.AttemptNothingRan, false},
		// No candidate at all, or a code this engine has never seen: the
		// dispatcher's verdict is what decides, never the code.
		{"no machine to route to", nil, machine("no_worker_available", true), pl.OutcomeRefused, pl.AttemptNothingRan, false},
		{"a refusal code nobody has seen", nil, machine("a_code_this_engine_has_never_seen", true), pl.OutcomeRefused, pl.AttemptNothingRan, false},
		// worker_feature_unavailable: the settings read could not be made. It
		// is transient, so the same machine may be tried again (ruling 5).
		{"worker_feature_unavailable", nil, machine("worker_feature_unavailable", true), pl.OutcomeRefused, pl.AttemptNothingRan, false},

		// machine | nothing ran, and that machine is not retried.
		{"worker_feature_disabled", nil, machine("worker_feature_disabled", true), pl.OutcomeRefused, pl.AttemptNothingRan, true},
		{"pipelines_not_allowed", nil, machine("pipelines_not_allowed", true), pl.OutcomeRefused, pl.AttemptNothingRan, true},

		// machine | pipeline_recovery_required is an UNKNOWN outcome, never a
		// fresh start, and that machine is not tried again. The Cockpit answers
		// it before the command (a dirty reservation, checkout or command
		// ownership) AND after it (recording the cleanup of a command that has
		// finished, native or container), and the code alone cannot say which.
		// Read as "nothing ran" it could run a command that already ran a second
		// time on another machine. The machine refuses new work until it is
		// reconciled, so retrying it would only spend the delay.
		{"pipeline_recovery_required", nil, machine("pipeline_recovery_required", false), pl.OutcomeFailed, pl.AttemptOutcomeUnknown, true},
		// The dispatcher never flags it (TestOnlyConfirmedContractRefusalsAllowAnotherMachine
		// in the worker package pins that), so this row is a promise about the
		// future: a flag that did arrive would not change what the code can prove.
		{"pipeline_recovery_required even if a dispatcher flagged it", nil, machine("pipeline_recovery_required", true), pl.OutcomeRefused, pl.AttemptOutcomeUnknown, true},

		// machine | the connection ended after the dispatch left.
		{"worker_disconnected after the dispatch", nil, machine("worker_disconnected", false), pl.OutcomeFailed, pl.AttemptOutcomeUnknown, false},
		// machine | the command exited 0 and its artifacts did not arrive.
		{"exit 0 and no archive arrived", artifactReq, exit("0", ""), pl.OutcomeFailed, pl.AttemptOutcomeUnknown, false},
		{"exit 0 and the machine found the artifacts too large", artifactReq, exit("0", `,"artifactsTooLarge":true`), pl.OutcomeFailed, pl.AttemptFinal, false},

		// either | the command's exit.
		{"exit 75", nil, exit("75", ""), pl.OutcomeFailed, pl.AttemptRetryRequested, false},
		{"exit 75 whose artifacts never came", artifactReq, exit("75", `,"artifactsMissing":["coverage.out"]`), pl.OutcomeFailed, pl.AttemptRetryRequested, false},
		{"exit 1", nil, exit("1", ""), pl.OutcomeFailed, pl.AttemptFinal, false},
		{"exit 7 whose artifacts never came", artifactReq, exit("7", `,"artifactsMissing":["coverage.out"]`), pl.OutcomeFailed, pl.AttemptFinal, false},
		{"exit 0", nil, exit("0", ""), pl.OutcomeSucceeded, "", false},

		// either | a timeout, a cancel, a gate's refusal, anything else.
		{"the dispatcher's timeout", nil, machine("timeout", false), pl.OutcomeFailed, pl.AttemptFinal, false},
		{"a cancel on the machine", nil, machine("cancelled", false), pl.OutcomeCancelled, pl.AttemptFinal, false},
		{"the owner's off switch", nil, gate("kill_switch_engaged"), pl.OutcomeRefused, pl.AttemptFinal, false},
		// Fleet.classify reads the off switch's code before the dispatcher's
		// flags, so the owner's "nothing runs on my machines" is final however
		// the verdict travelled: it is not a machine that could not take the step.
		{"the owner's off switch flagged only as refused before start", nil, machine("kill_switch_engaged", true), pl.OutcomeRefused, pl.AttemptFinal, false},
		{"the engine's own gate", nil, gate("bad_request"), pl.OutcomeRefused, pl.AttemptFinal, false},
		{"a command that could not run", nil, machine("exec_failed", false), pl.OutcomeFailed, pl.AttemptFinal, false},
		{"a code nobody classified", nil, machine("something_new", false), pl.OutcomeFailed, pl.AttemptFinal, false},
		{"an answer that is not the contract", nil, worker.Result{OK: true, OutputJSON: `{"durationMs":10}`}, pl.OutcomeFailed, pl.AttemptFinal, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) { return c.answer, nil }}
			f, _, _, _ := newTestFleet(t, d)
			req := fleetReq()
			if c.req != nil {
				req = c.req()
			}
			res := runFleet(t, f, req)
			if res.Status != c.status {
				t.Fatalf("status = %q (%+v), want %q", res.Status, res.Failure, c.status)
			}
			if res.AttemptClass != c.class || res.NotRetriedHere != c.notRetried {
				t.Errorf("class = %q, notRetriedHere = %v; want %q, %v (failure %+v)",
					res.AttemptClass, res.NotRetriedHere, c.class, c.notRetried, res.Failure)
			}
			if c.notRetried && res.Where.WorkerID != attemptMachine {
				t.Errorf("Where.WorkerID = %q: the driver cannot mark a machine it was not told", res.Where.WorkerID)
			}
		})
	}
}

// TestFleetAttemptsThatNeverReachedAMachine: the results the fleet makes
// before, or instead of, a dispatch are read from what the result says.
func TestFleetAttemptsThatNeverReachedAMachine(t *testing.T) {
	t.Run("a clone token that could not be minted", func(t *testing.T) {
		// The spec's first machine row: "or the clone token could not be minted".
		d := &fakeDispatcher{}
		f, _, tokens, _ := newTestFleet(t, d)
		tokens.err = errors.New("the installation was suspended")
		res := runFleet(t, f, fleetReq())
		wantFailure(t, res, pl.OutcomeFailed, pl.CodeCloneFailed)
		if res.AttemptClass != pl.AttemptNothingRan || res.NotRetriedHere || len(d.reqs) != 0 {
			t.Errorf("class = %q notRetriedHere = %v with %d dispatch(es), want nothing_ran and no dispatch", res.AttemptClass, res.NotRetriedHere, len(d.reqs))
		}
	})
	t.Run("a node with no dispatcher", func(t *testing.T) {
		f, _, _, _ := newTestFleet(t, nil)
		res := runFleet(t, f, fleetReq())
		wantFailure(t, res, pl.OutcomeFailed, pl.CodeRunnerUnavailable)
		if res.AttemptClass != pl.AttemptNothingRan {
			t.Errorf("class = %q, want nothing_ran", res.AttemptClass)
		}
	})
	t.Run("a recover-only attempt", func(t *testing.T) {
		// A previous driver may have started the command and left no result.
		d := &fakeDispatcher{}
		f, _, _, _ := newTestFleet(t, d)
		req := fleetReq()
		req.RecoverOnly = true
		res := runFleet(t, f, req)
		wantFailure(t, res, pl.OutcomeFailed, pl.CodeExecutionUncertain)
		if res.AttemptClass != pl.AttemptOutcomeUnknown || len(d.reqs) != 0 {
			t.Errorf("class = %q with %d dispatch(es), want outcome_unknown and none", res.AttemptClass, len(d.reqs))
		}
	})
	t.Run("a step cancelled before it was sent", func(t *testing.T) {
		d := &fakeDispatcher{}
		f, _, _, _ := newTestFleet(t, d)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := fleetReq()
		res, err := f.RunStep(ctx, req, fleetRun(req))
		if err != nil {
			t.Fatal(err)
		}
		wantFailure(t, res, pl.OutcomeCancelled, pl.CodeStepCancelled)
		if res.AttemptClass != pl.AttemptFinal {
			t.Errorf("class = %q, want final", res.AttemptClass)
		}
	})
	t.Run("a step cancelled while the dispatcher answered", func(t *testing.T) {
		// classify reads the cancel first, whatever the dispatcher said: a
		// stream that dropped because the run was cancelled is not "unknown".
		ctx, cancel := context.WithCancel(context.Background())
		d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
			cancel()
			return worker.Result{ErrorCode: "worker_disconnected", ErrorMessage: "context canceled", WorkerId: attemptMachine}, nil
		}}
		f, _, _, _ := newTestFleet(t, d)
		req := fleetReq()
		res, err := f.RunStep(ctx, req, fleetRun(req))
		if err != nil {
			t.Fatal(err)
		}
		if res.AttemptClass != pl.AttemptFinal {
			t.Errorf("class = %q, want final for a cancel", res.AttemptClass)
		}
	})
}

// The "either" rows of the spec's table read the same from the fleet's side.
func TestEitherSurfaceRowsReadTheSameOnAMachine(t *testing.T) {
	for _, c := range eitherSurfaceRows {
		t.Run(c.name, func(t *testing.T) {
			got, notRetried := fleetAttemptClass(worker.Result{}, c.res, false)
			if got != c.want || notRetried {
				t.Errorf("fleetAttemptClass(%s) = %q, %v; want %q, false", describeResult(c.res), got, notRetried, c.want)
			}
		})
	}
}

// TestAPoolPreferenceThatRanOutRanNothing: the pool step's first attempt gives
// up on the fleet after the policy's fleetWaitSeconds (Codex behaviour, kept).
// What it answers when the policy does not let it go on to the cloud is the
// driver's to read, and nothing ran.
func TestAPoolPreferenceThatRanOutRanNothing(t *testing.T) {
	now := exNow
	d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
		now = now.Add(31 * time.Second)
		return worker.Result{ErrorCode: "pipeline_capacity_busy", RefusedBeforeStart: true, WorkerId: attemptMachine}, nil
	}}
	f, _, _, _ := newTestFleet(t, d)
	f.now = func() time.Time { return now }
	f.capacityRetry = time.Millisecond
	req := placedPoolRequest()
	run := fleetRun(req)
	run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
	res, err := f.RunStep(context.Background(), req, run)
	if err != nil {
		t.Fatal(err)
	}
	wantFailure(t, res, pl.OutcomeRefused, pl.CodeNoMachineForNeed)
	if res.AttemptClass != pl.AttemptNothingRan || res.NotRetriedHere {
		t.Errorf("class = %q, notRetriedHere = %v, want nothing_ran for the machine to be tried again", res.AttemptClass, res.NotRetriedHere)
	}
}

// ---------------------------------------------------------------------------
// Placement: the pin, the exclusions, and the bounded wait
// ---------------------------------------------------------------------------

// placedPoolRequest is a portable container step of a pool the installation
// lets spend cloud, trimmed to what the fleet path needs to succeed with no
// archive.
func placedPoolRequest() pl.StepRequest {
	req := poolRequest()
	req.Step.Needs, req.Step.Services, req.Step.Caches, req.Step.Artifacts = nil, nil, nil, nil
	req.ComputePlacement = &pl.ComputePlacementPolicy{FleetWaitSeconds: 30, CloudFallback: true}
	return req
}

// busyMachine answers that the pinned machine is held by another pipeline
// command, and moves the test clock one second for each question.
func busyMachine(now *time.Time, calls *int) *fakeDispatcher {
	return &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
		*calls++
		*now = now.Add(time.Second)
		return worker.Result{ErrorCode: "pipeline_capacity_busy", ErrorMessage: "another pipeline command holds the machine",
			RefusedBeforeStart: true, WorkerId: "reg-1", NodeId: "agent-b"}, nil
	}}
}

// TestPlacementPinsAndExcludesThroughTheDispatcher: the pin and the exclusions
// of an attempt's Placement reach the dispatcher on the one Request it is
// sent, as the registration ids earlier attempts reported. Nothing else about
// the dispatch changes: the machine owner's consent label is still required,
// because a pin narrows the plan and never admits.
func TestPlacementPinsAndExcludesThroughTheDispatcher(t *testing.T) {
	for _, c := range []struct {
		name         string
		placement    *pl.AttemptPlacement
		wantPin      string
		wantExclude  []string
		wantRerouted string
	}{
		{"a first attempt names no machine", nil, "", nil, ""},
		{"a same-machine retry pins it", &pl.AttemptPlacement{Surface: pl.PlacementFleet, PinWorker: "reg-1"}, "reg-1", nil, ""},
		{"a failover excludes the machines already tried, and says why it moved",
			&pl.AttemptPlacement{Surface: pl.PlacementFleet, ExcludeWorkers: []string{"reg-1", "reg-2"}},
			"", []string{"reg-1", "reg-2"}, "worker:reg-2"},
		{"one excluded machine is the one it moved from",
			&pl.AttemptPlacement{Surface: pl.PlacementFleet, ExcludeWorkers: []string{"reg-1"}}, "", []string{"reg-1"}, "worker:reg-1"},
		{"an id is passed on as it was reported",
			&pl.AttemptPlacement{Surface: pl.PlacementFleet, PinWorker: "v1:worker:registration:reg-1"}, "v1:worker:registration:reg-1", nil, ""},
		// A pin that is only whitespace names nothing, so the dispatcher matches
		// no machine: the failure to want if a driver lost the value. It must
		// not be trimmed here into "no pin", which would run anywhere.
		{"a blank pin is passed on, not mended", &pl.AttemptPlacement{Surface: pl.PlacementFleet, PinWorker: "  "}, "  ", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := &fakeDispatcher{}
			f, _, _, _ := newTestFleet(t, d)
			req := fleetReq()
			req.Placement = c.placement
			res, err := newTestExecutor(nil, f).Execute(context.Background(), req)
			if err != nil || res.Status != pl.OutcomeSucceeded {
				t.Fatalf("Execute = %+v, %v", res, err)
			}
			got, _ := d.only(t)
			if got.PinRegistration != c.wantPin || !slices.Equal(got.ExcludeRegistrations, c.wantExclude) || got.ReroutedFrom != c.wantRerouted {
				t.Errorf("dispatch pin = %q, exclude = %v, reroutedFrom = %q; want %q, %v, %q",
					got.PinRegistration, got.ExcludeRegistrations, got.ReroutedFrom, c.wantPin, c.wantExclude, c.wantRerouted)
			}
			if got.Purpose != worker.PurposePipeline || got.RequireLabels[worker.PipelinesLabel] != worker.PipelinesAllowed {
				t.Errorf("a placed dispatch dropped the pipeline purpose or the machine owner's consent label: %q %v", got.Purpose, got.RequireLabels)
			}
		})
	}

	t.Run("the dispatcher's list is its own copy", func(t *testing.T) {
		d := &fakeDispatcher{}
		f, _, _, _ := newTestFleet(t, d)
		req := fleetReq()
		req.Placement = &pl.AttemptPlacement{Surface: pl.PlacementFleet, ExcludeWorkers: []string{"reg-1"}}
		if _, err := newTestExecutor(nil, f).Execute(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		got, _ := d.only(t)
		if len(got.ExcludeRegistrations) != 1 {
			t.Fatalf("the dispatcher was sent exclusions %v, want the one machine", got.ExcludeRegistrations)
		}
		got.ExcludeRegistrations[0] = "edited"
		if req.Placement.ExcludeWorkers[0] != "reg-1" {
			t.Error("the dispatcher's exclusions alias the driver's placement")
		}
	})
}

// TestClusterPlacementRefusedForAFleetOnlyStep: a native step needs a
// machine, and the cluster has none to give it. A placement that names the
// cluster for it is refused before anything is sent, whatever the driver
// meant: the manifest's placement bounds where an attempt may go (spec D4).
func TestClusterPlacementRefusedForAFleetOnlyStep(t *testing.T) {
	d := &fakeDispatcher{}
	f, _, _, _ := newTestFleet(t, d)
	wb := &scriptedWorkbench{}
	req := fleetReq()
	req.Placement = &pl.AttemptPlacement{Surface: pl.PlacementCluster}
	res, err := newTestExecutor(wb, f).Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v -- a refusal is a typed outcome, not an executor error", err)
	}
	wantFailure(t, res, pl.OutcomeRefused, pl.CodeStepInvalid)
	if res.AttemptClass != pl.AttemptFinal {
		t.Errorf("class = %q, want final", res.AttemptClass)
	}
	if len(wb.forwards) != 0 || len(d.reqs) != 0 {
		t.Fatalf("a refused placement was sent on: %d forward(s) to the workbench, %d dispatch(es) to a machine", len(wb.forwards), len(d.reqs))
	}
	if !strings.Contains(res.Failure.Message, "owner's machines") {
		t.Errorf("message %q does not say why the cluster cannot take the step", res.Failure.Message)
	}
}

// TestPinnedBusyMachineWaitIsBounded (Review Focus 5): two steps pin the same
// busy machine. The first attempt waits as long as the run's ceiling allows,
// because nothing else can run the step; a retry the driver pinned to one
// machine waits at most the wait it was given, then counts as nothing ran and
// is not tried here again, so the ladder moves on.
func TestPinnedBusyMachineWaitIsBounded(t *testing.T) {
	now, calls := exNow, 0
	d := busyMachine(&now, &calls)
	f, lib, _, _ := newTestFleet(t, d)
	f.now = func() time.Time { return now }
	f.capacityRetry = time.Millisecond
	req := fleetReq()
	req.Placement = &pl.AttemptPlacement{Surface: pl.PlacementFleet, PinWorker: "reg-1", MaxWaitSeconds: 2}

	// A step that never stops waiting must fail this test, not hang it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := newTestExecutor(nil, f).Execute(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	wantFailure(t, res, pl.OutcomeRefused, pl.CodeNoMachineForNeed)
	if res.AttemptClass != pl.AttemptNothingRan || !res.NotRetriedHere {
		t.Errorf("class = %q, notRetriedHere = %v, want nothing_ran and not retried here", res.AttemptClass, res.NotRetriedHere)
	}
	if elapsed := now.Sub(exNow); elapsed < 2*time.Second || elapsed > 3*time.Second {
		t.Errorf("the wait took %v of the test clock, want it to end at the 2s it was given (within 3s)", elapsed)
	}
	if calls < 2 {
		t.Errorf("the step asked %d time(s): it gave up without waiting at all", calls)
	}
	for i, got := range d.reqs {
		if got.PinRegistration != "reg-1" {
			t.Errorf("dispatch %d pin = %q: the pin was lost while the step waited", i, got.PinRegistration)
		}
	}
	// Nothing started, so nothing was filed.
	if len(lib.files) != 0 {
		t.Errorf("a step that ran nowhere stored %d Library file(s)", len(lib.files))
	}
	// It says which machine it waited on, so the attempt record does.
	if res.Where.Surface != "fleet" || res.Where.WorkerID != "reg-1" {
		t.Errorf("Where = %+v, want the fleet and the busy machine", res.Where)
	}
	if res.Failure.Message == "" || strings.Contains(res.Failure.Message, "another pipeline command holds") {
		t.Errorf("message %q: it should say the wait ran out, and carry the code and not the machine's words", res.Failure.Message)
	}
}

// A pinned machine that is merely offline is waited for, within the same
// bound (spec section 5: "waits for it at most max(delaysSeconds)").
func TestPinnedOfflineMachineWaitIsBoundedToo(t *testing.T) {
	now, calls := exNow, 0
	d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
		calls++
		now = now.Add(time.Second)
		return worker.Result{ErrorCode: "no_worker_available", ErrorMessage: "reg-1 is not connected",
			RefusedBeforeStart: true, WaitForConnection: true}, nil
	}}
	f, _, _, _ := newTestFleet(t, d)
	f.now = func() time.Time { return now }
	f.capacityRetry = time.Millisecond
	req := fleetReq()
	req.Placement = &pl.AttemptPlacement{Surface: pl.PlacementFleet, PinWorker: "reg-1", MaxWaitSeconds: 2}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := newTestExecutor(nil, f).Execute(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	wantFailure(t, res, pl.OutcomeRefused, pl.CodeNoMachineForNeed)
	if res.AttemptClass != pl.AttemptNothingRan || !res.NotRetriedHere || calls < 2 {
		t.Errorf("class = %q, notRetriedHere = %v after %d question(s); want nothing_ran, not retried here, after waiting", res.AttemptClass, res.NotRetriedHere, calls)
	}
}

// A machine that is not a candidate at all is not waited for: no wait brings
// it back, and waiting is what MaxWaitSeconds would have been spent on.
func TestAPlacedAttemptDoesNotWaitForAMachineThatIsNotACandidate(t *testing.T) {
	calls := 0
	d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
		calls++
		return worker.Result{ErrorCode: "no_worker_available", ErrorMessage: "reg-1: Host commands are off in Fleet settings",
			RefusedBeforeStart: true}, nil
	}}
	f, _, _, _ := newTestFleet(t, d)
	f.capacityRetry = time.Millisecond
	req := fleetReq()
	req.Placement = &pl.AttemptPlacement{Surface: pl.PlacementFleet, PinWorker: "reg-1", MaxWaitSeconds: 600}
	res, err := newTestExecutor(nil, f).Execute(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	wantFailure(t, res, pl.OutcomeRefused, pl.CodeNoMachineForNeed)
	if calls != 1 || res.AttemptClass != pl.AttemptNothingRan {
		t.Errorf("asked %d time(s), class %q; want one question and nothing_ran", calls, res.AttemptClass)
	}
}

// TestAPlacedPoolAttemptWaitsOnItsOwnBudget (ruling A): a pool step's FIRST
// attempt keeps Codex's behaviour -- the policy's fleetWaitSeconds, then the
// cloud -- and an attempt the driver placed on the fleet does not, because the
// driver already decided the wait (it passes fleetWaitSeconds as the
// placement's MaxWaitSeconds) and the cloud (it is the driver's to name).
// Applying the preference deadline to a placed attempt as well would cut its
// wait short with the wrong answer, and would turn a machine's refusal that
// the driver can route around into a pipeline_executor_error it cannot.
func TestAPlacedPoolAttemptWaitsOnItsOwnBudget(t *testing.T) {
	req := func(placement *pl.AttemptPlacement) pl.StepRequest {
		r := placedPoolRequest()
		r.ComputePlacement = &pl.ComputePlacementPolicy{FleetWaitSeconds: 0, CloudFallback: true}
		r.Placement = placement
		return r
	}
	t.Run("a first attempt gives up on the fleet at once when the preference is zero", func(t *testing.T) {
		now, calls := exNow, 0
		d := busyMachine(&now, &calls)
		f, _, _, _ := newTestFleet(t, d)
		f.now = func() time.Time { return now }
		f.capacityRetry = time.Millisecond
		r := req(nil)
		run := fleetRun(r)
		run.RunDeadline = exNow.Add(time.Hour).Format(time.RFC3339)
		res, err := f.RunStep(context.Background(), r, run)
		if err != nil {
			t.Fatal(err)
		}
		wantFailure(t, res, pl.OutcomeRefused, pl.CodeNoMachineForNeed)
		if calls != 1 {
			t.Fatalf("asked %d time(s), want the preference to have ended the wait after the first", calls)
		}
	})

	t.Run("a placed attempt waits through the same refusals and runs when a machine frees up", func(t *testing.T) {
		now, calls := exNow, 0
		d := &fakeDispatcher{answer: func(_ context.Context, w worker.Request) (worker.Result, error) {
			calls++
			now = now.Add(time.Second)
			if calls <= 2 {
				return worker.Result{ErrorCode: "pipeline_capacity_busy", RefusedBeforeStart: true, WorkerId: "reg-1"}, nil
			}
			// A command that has started is never cut short by the wait it
			// waited out: its own timeout is the step's.
			if w.Args["timeoutSec"] != 900 || w.Timeout != 900*time.Second+fleetCloneSlack {
				t.Errorf("the wait spent the command's budget: timeoutSec %v, timeout %v", w.Args["timeoutSec"], w.Timeout)
			}
			return worker.Result{OK: true, OutputJSON: `{"exitCode":0,"durationMs":10}`, WorkerId: "reg-1", NodeId: "agent-b"}, nil
		}}
		f, _, _, _ := newTestFleet(t, d)
		f.now = func() time.Time { return now }
		f.capacityRetry = time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := newTestExecutor(nil, f).Execute(ctx, req(&pl.AttemptPlacement{Surface: pl.PlacementFleet, MaxWaitSeconds: 10}))
		if err != nil || res.Status != pl.OutcomeSucceeded || calls != 3 {
			t.Fatalf("Execute = %+v, %v after %d question(s); want a success on the third", res, err, calls)
		}
	})

	t.Run("an unpinned wait that runs out is nothing ran, and the machine is not tried here again", func(t *testing.T) {
		now, calls := exNow, 0
		d := busyMachine(&now, &calls)
		f, _, _, _ := newTestFleet(t, d)
		f.now = func() time.Time { return now }
		f.capacityRetry = time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := newTestExecutor(nil, f).Execute(ctx, req(&pl.AttemptPlacement{Surface: pl.PlacementFleet, ExcludeWorkers: []string{"reg-2"}, MaxWaitSeconds: 2}))
		if err != nil {
			t.Fatal(err)
		}
		wantFailure(t, res, pl.OutcomeRefused, pl.CodeNoMachineForNeed)
		if res.AttemptClass != pl.AttemptNothingRan || !res.NotRetriedHere {
			t.Errorf("class = %q, notRetriedHere = %v, want nothing_ran and not retried here, so rule 3 can name the cluster", res.AttemptClass, res.NotRetriedHere)
		}
		if res.Where.WorkerID != "reg-1" {
			t.Errorf("Where.WorkerID = %q, want the machine that stayed busy", res.Where.WorkerID)
		}
	})

	t.Run("a machine the driver can route around is nothing ran, not an executor error", func(t *testing.T) {
		refusal := worker.Result{ErrorCode: "pipelines_not_allowed", ErrorMessage: "reg-1 no longer allows pipelines",
			RefusedBeforeStart: true, WorkerId: "reg-1", NodeId: "agent-b"}
		d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) { return refusal, nil }}
		f, _, _, _ := newTestFleet(t, d)
		res, err := newTestExecutor(nil, f).Execute(context.Background(),
			req(&pl.AttemptPlacement{Surface: pl.PlacementFleet, PinWorker: "reg-1", MaxWaitSeconds: 30}))
		if err != nil {
			t.Fatal(err)
		}
		wantFailure(t, res, pl.OutcomeRefused, pl.CodeNoMachineForNeed)
		if res.AttemptClass != pl.AttemptNothingRan || !res.NotRetriedHere {
			t.Errorf("class = %q, notRetriedHere = %v, want nothing_ran and not retried here", res.AttemptClass, res.NotRetriedHere)
		}

		// The same refusal on the pool step's FIRST attempt is the Codex rule,
		// kept as it was: a refusal for a reason other than confirmed capacity
		// is never a licence to go on to the cloud, and reads as an executor
		// error. It is final -- the driver does not retry it.
		d = &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) { return refusal, nil }}
		f, _, _, _ = newTestFleet(t, d)
		res, err = newTestExecutor(&scriptedWorkbench{}, f).Execute(context.Background(), req(nil))
		if err != nil {
			t.Fatal(err)
		}
		wantFailure(t, res, pl.OutcomeRefused, pl.CodeExecutorError)
		if res.AttemptClass != pl.AttemptFinal {
			t.Errorf("a pool first attempt's non-capacity refusal: class = %q, want final", res.AttemptClass)
		}
	})
}

// A wait for capacity is a wait before anything starts. A command already
// running is bounded by its own timeout and the run's ceiling, never by the
// wait the driver allowed for a free machine.
func TestAPlacedWaitNeverShortensAStartedCommand(t *testing.T) {
	now := exNow
	d := &fakeDispatcher{answer: func(ctx context.Context, w worker.Request) (worker.Result, error) {
		now = now.Add(5 * time.Minute)
		if ctx.Err() != nil || w.Timeout != 900*time.Second+fleetCloneSlack {
			t.Errorf("the placement's wait became a command deadline: ctx %v, timeout %v", ctx.Err(), w.Timeout)
		}
		return worker.Result{OK: true, OutputJSON: `{"exitCode":0,"durationMs":10}`, WorkerId: "reg-1", NodeId: "agent-b"}, nil
	}}
	f, _, _, _ := newTestFleet(t, d)
	f.now = func() time.Time { return now }
	req := fleetReq()
	req.Placement = &pl.AttemptPlacement{Surface: pl.PlacementFleet, PinWorker: "reg-1", MaxWaitSeconds: 1}
	res, err := newTestExecutor(nil, f).Execute(context.Background(), req)
	if err != nil || res.Status != pl.OutcomeSucceeded {
		t.Fatalf("Execute = %+v, %v", res, err)
	}
}
