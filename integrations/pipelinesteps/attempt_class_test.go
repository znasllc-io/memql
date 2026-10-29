package pipelinesteps

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	rows := append([]classRow{
		// cluster | the clone failed. UNKNOWN, though a failed clone init container
		// does leave the pod terminal: the code has TWO sources the agent cannot
		// tell apart. The other is the runner's own failure to mint the clone token
		// (create()), made by a Run that found no Job while another Run of the same
		// attempt can still be creating it; see
		// TestAClusterCloneFailureIsAnUnknownOutcomeFromEitherSource.
		{"cluster", "the Job's clone failed", failedResult(pl.CodeCloneFailed, "x"), pl.AttemptOutcomeUnknown},

		// cluster | the other verdicts that read "the command never started" are
		// reported while the Job and its pod are still ALIVE (the runner only
		// annotates the Job; the agent's acknowledgement or a cancel deletes it).
		// The kubelet keeps retrying a failed pull and restarting service sidecars,
		// and the scheduler keeps trying to place a pod, so the step's command can
		// start AFTER the report: always an unknown outcome, whatever the pod
		// looked like when it was read. StartedAt (empty while the step container
		// has not started) and the exit status (-1 either way) are no proof, so a
		// row that sets them reads the same.
		{"cluster", "an image could not be pulled", failedResult(pl.CodeImagePullFailed, "x"), pl.AttemptOutcomeUnknown},
		{"cluster", "an image could not be pulled and the step had not started when the verdict was read", pl.StepResult{
			Status: pl.OutcomeFailed, ExitCode: -1, StartedAt: "", Failure: &pl.Failure{Code: pl.CodeImagePullFailed, Message: "x"}}, pl.AttemptOutcomeUnknown},
		{"cluster", "an image could not be pulled while the step was running", pl.StepResult{
			Status: pl.OutcomeFailed, ExitCode: -1, StartedAt: "2026-10-04T12:00:05Z", Failure: &pl.Failure{Code: pl.CodeImagePullFailed, Message: "x"}}, pl.AttemptOutcomeUnknown},
		{"cluster", "a service stopped before the step", failedResult(pl.CodeServiceFailed, "x"), pl.AttemptOutcomeUnknown},
		{"cluster", "no node could schedule the pod", failedResult(pl.CodeJobUnschedulable, "x"), pl.AttemptOutcomeUnknown},

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
		{"a Job whose clone failed", failedResult(pl.CodeCloneFailed, "clone"), pl.AttemptOutcomeUnknown},
		{"a runner that could not mint the clone token", failedResult(pl.CodeCloneFailed, "the clone token could not be minted: installation 42 is suspended"), pl.AttemptOutcomeUnknown},
		{"a Job whose image could not be pulled", failedResult(pl.CodeImagePullFailed, "pull"), pl.AttemptOutcomeUnknown},
		{"a Job whose service stopped before the step", failedResult(pl.CodeServiceFailed, "service"), pl.AttemptOutcomeUnknown},
		{"a Job whose pod no node could schedule", failedResult(pl.CodeJobUnschedulable, "pod"), pl.AttemptOutcomeUnknown},
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

// TestARecoveryIsAnUnknownOutcomeAtEverySiteThatMakesRunnerUnavailable: a
// recovery re-sends the SAME attempt because a previous driver journaled its
// intent and recorded no receipt (RecoverOnly), so the attempt's Job may exist
// and be running under the previous driver's runner. This executor has never
// heard from that runner, so "no replica ever took the step" -- a fact about
// this executor's own memory (clusterStep.reached), which starts false -- is no
// proof at all. At the three sites where the agent makes
// pipeline_runner_unavailable, a recovery therefore reads as an unknown outcome,
// and its sentence does not say "Nothing ran.". (The runner itself answers a
// recovery that cannot find the Job pipeline_execution_uncertain.)
func TestARecoveryIsAnUnknownOutcomeAtEverySiteThatMakesRunnerUnavailable(t *testing.T) {
	notConfigured := &nodev1.WorkbenchForwardResponse{ErrorCode: workbench.ErrCodePipelinesNotConfigured, ErrorMessage: "no runner"}
	recovery := func() pl.StepRequest {
		req := exRequest()
		req.RecoverOnly = true
		return req
	}
	poolRecovery := func() pl.StepRequest {
		req := poolRequest()
		req.RecoverOnly = true
		return req
	}
	// noReplica runs a recovery that no workbench replica is ever reachable for.
	// The clock moves half an hour per try, so the run's bound is reached after a
	// handful of tries and the test does not wait for it.
	noReplica := func(t *testing.T, req pl.StepRequest) pl.StepResult {
		var clock *exClock
		unreachable := func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
			clock.advance(30 * time.Minute)
			return nil, "", workbench.ErrNoWorkbenchPeer
		}
		wb := &scriptedWorkbench{step: unreachable, status: unreachable}
		e := newTestExecutor(wb, nil)
		clock = withClock(e)
		e.noPeerPatience = 30 * time.Second
		res := awaitResult(t, executeAndCommitAsync(e, context.Background(), req), "the recovery waited for a replica to the run's bound")
		if n := len(wb.sent(workbench.PipelineStepAction)); n < 3 {
			t.Errorf("step forwards = %d: a recovery must keep trying past the no-replica patience, not give up at it", n)
		}
		return res
	}

	for _, c := range []struct {
		name string
		run  func(t *testing.T) pl.StepResult
		code string
	}{
		{"the agent has no route to a workbench", func(t *testing.T) pl.StepResult {
			res, err := newTestExecutor(nil, nil).Execute(context.Background(), recovery())
			if err != nil {
				t.Fatal(err)
			}
			return res
		}, pl.CodeExecutionUncertain},
		{"a pool step's recovery, on an agent with no route to a workbench", func(t *testing.T) pl.StepResult {
			// runPool sends a recovery to the cluster and never to the fleet.
			fleet := &poolFleet{}
			res, err := newTestExecutor(nil, fleet).Execute(context.Background(), poolRecovery())
			if err != nil {
				t.Fatal(err)
			}
			if fleet.calls != 0 {
				t.Fatal("a pool step's recovery reached the fleet")
			}
			return res
		}, pl.CodeExecutionUncertain},
		{"no replica was reachable for longer than the no-replica patience", func(t *testing.T) pl.StepResult {
			return noReplica(t, recovery())
		}, pl.CodeNodeLost},
		{"a pool step's recovery, with no replica reachable", func(t *testing.T) pl.StepResult {
			return noReplica(t, poolRecovery())
		}, pl.CodeNodeLost},
		{"the first replica to answer has no runner", func(t *testing.T) pl.StepResult {
			wb := &scriptedWorkbench{step: func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
				return notConfigured, "workbench-a", nil
			}}
			res, err := newTestExecutor(wb, nil).Execute(context.Background(), recovery())
			if err != nil {
				t.Fatal(err)
			}
			return res
		}, pl.CodeRunnerUnavailable},
		{"a pool step's recovery, answered first by a replica with no runner", func(t *testing.T) pl.StepResult {
			wb := &scriptedWorkbench{step: func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
				return notConfigured, "workbench-a", nil
			}}
			res, err := newTestExecutor(wb, &poolFleet{}).Execute(context.Background(), poolRecovery())
			if err != nil {
				t.Fatal(err)
			}
			return res
		}, pl.CodeRunnerUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := c.run(t)
			wantFailure(t, res, pl.OutcomeFailed, c.code)
			if res.AttemptClass != pl.AttemptOutcomeUnknown || res.NotRetriedHere {
				t.Errorf("class = %q, notRetriedHere = %v, want outcome_unknown and false: the previous driver's runner may be running the step's Job", res.AttemptClass, res.NotRetriedHere)
			}
			if strings.Contains(res.Failure.Message, "Nothing ran") {
				t.Errorf("failure %q says \"Nothing ran.\" about a recovery whose class is unknown", res.Failure.Message)
			}
		})
	}
}

// TestAClusterVerdictMadeWhileTheJobIsAliveIsAnUnknownOutcome drives the REAL
// classifier and the REAL settling of its verdict (Classify, then step.decision)
// on a pod for each cluster verdict that reads "the command never started", and
// then the class. A runner reports these while the Job and its pod are still
// alive -- settling only annotates the Job; the agent's acknowledgement or a
// cancel deletes it -- and the cluster keeps working on the pod meanwhile: the
// kubelet retries a failed pull with back-off and restarts a service's sidecar,
// and the scheduler keeps trying to place an unschedulable pod. Any of them can
// succeed AFTER the report, and the step's command then starts. So a pull
// failure, a service failure and an unschedulable pod are always an unknown
// outcome, however the pod looked when it was read. (A failed clone is unknown
// for another reason, its two sources: TestAClusterCloneFailureIsAnUnknownOutcome
// FromEitherSource.)
//
// This test replaces the round-1 test that read the step container's StartedAt.
// With these three always unknown the class no longer reads StartedAt, so a test
// that pinned it would pin a dead path; this one asserts the class for every pod
// shape instead, including the one where StartedAt is empty (the step had not
// started when the verdict was read), which is the case the old rule trusted.
func TestAClusterVerdictMadeWhileTheJobIsAliveIsAnUnknownOutcome(t *testing.T) {
	unschedulable := &Pod{
		Metadata: ObjectMeta{Name: "mp-a7a72726d5075767e0b6d115-w4pdd"},
		Status: PodStatus{Phase: "Pending", Conditions: []PodCondition{{
			Type: "PodScheduled", Status: "False", Reason: "Unschedulable",
			Message: "0/1 nodes are available: 1 node(s) didn't match Pod's node affinity/selector.",
		}}},
	}
	for _, c := range []struct {
		name string
		pod  *Pod
		now  time.Time
		code string
		want pl.AttemptClass
	}{
		{"the step's own image cannot be pulled (the step has not started)",
			clsPod("Pending", []ContainerStatus{clsCloneDone},
				clsWaiting("step", "ghcr.io/acme/toolchain:does-not-exist", "ErrImagePull", "not found")),
			clsNow, pl.CodeImagePullFailed, pl.AttemptOutcomeUnknown},
		{"a service's image cannot be pulled before the step starts",
			clsPod("Pending", []ContainerStatus{clsCloneDone, clsWaiting("svc-db", "redis:7.9-nope", "ErrImagePull", "not found")}, clsStepInit),
			clsNow, pl.CodeImagePullFailed, pl.AttemptOutcomeUnknown},
		{"a service that restarted cannot pull its image again while the step is running",
			clsPod("Running", []ContainerStatus{clsCloneDone, clsWaiting("svc-db", "redis:7", "ImagePullBackOff", "Back-off pulling image")}, clsRunning("step")),
			clsNow, pl.CodeImagePullFailed, pl.AttemptOutcomeUnknown},
		{"a service stopped before the step started",
			clsPod("Pending", []ContainerStatus{clsCloneDone, clsTerminated("svc-db", 1, "Error", "", clsStart.Add(2*time.Second))}, clsStepInit),
			clsNow, pl.CodeServiceFailed, pl.AttemptOutcomeUnknown},
		{"no node could schedule the pod",
			unschedulable, clsCreated.Add(10*time.Minute + time.Second), pl.CodeJobUnschedulable, pl.AttemptOutcomeUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			job := clsJob(1200)
			obs := Classify(job, c.pod, clsCreated, c.now, clsConfig(), pl.CodeStepTimeout)
			if obs.Phase != PhaseFailed || obs.Failure == nil || obs.Failure.Code != c.code {
				t.Fatalf("Classify = phase %s, failure %+v; this test is about the %s verdict the runner reports", clsPhaseNames[obs.Phase], obs.Failure, c.code)
			}
			s := &step{r: &Runner{now: func() time.Time { return clsNow }}, run: StepRun{TimeoutSeconds: 1200, DeadlineCode: pl.CodeStepTimeout}}
			dec := s.decision(obs, c.pod, job)
			if got := clusterAttemptClass(dec); got != c.want {
				t.Errorf("clusterAttemptClass(%s, StartedAt %q) = %q, want %q", describeResult(dec), dec.StartedAt, got, c.want)
			}
		})
	}
}

// TestAClusterCloneFailureIsAnUnknownOutcomeFromEitherSource: on the cluster
// pipeline_clone_failed has two sources, and nothing in the result says which
// spoke.
//
//   - the pod verdict: the clone init container exited non-zero. That leaves the
//     pod terminal (restartPolicy Never, backoffLimit 0; TestBuildJob pins both),
//     so by itself it would prove the step never started;
//   - the runner's own failure to mint the clone token (create()), made by a Run
//     that found no Job and so created nothing (TestRunnerCloneTokenFailure
//     CreatesNothing). Another Run of the same attempt can still be alive in
//     create(): the create() doc says the Job it answers may be one another Run
//     created first, a recovery proceeds while the queued Secret reads queued
//     (the state a previous driver's Run leaves between quota-rejected POSTs),
//     and the Run whose mint failed never takes the creation claim, so the other
//     can still POST Job N.
//
// So the agent reads the code as an unknown outcome whichever spoke. The cluster
// already retries the clone's fetch three times inside the Job, so the retries
// this gives up are rare. This test drives both sources through the real
// functions (Classify and step.decision; the real Runner) and asserts the class.
func TestAClusterCloneFailureIsAnUnknownOutcomeFromEitherSource(t *testing.T) {
	t.Run("the pod verdict", func(t *testing.T) {
		pod := clsPod("Failed", []ContainerStatus{clsTerminated("clone", 128, "Error", "fatal: not our ref", clsStart.Add(time.Second)), clsSvcInit}, clsStepInit)
		job := clsJob(1200)
		obs := Classify(job, pod, clsCreated, clsNow, clsConfig(), pl.CodeStepTimeout)
		if obs.Phase != PhaseFailed || obs.Failure == nil || obs.Failure.Code != pl.CodeCloneFailed {
			t.Fatalf("Classify = phase %s, failure %+v; this test is about the clone verdict", clsPhaseNames[obs.Phase], obs.Failure)
		}
		s := &step{r: &Runner{now: func() time.Time { return clsNow }}, run: StepRun{TimeoutSeconds: 1200, DeadlineCode: pl.CodeStepTimeout}}
		if got := clusterAttemptClass(s.decision(obs, pod, job)); got != pl.AttemptOutcomeUnknown {
			t.Errorf("class = %q, want outcome_unknown", got)
		}
	})

	t.Run("the runner's clone-token failure", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.tokens.err = errors.New("installation 42 is suspended")
		res := h.run(t, rtRun())
		rtWantCode(t, res, pl.OutcomeFailed, pl.CodeCloneFailed)
		for _, request := range h.c.requests() {
			if request.Method != http.MethodGet {
				t.Fatalf("the Run that could not mint a token mutated Kubernetes: %s %s", request.Method, request.Path)
			}
		}
		if got := clusterAttemptClass(res); got != pl.AttemptOutcomeUnknown {
			t.Errorf("class = %q, want outcome_unknown: this Run created nothing, and another Run of the attempt may still create Job N", got)
		}
	})
}
