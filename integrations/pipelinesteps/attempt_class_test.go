package pipelinesteps

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// attempt_class_test.go -- what an attempt that did not succeed proves about
// its command (spec section 4 of
// docs/superpowers/specs/2026-10-28-pipeline-fleet-first-retries-failover-design.md).
//
// The driver retries from this class and nothing else, so each row here is a
// line of the spec's table. The machine rows live in fleet_attempt_test.go
// (they read the dispatcher's own word, which only an agent build links).

// classRow is one line of the spec's table.
type classRow struct {
	// surface is the table's "Where" column: "cluster" (a Job's answer) or
	// "either" (a reading that does not depend on where the step ran).
	surface string
	name    string
	res     pl.StepResult
	want    pl.AttemptClass
}

func exitedWith(code int) pl.StepResult {
	return pl.StepResult{Status: pl.OutcomeFailed, ExitCode: code}
}

// describeResult is a result as a failing table row should read: its status,
// its exit and its code, not a pointer.
func describeResult(res pl.StepResult) string {
	code := "no failure"
	if res.Failure != nil {
		code = res.Failure.Code
	}
	return fmt.Sprintf("%s, exit %d, %s", res.Status, res.ExitCode, code)
}

// eitherSurfaceRows are the rows that read the same on a Job and on a
// machine. fleet_attempt_test.go runs them through the fleet's classifier too,
// so "either" is checked on both sides rather than assumed.
var eitherSurfaceRows = []classRow{
	// either | the command exited 75: it said another run is safe and useful.
	{"either", "the command exited 75", exitedWith(75), pl.AttemptRetryRequested},
	// either | any other exit.
	{"either", "the command exited 1", exitedWith(1), pl.AttemptFinal},
	{"either", "the command exited 2", exitedWith(2), pl.AttemptFinal},
	{"either", "the command was killed (137)", exitedWith(137), pl.AttemptFinal},
	{"either", "exit 74 is not exit 75", exitedWith(74), pl.AttemptFinal},
	{"either", "exit 76 is not exit 75", exitedWith(76), pl.AttemptFinal},
	// 75 only means "retry me" when it is the whole answer: a typed failure
	// beside it (the step also timed out, say) is that failure's, and a
	// status other than failed is not a command's exit at all.
	{"either", "exit 75 beside a typed failure is the failure's", pl.StepResult{
		Status: pl.OutcomeFailed, ExitCode: 75, Failure: &pl.Failure{Code: pl.CodeStepTimeout, Message: "x"}}, pl.AttemptFinal},
	{"either", "exit 75 of a cancelled step is a cancel", pl.StepResult{Status: pl.OutcomeCancelled, ExitCode: 75}, pl.AttemptFinal},
	{"either", "exit 75 of a refused step is a refusal", pl.StepResult{Status: pl.OutcomeRefused, ExitCode: 75}, pl.AttemptFinal},
	// either | a timeout: a hang and a stall look alike from outside.
	{"either", "the step's own timeout", failedResult(pl.CodeStepTimeout, "x"), pl.AttemptFinal},
	{"either", "the run's ceiling", failedResult(pl.CodeRunCeiling, "x"), pl.AttemptFinal},
	// either | a cancel.
	{"either", "a cancel", cancelledResult("x"), pl.AttemptFinal},
	// either | a refusal.
	{"either", "a refusal", refusedResult(pl.CodeStepInvalid, "x"), pl.AttemptFinal},
	{"either", "an unknown need", refusedResult(pl.CodeNeedUnknown, "x"), pl.AttemptFinal},
	{"either", "a fleet the owner did not consent to", refusedResult(pl.CodeFleetNotConsented, "x"), pl.AttemptFinal},
	{"either", "the owner's off switch", refusedResult(pl.CodeFleetDisabled, "x"), pl.AttemptFinal},
	// either | the rest of the spec's "final" list.
	{"either", "the disk the step wrote", failedResult(pl.CodeStepDiskExceeded, "x"), pl.AttemptFinal},
	{"either", "a Job the cluster rejected", failedResult(pl.CodeJobRejected, "x"), pl.AttemptFinal},
	{"either", "a cluster that cannot isolate its steps", failedResult(pl.CodeIsolationUnenforced, "x"), pl.AttemptFinal},
	{"either", "artifacts too large to keep", failedResult(pl.CodeArtifactTooLarge, "x"), pl.AttemptFinal},
	{"either", "an executor error", failedResult(pl.CodeExecutorError, "x"), pl.AttemptFinal},
	{"either", "a secret failure", failedResult(pl.CodeSecretMissing, "x"), pl.AttemptFinal},
	// A code this table has never seen is final (D3): an unexpected answer is
	// never retried.
	{"either", "a code nobody classified", failedResult("pipeline_a_code_from_the_future", "x"), pl.AttemptFinal},
	// either | pipeline_execution_uncertain is the executor's own word that a
	// previous driver may have started the command. It is not in the spec's
	// table; both surfaces read it as outcome unknown.
	{"either", "a previous driver may have started it", failedResult(pl.CodeExecutionUncertain, "x"), pl.AttemptOutcomeUnknown},
	// pipeline_no_machine_for_need is only ever made when the dispatcher
	// proved no machine started the command (Fleet.classify, and the bounded
	// wait that gives up); a router that does not classify its own refusal
	// still reads right.
	{"either", "no machine could take it", refusedResult(pl.CodeNoMachineForNeed, "x"), pl.AttemptNothingRan},
}

func TestAttemptClassTable(t *testing.T) {
	pullFailure := &pl.Failure{Code: pl.CodeImagePullFailed, Message: "x"}
	rows := append([]classRow{
		// cluster | the Job never started the command.
		{"cluster", "a service stopped before the step", failedResult(pl.CodeServiceFailed, "x"), pl.AttemptNothingRan},
		{"cluster", "no node could schedule the pod", failedResult(pl.CodeJobUnschedulable, "x"), pl.AttemptNothingRan},
		{"cluster", "the Job's clone failed", failedResult(pl.CodeCloneFailed, "x"), pl.AttemptNothingRan},

		// cluster | an image that cannot be pulled is nothing ran only when the
		// RESULT proves the step's own container never started. The classifier
		// reports a pull failure of ANY container of the pod, a service's
		// sidecar included, and does so even while the step is running (the
		// kubelet pulls a restarted sidecar's image again), so the code proves
		// nothing alone. The proof is StartedAt, which the runner takes from the
		// step container (stepTimes) and leaves empty until it starts; the exit
		// status is -1 in both cases and proves nothing.
		{"cluster", "an image could not be pulled and the step never started", failedResult(pl.CodeImagePullFailed, "x"), pl.AttemptNothingRan},
		{"cluster", "an image could not be pulled while the step was running", pl.StepResult{
			Status: pl.OutcomeFailed, ExitCode: -1, StartedAt: "2026-10-04T12:00:05Z", Failure: pullFailure}, pl.AttemptOutcomeUnknown},
		{"cluster", "a pull failure that carries an exit status is not a pull failure of a step that never ran", pl.StepResult{
			Status: pl.OutcomeFailed, ExitCode: 0, Failure: pullFailure}, pl.AttemptOutcomeUnknown},

		// cluster | pipeline_runner_unavailable, as a RUNNER reports it or as it
		// arrives any other way (it is all this function can see), is an unknown
		// outcome: a runner answers it where the step's Job exists or may exist
		// (a Run that could not read the Job it had never seen, an unreadable
		// queued record on a recovery, a Job it could not claim or whose log it
		// could not open), and the Job is not held back by its claim. The agent's
		// own refusal before any runner took the step is classified where the
		// agent makes it (noRunnerReached; the Execute-level tests below).
		{"cluster", "a runner reported that it could not take the step", failedResult(pl.CodeRunnerUnavailable, "x"), pl.AttemptOutcomeUnknown},
		// cluster | the node under the pod went away: the command may have run.
		{"cluster", "the node was lost", failedResult(pl.CodeNodeLost, "x"), pl.AttemptOutcomeUnknown},
		// The spec lists pipeline_artifact_unavailable for a MACHINE only. A
		// Job's collection failing after an exit 0 is not in the table, so it
		// reads as the default, final (D3), until the table says otherwise.
		{"cluster", "artifacts a Job could not collect (not in the table)", pl.StepResult{
			Status: pl.OutcomeFailed, ExitCode: 0, Failure: &pl.Failure{Code: pl.CodeArtifactUnavailable, Message: "x"}}, pl.AttemptFinal},
	}, eitherSurfaceRows...)

	for _, c := range rows {
		t.Run(c.surface+"/"+c.name, func(t *testing.T) {
			if got := clusterAttemptClass(c.res); got != c.want {
				t.Errorf("clusterAttemptClass(%s) = %q, want %q", describeResult(c.res), got, c.want)
			}
		})
	}

	t.Run("a step that succeeded has no class to give", func(t *testing.T) {
		if got := clusterAttemptClass(pl.StepResult{Status: pl.OutcomeSucceeded}); got != "" {
			t.Errorf("a success was classified %q", got)
		}
	})
}

// clusterAnswer is a scripted workbench whose step forward answers res.
func clusterAnswer(res pl.StepResult) *scriptedWorkbench {
	return &scriptedWorkbench{step: func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
		return outcomeResponse(res), "workbench-a", nil
	}}
}

// TestExecuteClassifiesWhatTheClusterAnswered: Execute puts the class on a
// Job's answer, so the driver reads one place whichever surface ran the step.
func TestExecuteClassifiesWhatTheClusterAnswered(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer pl.StepResult
		want   pl.AttemptClass
	}{
		{"a Job that could not start its command", failedResult(pl.CodeImagePullFailed, "pull"), pl.AttemptNothingRan},
		{"a command that asked to run again", exitedWith(75), pl.AttemptRetryRequested},
		{"a node lost under the step", failedResult(pl.CodeNodeLost, "lost"), pl.AttemptOutcomeUnknown},
		{"an ordinary failure", exitedWith(1), pl.AttemptFinal},
		{"a timeout", failedResult(pl.CodeStepTimeout, "slow"), pl.AttemptFinal},
		{"a success", exOutcome("workbench-a"), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newTestExecutor(clusterAnswer(c.answer), &fakeFleet{})
			res, err := e.Execute(context.Background(), exRequest())
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.AttemptClass != c.want {
				t.Errorf("class = %q, want %q (result %+v)", res.AttemptClass, c.want, res)
			}
			if res.NotRetriedHere {
				t.Error("a Job is the cluster as a whole; there is no machine to not retry")
			}
		})
	}

	t.Run("a class a runner sent on a finished status is not believed either", func(t *testing.T) {
		// The second place a runner's outcome enters: the status the agent reads
		// when the reply to the forward was lost.
		forged := exitedWith(1)
		forged.AttemptClass, forged.NotRetriedHere = pl.AttemptNothingRan, true
		wb := &scriptedWorkbench{}
		wb.status = func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
			return finishedResponse(forged), "workbench-b", nil
		}
		e := newTestExecutor(wb, &fakeFleet{})
		res := awaitResult(t, executeAndCommitAsync(e, context.Background(), exRequest()), "the status said finished")
		if res.AttemptClass != pl.AttemptFinal || res.NotRetriedHere {
			t.Errorf("class = %q, notRetriedHere = %v: the runner's own word was kept", res.AttemptClass, res.NotRetriedHere)
		}
	})

	t.Run("a class a runner sent is not believed", func(t *testing.T) {
		// The class is the AGENT's reading of the failure. A reply that
		// arrives with one already on it (a runner of another version, a
		// replica that does not know better) must not be able to make an
		// ordinary failure retryable.
		forged := exitedWith(1)
		forged.AttemptClass, forged.NotRetriedHere = pl.AttemptNothingRan, true
		e := newTestExecutor(clusterAnswer(forged), &fakeFleet{})
		res, err := e.Execute(context.Background(), exRequest())
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if res.AttemptClass != pl.AttemptFinal || res.NotRetriedHere {
			t.Errorf("class = %q, notRetriedHere = %v: the runner's own word was kept", res.AttemptClass, res.NotRetriedHere)
		}
	})
}

// TestExecuteClassifiesWhatItRefusedAndWhatItCouldNotRoute: every ending of
// Execute carries a class, including the ones the executor made itself before
// any surface was asked.
func TestExecuteClassifiesWhatItRefusedAndWhatItCouldNotRoute(t *testing.T) {
	t.Run("a refusal is final", func(t *testing.T) {
		e := newTestExecutor(&scriptedWorkbench{}, &fakeFleet{})
		req := exRequest()
		req.Compute, req.Step.Needs = pl.ComputeClusterAndFleet, []string{"docker", "teleporter"}
		req.Step.Execution, req.Step.Image = pl.ExecutionNative, ""
		res, err := e.Execute(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		wantFailure(t, res, pl.OutcomeRefused, pl.CodeNeedUnknown)
		if res.AttemptClass != pl.AttemptFinal {
			t.Errorf("class = %q, want final", res.AttemptClass)
		}
	})

	t.Run("a run past its ceiling is final", func(t *testing.T) {
		e := newTestExecutor(&scriptedWorkbench{}, &fakeFleet{})
		req := exRequest()
		req.RunStartedAt = exNow.Add(-121 * time.Minute).Format(time.RFC3339Nano)
		res, err := e.Execute(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		wantFailure(t, res, pl.OutcomeFailed, pl.CodeRunCeiling)
		if res.AttemptClass != pl.AttemptFinal {
			t.Errorf("class = %q, want final: the ceiling is the run's, and a retry would meet it again", res.AttemptClass)
		}
	})

	t.Run("a node with no route to a workbench ran nothing", func(t *testing.T) {
		e := newTestExecutor(nil, &fakeFleet{})
		res, err := e.Execute(context.Background(), exRequest())
		if err != nil {
			t.Fatal(err)
		}
		wantFailure(t, res, pl.OutcomeFailed, pl.CodeRunnerUnavailable)
		if res.AttemptClass != pl.AttemptNothingRan {
			t.Errorf("class = %q, want nothing_ran: no Job was ever created", res.AttemptClass)
		}
	})

	t.Run("a node with no fleet dispatcher ran nothing", func(t *testing.T) {
		e := newTestExecutor(&scriptedWorkbench{}, nil)
		req := exRequest()
		req.Compute, req.Step.Needs = pl.ComputeClusterAndFleet, []string{"docker"}
		req.Step.Execution, req.Step.Image = pl.ExecutionNative, ""
		res, err := e.Execute(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		wantFailure(t, res, pl.OutcomeFailed, pl.CodeRunnerUnavailable)
		if res.AttemptClass != pl.AttemptNothingRan {
			t.Errorf("class = %q, want nothing_ran", res.AttemptClass)
		}
	})

	t.Run("a recover-only fleet attempt is an unknown outcome and reaches no machine", func(t *testing.T) {
		// A previous driver may have started the command on a machine and no
		// durable result was recorded. Nothing is sent again; whether the
		// step may run once more is the manifest's `repeatable`, decided by
		// the driver from this class.
		fleet := &fakeFleet{}
		e := newTestExecutor(nil, fleet)
		req := exRequest()
		req.RecoverOnly, req.Compute = true, pl.ComputeClusterAndFleet
		req.Step.Execution, req.Step.Image = pl.ExecutionNative, ""
		res, err := e.Execute(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		wantFailure(t, res, pl.OutcomeFailed, pl.CodeExecutionUncertain)
		if res.AttemptClass != pl.AttemptOutcomeUnknown {
			t.Errorf("class = %q, want outcome_unknown", res.AttemptClass)
		}
		if len(fleet.runs()) != 0 {
			t.Fatal("a recover-only fleet attempt was sent to a machine")
		}
	})

	t.Run("a recover-only pool attempt that finds no Job is an unknown outcome", func(t *testing.T) {
		// runPool sends a recovery to the cluster, which can adopt a Job by
		// name and otherwise answers pipeline_execution_uncertain.
		wb := clusterAnswer(failedResult(pl.CodeExecutionUncertain, "no existing job"))
		fleet := &poolFleet{}
		e := newTestExecutor(wb, fleet)
		req := poolRequest()
		req.RecoverOnly = true
		res, err := e.Execute(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		wantFailure(t, res, pl.OutcomeFailed, pl.CodeExecutionUncertain)
		if res.AttemptClass != pl.AttemptOutcomeUnknown || fleet.calls != 0 {
			t.Errorf("class = %q with %d fleet call(s), want outcome_unknown and none", res.AttemptClass, fleet.calls)
		}
	})
}

// classifyingFleet is a FleetRouter that answers a canned result, as a
// router of another implementation would.
type classifyingFleet struct {
	mu     sync.Mutex
	result pl.StepResult
	calls  int
}

func (f *classifyingFleet) RunStep(context.Context, pl.StepRequest, StepRun) (pl.StepResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.result, nil
}

// TestExecuteClassifiesWhatAFleetRouterLeftUnclassified: the real Fleet
// classifies what the dispatcher said (fleet_attempt_test.go), but the
// invariant the driver relies on -- every non-success ending carries a class
// -- must not depend on which FleetRouter is installed. An unclassified
// answer reads by the shared rules; one already classified is kept.
func TestExecuteClassifiesWhatAFleetRouterLeftUnclassified(t *testing.T) {
	fleetReq := func() pl.StepRequest {
		req := exRequest()
		req.Compute, req.Step.Needs = pl.ComputeClusterAndFleet, []string{"docker"}
		req.Step.Execution, req.Step.Image = pl.ExecutionNative, ""
		return req
	}
	t.Run("by the shared rules", func(t *testing.T) {
		for _, c := range []struct {
			answer pl.StepResult
			want   pl.AttemptClass
		}{
			{refusedResult(pl.CodeNoMachineForNeed, "busy"), pl.AttemptNothingRan},
			{exitedWith(75), pl.AttemptRetryRequested},
			{exitedWith(3), pl.AttemptFinal},
			{failedResult(pl.CodeNodeLost, "lost"), pl.AttemptOutcomeUnknown},
		} {
			fleet := &classifyingFleet{result: c.answer}
			res, err := newTestExecutor(nil, fleet).Execute(context.Background(), fleetReq())
			if err != nil {
				t.Fatal(err)
			}
			if res.AttemptClass != c.want {
				t.Errorf("answer %+v: class = %q, want %q", c.answer, res.AttemptClass, c.want)
			}
		}
	})
	t.Run("a class the router gave is kept, with its machine verdict", func(t *testing.T) {
		answer := refusedResult(pl.CodeNoMachineForNeed, "busy")
		answer.AttemptClass, answer.NotRetriedHere = pl.AttemptNothingRan, true
		res, err := newTestExecutor(nil, &classifyingFleet{result: answer}).Execute(context.Background(), fleetReq())
		if err != nil {
			t.Fatal(err)
		}
		if res.AttemptClass != pl.AttemptNothingRan || !res.NotRetriedHere {
			t.Errorf("class = %q, notRetriedHere = %v, want the router's own", res.AttemptClass, res.NotRetriedHere)
		}
	})
	t.Run("a success keeps no class", func(t *testing.T) {
		res, err := newTestExecutor(nil, &classifyingFleet{result: pl.StepResult{Status: pl.OutcomeSucceeded}}).Execute(context.Background(), fleetReq())
		if err != nil {
			t.Fatal(err)
		}
		if res.AttemptClass != "" || res.NotRetriedHere {
			t.Errorf("a success carries class %q / notRetriedHere %v", res.AttemptClass, res.NotRetriedHere)
		}
	})
}

// A forwarded cluster attempt must still name its Job and carry no machine
// fields: the workbench replica that receives it has nothing to do with a pin.
func TestAClusterForwardCarriesNoMachineNarrowing(t *testing.T) {
	wb := clusterAnswer(exOutcome("workbench-a"))
	e := newTestExecutor(wb, &fakeFleet{})
	if _, err := e.Execute(context.Background(), exRequest()); err != nil {
		t.Fatal(err)
	}
	forwards := wb.sent(workbench.PipelineStepAction)
	if len(forwards) != 1 {
		t.Fatalf("step forwards = %d, want 1", len(forwards))
	}
	run := stepRunOf(t, forwards[0])
	if run.PinWorker != "" || len(run.ExcludeWorkers) != 0 || run.MaxWaitSeconds != 0 {
		t.Errorf("a cluster attempt's StepRun names machines: %+v", run)
	}
}

// TestAClusterRunnerUnavailableIsNothingRanOnlyWhenTheAgentMadeIt: the same
// code, pipeline_runner_unavailable, is nothing ran when the AGENT makes it
// before any runner took the step, and an unknown outcome in every other way
// it can arrive. A runner makes it where the step's Job exists or may exist
// (see the table above), and a Job runs without the runner's claim, so reading
// it as nothing ran would send a second Job for a command that may already be
// running: a non-repeatable step would run twice.
func TestAClusterRunnerUnavailableIsNothingRanOnlyWhenTheAgentMadeIt(t *testing.T) {
	runnerSaid := failedResult(pl.CodeRunnerUnavailable, "the Kubernetes API server could not be asked for the step's Job: connection refused")
	notConfigured := &nodev1.WorkbenchForwardResponse{ErrorCode: workbench.ErrCodePipelinesNotConfigured, ErrorMessage: "no runner"}

	for _, c := range []struct {
		name string
		run  func(t *testing.T) pl.StepResult
		want pl.AttemptClass
		// says is in the failure's sentence; saysNot must not be: a sentence
		// that says "Nothing ran." about a step whose class is unknown would
		// tell the owner the opposite of the record.
		says, saysNot string
	}{
		// MADE BY THE AGENT, before any runner took the step.
		{"the agent has no route to a workbench", func(t *testing.T) pl.StepResult {
			res, err := newTestExecutor(nil, nil).Execute(context.Background(), exRequest())
			if err != nil {
				t.Fatal(err)
			}
			return res
		}, pl.AttemptNothingRan, "Nothing ran", ""},
		{"no replica was reachable, and none ever took the step", func(t *testing.T) pl.StepResult {
			var clock *exClock
			unreachable := func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
				clock.advance(10 * time.Second) // each try costs the executor's clock ten seconds
				return nil, "", workbench.ErrNoWorkbenchPeer
			}
			wb := &scriptedWorkbench{step: unreachable, status: unreachable}
			e := newTestExecutor(wb, nil)
			clock = withClock(e)
			e.noPeerPatience = 30 * time.Second
			return awaitResult(t, executeAndCommitAsync(e, context.Background(), exRequest()), "no replica was ever reachable")
		}, pl.AttemptNothingRan, "Nothing ran", ""},
		{"a step that needs a machine, on a node with no fleet dispatcher", func(t *testing.T) pl.StepResult {
			req := exRequest()
			req.Compute, req.Step.Needs = pl.ComputeClusterAndFleet, []string{"docker"}
			req.Step.Execution, req.Step.Image = pl.ExecutionNative, ""
			res, err := newTestExecutor(&scriptedWorkbench{}, nil).Execute(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			return res
		}, pl.AttemptNothingRan, "Nothing ran", ""},
		{"a pool step's first attempt, on a node with no fleet dispatcher", func(t *testing.T) pl.StepResult {
			res, err := newTestExecutor(&scriptedWorkbench{}, nil).Execute(context.Background(), poolRequest())
			if err != nil {
				t.Fatal(err)
			}
			return res
		}, pl.AttemptNothingRan, "Nothing ran", ""},
		{"the first replica to answer has no runner", func(t *testing.T) pl.StepResult {
			wb := &scriptedWorkbench{step: func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
				return notConfigured, "workbench-a", nil
			}}
			res, err := newTestExecutor(wb, nil).Execute(context.Background(), exRequest())
			if err != nil {
				t.Fatal(err)
			}
			return res
		}, pl.AttemptNothingRan, "Nothing ran", ""},

		// NOT MADE BY THE AGENT BEFORE ANY RUNNER TOOK THE STEP.
		{"a replica with no runner answers after another replica took the step and was lost", func(t *testing.T) pl.StepResult {
			// The first replica may have made the Job before it went away; the
			// re-forward then lands on a replica that cannot run steps. That
			// replica ran nothing, but the step did not start there.
			wb := &scriptedWorkbench{step: func(n int, _ *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
				if n == 1 {
					return nil, "workbench-a", workbench.ErrWorkbenchPeerLost
				}
				return notConfigured, "workbench-b", nil
			}}
			res, err := newTestExecutor(wb, nil).Execute(context.Background(), exRequest())
			if err != nil {
				t.Fatal(err)
			}
			return res
		}, pl.AttemptOutcomeUnknown, "", "Nothing ran"},
		{"a runner reported it in its reply to the forward", func(t *testing.T) pl.StepResult {
			res, err := newTestExecutor(clusterAnswer(runnerSaid), nil).Execute(context.Background(), exRequest())
			if err != nil {
				t.Fatal(err)
			}
			return res
		}, pl.AttemptOutcomeUnknown, "", ""},
		{"a runner reported it in a finished status", func(t *testing.T) pl.StepResult {
			wb := &scriptedWorkbench{}
			wb.status = func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
				return finishedResponse(runnerSaid), "workbench-b", nil
			}
			return awaitResult(t, executeAndCommitAsync(newTestExecutor(wb, nil), context.Background(), exRequest()), "the status said finished")
		}, pl.AttemptOutcomeUnknown, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := c.run(t)
			wantFailure(t, res, pl.OutcomeFailed, pl.CodeRunnerUnavailable)
			if res.AttemptClass != c.want || res.NotRetriedHere {
				t.Errorf("class = %q, notRetriedHere = %v, want %q and false", res.AttemptClass, res.NotRetriedHere, c.want)
			}
			if c.says != "" && !strings.Contains(res.Failure.Message, c.says) {
				t.Errorf("failure %q, want it to say %q", res.Failure.Message, c.says)
			}
			if c.saysNot != "" && strings.Contains(res.Failure.Message, c.saysNot) {
				t.Errorf("failure %q says %q about a step whose class is %s", res.Failure.Message, c.saysNot, c.want)
			}
		})
	}
}

// TestAnImagePullFailureIsNothingRanOnlyWhenTheStepNeverStarted drives the
// REAL classifier and the REAL settling of its verdict (Classify, then
// step.decision, which fills StartedAt from stepTimes), so the rule in
// clusterAttemptClass rests on what the runner records and not on a reading of
// it. Three pods, all reported as pipeline_image_pull_failed with exit -1; only
// StartedAt tells them apart.
func TestAnImagePullFailureIsNothingRanOnlyWhenTheStepNeverStarted(t *testing.T) {
	for _, c := range []struct {
		name        string
		pod         *Pod
		wantStarted bool
		want        pl.AttemptClass
	}{
		{"the step's own image cannot be pulled",
			clsPod("Pending", []ContainerStatus{clsCloneDone},
				clsWaiting("step", "ghcr.io/acme/toolchain:does-not-exist", "ErrImagePull", "not found")),
			false, pl.AttemptNothingRan},
		{"a service's image cannot be pulled before the step starts",
			clsPod("Pending", []ContainerStatus{clsCloneDone, clsWaiting("svc-db", "redis:7.9-nope", "ErrImagePull", "not found")}, clsStepInit),
			false, pl.AttemptNothingRan},
		{"a service that restarted cannot pull its image again while the step is running",
			// The sidecar started, the step started after it, and the sidecar was
			// restarted (imagePullPolicy Always) and cannot reach the registry.
			// Classify checks the pull state of every container before it asks
			// whether the step is running, so this is reported as a pull failure
			// of a command that is running.
			clsPod("Running", []ContainerStatus{clsCloneDone, clsWaiting("svc-db", "redis:7", "ImagePullBackOff", "Back-off pulling image")}, clsRunning("step")),
			true, pl.AttemptOutcomeUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			job := clsJob(1200)
			obs := Classify(job, c.pod, clsCreated, clsNow, clsConfig(), pl.CodeStepTimeout)
			if obs.Phase != PhaseFailed || obs.Failure == nil || obs.Failure.Code != pl.CodeImagePullFailed {
				t.Fatalf("Classify = phase %s, failure %+v; this test is about a pull failure the runner reports", clsPhaseNames[obs.Phase], obs.Failure)
			}
			if obs.ExitCode != -1 {
				t.Errorf("exit code = %d, want -1 in every case: it is no signal of whether the command started", obs.ExitCode)
			}
			s := &step{r: &Runner{now: func() time.Time { return clsNow }}, run: StepRun{TimeoutSeconds: 1200, DeadlineCode: pl.CodeStepTimeout}}
			dec := s.decision(obs, c.pod, job)
			if started := dec.StartedAt != ""; started != c.wantStarted {
				t.Fatalf("the runner recorded StartedAt %q, want it set = %v: it is how the agent tells the step never started", dec.StartedAt, c.wantStarted)
			}
			if got := clusterAttemptClass(dec); got != c.want {
				t.Errorf("clusterAttemptClass = %q, want %q", got, c.want)
			}
		})
	}
}
