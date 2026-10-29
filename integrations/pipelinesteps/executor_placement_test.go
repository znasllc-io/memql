package pipelinesteps

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// executor_placement_test.go -- an attempt that carries a Placement goes
// exactly where it says (spec section 5). The driver, not the executor,
// decides the next place, so nothing inside one attempt may widen it: a
// "fleet" attempt never spills to the cluster, a "cluster" attempt never asks
// a machine, and neither can reach a place the step's own manifest placement
// does not allow.
//
// Not agent-tagged: Execute and the placement rules are in every build. The
// machine-side halves (the dispatcher's pin and exclusions, the bounded wait)
// are in fleet_attempt_test.go.

// recordingFleet is a FleetRouter that remembers what it was asked and answers
// a canned result.
type recordingFleet struct {
	mu     sync.Mutex
	reqs   []pl.StepRequest
	runs   []StepRun
	result pl.StepResult
}

func (f *recordingFleet) RunStep(_ context.Context, req pl.StepRequest, run StepRun) (pl.StepResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	f.runs = append(f.runs, run)
	return f.result, nil
}

func (f *recordingFleet) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

func (f *recordingFleet) only(t *testing.T) StepRun {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.runs) != 1 {
		t.Fatalf("fleet runs = %d, want 1", len(f.runs))
	}
	return f.runs[0]
}

// placementStep is one kind of step the matrix below places.
type placementStep struct {
	name string
	// req builds the request; the placement is set by the test.
	req func() pl.StepRequest
}

func clusterPlacementStep() pl.StepRequest { return exRequest() }

func fleetPlacementStep() pl.StepRequest {
	req := exRequest()
	req.Compute, req.Step.Needs = pl.ComputeClusterAndFleet, []string{"docker"}
	req.Step.Execution, req.Step.Image, req.Step.Services, req.Step.Caches = pl.ExecutionNative, "", nil, nil
	return req
}

// poolPlacementStep is a portable container step whose installation allows
// cloud fallback.
func poolPlacementStep() pl.StepRequest { return poolRequest() }

var placementSteps = []placementStep{
	{"cluster step", clusterPlacementStep},
	{"fleet step", fleetPlacementStep},
	{"pool step", poolPlacementStep},
}

func clusterSucceeds() *scriptedWorkbench { return clusterAnswer(exOutcome("workbench-a")) }

// where says which surface Execute asked.
func asked(wb *scriptedWorkbench, fleet *recordingFleet) (cluster, machines int) {
	return len(wb.sent(workbench.PipelineStepAction)), fleet.calls()
}

func TestPlacementRoutesAnAttemptWhereItSays(t *testing.T) {
	type outcome int
	const (
		toCluster outcome = iota
		toFleet
		refused
	)
	for _, c := range []struct {
		step      string
		placement *pl.AttemptPlacement
		want      outcome
	}{
		// No placement, or one that names no surface, is the step's own: the
		// first attempt.
		{"cluster step", nil, toCluster},
		{"cluster step", &pl.AttemptPlacement{}, toCluster},
		{"fleet step", nil, toFleet},
		{"fleet step", &pl.AttemptPlacement{}, toFleet},
		{"pool step", nil, toFleet}, // runPool tries the fleet first (its own tests cover the fall to the cluster)
		{"pool step", &pl.AttemptPlacement{}, toFleet},

		// A named surface is exact.
		{"cluster step", &pl.AttemptPlacement{Surface: pl.PlacementCluster}, toCluster},
		{"fleet step", &pl.AttemptPlacement{Surface: pl.PlacementFleet}, toFleet},
		{"pool step", &pl.AttemptPlacement{Surface: pl.PlacementFleet}, toFleet},
		{"pool step", &pl.AttemptPlacement{Surface: pl.PlacementCluster}, toCluster},

		// The manifest's placement bounds where an attempt may go (spec D4):
		// a cluster step retries on the cluster only, and a step that needs a
		// machine is never handed to the cluster.
		{"cluster step", &pl.AttemptPlacement{Surface: pl.PlacementFleet}, refused},
		{"fleet step", &pl.AttemptPlacement{Surface: pl.PlacementCluster}, refused},

		// A surface the executor does not know is refused, never read as "the
		// step's own": a typo must not send a step somewhere unintended.
		{"cluster step", &pl.AttemptPlacement{Surface: "moon"}, refused},
		{"fleet step", &pl.AttemptPlacement{Surface: "Fleet"}, refused},

		// A pin, exclusions and a wait name machines. On an attempt that is
		// not going to the fleet they would be silently ignored, so they are
		// refused instead.
		{"cluster step", &pl.AttemptPlacement{Surface: pl.PlacementCluster, PinWorker: "reg-1"}, refused},
		{"cluster step", &pl.AttemptPlacement{Surface: pl.PlacementCluster, ExcludeWorkers: []string{"reg-1"}}, refused},
		{"cluster step", &pl.AttemptPlacement{Surface: pl.PlacementCluster, MaxWaitSeconds: 5}, refused},
		{"cluster step", &pl.AttemptPlacement{PinWorker: "reg-1"}, refused},
		{"pool step", &pl.AttemptPlacement{Surface: pl.PlacementCluster, PinWorker: "reg-1"}, refused},
		// For a pool step the step's own placement is "try the fleet, then the
		// cluster": a pin there would be a pin that can spill. Name the fleet.
		{"pool step", &pl.AttemptPlacement{PinWorker: "reg-1"}, refused},
		{"pool step", &pl.AttemptPlacement{ExcludeWorkers: []string{"reg-1"}}, refused},
		{"pool step", &pl.AttemptPlacement{MaxWaitSeconds: 5}, refused},
		// ...and on a step that is the fleet's own, the pin is fine unnamed.
		{"fleet step", &pl.AttemptPlacement{PinWorker: "reg-1"}, toFleet},
		{"fleet step", &pl.AttemptPlacement{Surface: pl.PlacementFleet, PinWorker: "reg-1", ExcludeWorkers: []string{"reg-2"}, MaxWaitSeconds: 30}, toFleet},

		// A wait is bounded: negative is nonsense, and past an hour is past
		// anything the installation's policy allows (the same bound as a pool
		// step's fleetWaitSeconds).
		{"fleet step", &pl.AttemptPlacement{Surface: pl.PlacementFleet, MaxWaitSeconds: -1}, refused},
		{"fleet step", &pl.AttemptPlacement{Surface: pl.PlacementFleet, MaxWaitSeconds: 3601}, refused},
		{"fleet step", &pl.AttemptPlacement{Surface: pl.PlacementFleet, MaxWaitSeconds: 3600}, toFleet},
	} {
		name := c.step + " / " + placementLabel(c.placement)
		t.Run(name, func(t *testing.T) {
			var req pl.StepRequest
			for _, s := range placementSteps {
				if s.name == c.step {
					req = s.req()
				}
			}
			req.Placement = c.placement
			wb := clusterSucceeds()
			fleet := &recordingFleet{result: pl.StepResult{Status: pl.OutcomeSucceeded}}
			res, err := newTestExecutor(wb, fleet).Execute(context.Background(), req)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			cluster, machines := asked(wb, fleet)
			switch c.want {
			case toCluster:
				if cluster != 1 || machines != 0 || res.Status != pl.OutcomeSucceeded {
					t.Errorf("asked the cluster %d and the fleet %d time(s), result %+v; want the cluster once and a success", cluster, machines, res)
				}
			case toFleet:
				if cluster != 0 || machines != 1 || res.Status != pl.OutcomeSucceeded {
					t.Errorf("asked the cluster %d and the fleet %d time(s), result %+v; want the fleet once and a success", cluster, machines, res)
				}
			case refused:
				wantFailure(t, res, pl.OutcomeRefused, pl.CodeStepInvalid)
				if cluster != 0 || machines != 0 {
					t.Errorf("a refused placement was handed on: the cluster %d, the fleet %d", cluster, machines)
				}
				if res.AttemptClass != pl.AttemptFinal {
					t.Errorf("class = %q, want final: the same placement would be refused again", res.AttemptClass)
				}
				if !strings.Contains(res.Failure.Message, "placement") {
					t.Errorf("message %q does not say it is the attempt's placement that was refused", res.Failure.Message)
				}
			}
		})
	}
}

func placementLabel(p *pl.AttemptPlacement) string {
	if p == nil {
		return "no placement"
	}
	raw, _ := json.Marshal(p)
	return string(raw)
}

// TestACloudAttemptOfAPoolStepNeedsThePoliciesLeave: only the installation's
// own captured policy lets a pool step spend cloud (spec D7). The driver
// asks the same question before it names the cluster; the executor does not
// take the driver's word for it, because the cluster is the one place an
// attempt costs money.
func TestACloudAttemptOfAPoolStepNeedsThePoliciesLeave(t *testing.T) {
	req := poolPlacementStep()
	req.ComputePlacement.CloudFallback = false
	req.Placement = &pl.AttemptPlacement{Surface: pl.PlacementCluster}
	wb := clusterSucceeds()
	fleet := &recordingFleet{}
	res, err := newTestExecutor(wb, fleet).Execute(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	wantFailure(t, res, pl.OutcomeRefused, pl.CodeStepInvalid)
	if !strings.Contains(res.Failure.Message, "cloud") {
		t.Errorf("message %q does not say it is the cloud that the policy does not allow", res.Failure.Message)
	}
	if cluster, machines := asked(wb, fleet); cluster != 0 || machines != 0 {
		t.Fatalf("a Job was started against the installation's policy: cluster %d, fleet %d", cluster, machines)
	}

	t.Run("and with the policy's leave it runs as a Job, with no machine in it", func(t *testing.T) {
		req := poolPlacementStep()
		req.Placement = &pl.AttemptPlacement{Surface: pl.PlacementCluster}
		wb := clusterSucceeds()
		fleet := &recordingFleet{}
		res, err := newTestExecutor(wb, fleet).Execute(context.Background(), req)
		if err != nil || res.Status != pl.OutcomeSucceeded {
			t.Fatalf("Execute = %+v, %v", res, err)
		}
		if cluster, machines := asked(wb, fleet); cluster != 1 || machines != 0 {
			t.Fatalf("cluster %d, fleet %d, want one Job and no machine", cluster, machines)
		}
	})
}

// TestAPlacedFleetAttemptNeverSpillsToTheCluster: runPool falls to the cluster
// inside ONE attempt when the fleet refuses before start and the policy
// allows cloud. That is a first attempt's behaviour. An attempt the driver
// placed on the fleet answers the fleet's refusal as it is, so the driver
// can count the machine as tried and decide the next place itself.
func TestAPlacedFleetAttemptNeverSpillsToTheCluster(t *testing.T) {
	noMachine := refusedResult(pl.CodeNoMachineForNeed, "every machine refused before start")
	noMachine.AttemptClass = pl.AttemptNothingRan

	t.Run("without a placement the pool step's first attempt does spill", func(t *testing.T) {
		wb := clusterSucceeds()
		fleet := &recordingFleet{result: noMachine}
		res, err := newTestExecutor(wb, fleet).Execute(context.Background(), poolPlacementStep())
		if err != nil || res.Status != pl.OutcomeSucceeded {
			t.Fatalf("Execute = %+v, %v; want the first attempt to reach the cluster", res, err)
		}
		if cluster, machines := asked(wb, fleet); cluster != 1 || machines != 1 {
			t.Fatalf("cluster %d, fleet %d, want the fleet once and then the cluster once", cluster, machines)
		}
	})

	t.Run("a placement on the fleet answers the refusal and stops", func(t *testing.T) {
		req := poolPlacementStep()
		req.Placement = &pl.AttemptPlacement{Surface: pl.PlacementFleet, ExcludeWorkers: []string{"reg-1"}}
		wb := clusterSucceeds()
		fleet := &recordingFleet{result: noMachine}
		res, err := newTestExecutor(wb, fleet).Execute(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		wantFailure(t, res, pl.OutcomeRefused, pl.CodeNoMachineForNeed)
		if res.AttemptClass != pl.AttemptNothingRan {
			t.Errorf("class = %q, want nothing_ran", res.AttemptClass)
		}
		if cluster, machines := asked(wb, fleet); cluster != 0 || machines != 1 {
			t.Fatalf("cluster %d, fleet %d: the placed attempt spilled to the cluster, spending cloud the driver did not choose", cluster, machines)
		}
	})
}

// TestPlacementTravelsToTheFleetInTheStepRun: the pin, the exclusions and the
// wait are copied into the StepRun the fleet reads, as the attempt's own
// copies (the request is the driver's, and not the fleet's to edit).
func TestPlacementTravelsToTheFleetInTheStepRun(t *testing.T) {
	req := fleetPlacementStep()
	req.Placement = &pl.AttemptPlacement{Surface: pl.PlacementFleet, PinWorker: "reg-1", ExcludeWorkers: []string{"reg-2", "reg-3"}, MaxWaitSeconds: 45}
	fleet := &recordingFleet{result: pl.StepResult{Status: pl.OutcomeSucceeded}}
	if _, err := newTestExecutor(nil, fleet).Execute(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	run := fleet.only(t)
	if run.PinWorker != "reg-1" || !slices.Equal(run.ExcludeWorkers, []string{"reg-2", "reg-3"}) || run.MaxWaitSeconds != 45 {
		t.Fatalf("StepRun placement = pin %q, exclude %v, wait %d; want reg-1, [reg-2 reg-3], 45", run.PinWorker, run.ExcludeWorkers, run.MaxWaitSeconds)
	}
	run.ExcludeWorkers[0] = "edited"
	if req.Placement.ExcludeWorkers[0] != "reg-2" {
		t.Error("editing the StepRun's exclusions edited the driver's placement")
	}

	t.Run("a first attempt carries none, and none appears on the wire", func(t *testing.T) {
		fleet := &recordingFleet{result: pl.StepResult{Status: pl.OutcomeSucceeded}}
		if _, err := newTestExecutor(nil, fleet).Execute(context.Background(), fleetPlacementStep()); err != nil {
			t.Fatal(err)
		}
		run := fleet.only(t)
		if run.PinWorker != "" || run.ExcludeWorkers != nil || run.MaxWaitSeconds != 0 {
			t.Fatalf("a first attempt's StepRun names machines: %+v", run)
		}
		raw, err := json.Marshal(run)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"pinWorker", "excludeWorkers", "maxWaitSeconds"} {
			if strings.Contains(string(raw), key) {
				t.Errorf("an unplaced StepRun carries %q on the wire: %s", key, raw)
			}
		}
	})

	t.Run("the names are the wire's", func(t *testing.T) {
		raw, err := json.Marshal(StepRun{PinWorker: "a", ExcludeWorkers: []string{"b"}, MaxWaitSeconds: 1})
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{`"pinWorker":"a"`, `"excludeWorkers":["b"]`, `"maxWaitSeconds":1`} {
			if !strings.Contains(string(raw), key) {
				t.Errorf("StepRun JSON %s lacks %s", raw, key)
			}
		}
	})
}

// TestARecoverOnlyPlacedAttemptReachesNoMachine: a recovery re-sends the SAME
// attempt so a runner can re-attach to work in flight. A machine cannot be
// re-attached to: the outcome of what it may have run is unknown, and nothing
// is sent again. A cluster attempt can, by Job name.
func TestARecoverOnlyPlacedAttemptReachesNoMachine(t *testing.T) {
	t.Run("a pool step placed on the fleet", func(t *testing.T) {
		req := poolPlacementStep()
		req.RecoverOnly = true
		req.Placement = &pl.AttemptPlacement{Surface: pl.PlacementFleet}
		wb := clusterSucceeds()
		fleet := &recordingFleet{}
		res, err := newTestExecutor(wb, fleet).Execute(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		wantFailure(t, res, pl.OutcomeFailed, pl.CodeExecutionUncertain)
		if res.AttemptClass != pl.AttemptOutcomeUnknown {
			t.Errorf("class = %q, want outcome_unknown", res.AttemptClass)
		}
		if cluster, machines := asked(wb, fleet); cluster != 0 || machines != 0 {
			t.Fatalf("a recovery was handed on: cluster %d, fleet %d", cluster, machines)
		}
	})
	t.Run("a pool step placed on the cluster adopts its Job", func(t *testing.T) {
		req := poolPlacementStep()
		req.RecoverOnly = true
		req.Placement = &pl.AttemptPlacement{Surface: pl.PlacementCluster}
		wb := &scriptedWorkbench{step: func(_ int, f *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
			if !stepRunOf(t, f).RecoverOnly {
				t.Error("the recovery was sent as a new Job")
			}
			return outcomeResponse(exOutcome("workbench-b")), "workbench-b", nil
		}}
		fleet := &recordingFleet{}
		res, err := newTestExecutor(wb, fleet).Execute(context.Background(), req)
		if err != nil || res.Status != pl.OutcomeSucceeded {
			t.Fatalf("Execute = %+v, %v", res, err)
		}
		if cluster, machines := asked(wb, fleet); cluster != 1 || machines != 0 {
			t.Fatalf("cluster %d, fleet %d, want the Job adopted and no machine asked", cluster, machines)
		}
	})
}

// TestAFleetAttemptWithoutADispatcherSaysWhatItWasPlacedOn: the sentence names
// the step's needs when it has them, and does not say "needs ," when it has
// none (a pool step placed on the fleet).
func TestAFleetAttemptWithoutADispatcherSaysWhatItWasPlacedOn(t *testing.T) {
	for _, c := range []struct {
		name string
		req  pl.StepRequest
		says string
	}{
		{"a step that needs docker", fleetPlacementStep(), "docker"},
		{"a pool step placed on the fleet", func() pl.StepRequest {
			r := poolPlacementStep()
			r.Placement = &pl.AttemptPlacement{Surface: pl.PlacementFleet}
			return r
		}(), "placed on one of the owner's machines"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := newTestExecutor(&scriptedWorkbench{}, nil).Execute(context.Background(), c.req)
			if err != nil {
				t.Fatal(err)
			}
			wantFailure(t, res, pl.OutcomeFailed, pl.CodeRunnerUnavailable)
			if !strings.Contains(res.Failure.Message, c.says) || strings.Contains(res.Failure.Message, "needs ,") {
				t.Errorf("message %q, want it to say %q and not \"needs ,\"", res.Failure.Message, c.says)
			}
			if res.AttemptClass != pl.AttemptNothingRan {
				t.Errorf("class = %q, want nothing_ran", res.AttemptClass)
			}
		})
	}
}

// TestAPolicyOnADedicatedStepChangesNoRoute: retries apply to every step, so
// the driver now hands the installed policy to a cluster step and a fleet step
// as well as to a pool step (spec section 6). Only a pool step may read the
// policy's other half, the Fleet wait and the leave to spend cloud; if any
// path read it for another step, a fleet step the owner's machines could not
// take would quietly become a cloud Job, which is spending the owner never
// allowed, or a cluster step would go and ask the machines first. The routes
// below are the ones the same steps take with no policy at all.
func TestAPolicyOnADedicatedStepChangesNoRoute(t *testing.T) {
	policy := &pl.ComputePlacementPolicy{FleetWaitSeconds: 30, CloudFallback: true, Retries: pl.DefaultRetryPolicy()}

	t.Run("a cluster step still goes to the cluster alone", func(t *testing.T) {
		req := clusterPlacementStep()
		req.ComputePlacement = policy
		wb := clusterSucceeds()
		fleet := &recordingFleet{result: pl.StepResult{Status: pl.OutcomeSucceeded}}
		res, err := newTestExecutor(wb, fleet).Execute(context.Background(), req)
		if err != nil || res.Status != pl.OutcomeSucceeded {
			t.Fatalf("Execute = %+v, %v", res, err)
		}
		if cluster, machines := asked(wb, fleet); cluster != 1 || machines != 0 {
			t.Fatalf("cluster %d, fleet %d, want the cluster once and no machine", cluster, machines)
		}
	})

	t.Run("a fleet step that no machine can take ends there, and never becomes a Job", func(t *testing.T) {
		req := fleetPlacementStep()
		req.ComputePlacement = policy
		wb := clusterSucceeds()
		fleet := &recordingFleet{result: refusedResult(pl.CodeNoMachineForNeed, "every machine refused before start")}
		res, err := newTestExecutor(wb, fleet).Execute(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		wantFailure(t, res, pl.OutcomeRefused, pl.CodeNoMachineForNeed)
		if cluster, machines := asked(wb, fleet); cluster != 0 || machines != 1 {
			t.Fatalf("cluster %d, fleet %d: a fleet step reached the cloud because the policy allowed it for pool steps", cluster, machines)
		}
	})
}
