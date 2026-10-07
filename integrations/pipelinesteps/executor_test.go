package pipelinesteps

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// executor_test.go -- the agent's executor against a scripted workbench
// (epic memql#5478, #5493). Each test scripts what the workbench side
// answers; executor_hop_test.go runs the same executor against real forward
// handlers on two replicas.

// exNow is the executor's clock in these tests. Timers run in real time; the
// clock only decides how much of the run's ceiling is left.
var exNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

const exAgent = "agent-a"

// exRequest is a cluster step of a run that started ten minutes before exNow.
func exRequest() pl.StepRequest {
	req := testRequest()
	req.RunStartedAt = exNow.Add(-10 * time.Minute).Format(time.RFC3339)
	return req
}

func exConfig() Config {
	cfg := testConfig()
	cfg.RunCeiling = 120 * time.Minute
	cfg.DefaultStepTimeout = 20 * time.Minute
	cfg.HeartbeatStale = 45 * time.Second
	return cfg
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// exClock is an executor clock a test moves by hand. Every patience the
// executor measures between forwards elapses only when the test advances it,
// however loaded the machine running the test is; timers stay real.
type exClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *exClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *exClock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

func (c *exClock) set(at time.Time) {
	c.mu.Lock()
	c.at = at
	c.mu.Unlock()
}

// withClock gives e an advanceable clock starting at exNow. Call it before
// the executor runs anything.
func withClock(e *Executor) *exClock {
	c := &exClock{at: exNow}
	e.now = c.now
	return c
}

// newTestExecutor is an executor on the fixed clock, with every interval
// small enough for a test and the give-up far enough away that only a test
// about it reaches it. No patience measured on the fixed clock ever runs out:
// a stale or absent status never forwards the step again, however many
// arrive, so a test about one moves the clock (withClock) and sets the
// patience it measures.
func newTestExecutor(fwd Forwarder, fleet FleetRouter) *Executor {
	e := NewExecutor(exConfig(), fwd, fleet, quietLogger())
	e.now = func() time.Time { return exNow }
	e.statusEvery = 20 * time.Millisecond
	e.peerWatch = 5 * time.Millisecond
	e.lostGrace = time.Hour
	e.noPeerWait = 5 * time.Millisecond
	e.noPeerPatience = time.Hour
	e.callTimeout = 2 * time.Second
	return e
}

// exForward is one request the scripted workbench was sent.
type exForward struct {
	req     *nodev1.WorkbenchForwardRequest
	pin     string
	exclude string
	watched bool
	// ended is closed when the forward stopped waiting because its context
	// ended -- the moment a real router sends WorkbenchForwardCancel.
	ended chan struct{}
}

// scriptedWorkbench is a Forwarder whose answers the test writes. A step
// forward with no script waits until its context ends, as a real one does
// when its reply is lost.
type scriptedWorkbench struct {
	mu       sync.Mutex
	forwards []*exForward

	step   func(n int, f *exForward) (*nodev1.WorkbenchForwardResponse, string, error)
	status func(n int, f *exForward) (*nodev1.WorkbenchForwardResponse, string, error)
	ack    func(f *exForward) (*nodev1.WorkbenchForwardResponse, string, error)
	cancel func(n int, f *exForward) (*nodev1.WorkbenchForwardResponse, string, error)

	steps, statuses, cancels int
}

func (w *scriptedWorkbench) SelfNodeId() string   { return exAgent }
func (w *scriptedWorkbench) SelfNodeType() string { return "agent" }

func (w *scriptedWorkbench) ForwardWatchedExcluding(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pin, exclude string, _ time.Duration, onSelected func(string)) (*nodev1.WorkbenchForwardResponse, string, error) {
	if onSelected != nil {
		onSelected("workbench-a")
	}
	return w.forward(ctx, req, pin, exclude, true)
}

func (w *scriptedWorkbench) Forward(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pin string) (*nodev1.WorkbenchForwardResponse, string, error) {
	return w.forward(ctx, req, pin, "", false)
}

func (w *scriptedWorkbench) forward(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pin, exclude string, watched bool) (*nodev1.WorkbenchForwardResponse, string, error) {
	f := &exForward{req: req, pin: pin, exclude: exclude, watched: watched, ended: make(chan struct{})}
	w.mu.Lock()
	w.forwards = append(w.forwards, f)
	var script func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error)
	n := 0
	switch req.GetAction() {
	case workbench.PipelineStepAction:
		w.steps++
		n, script = w.steps, w.step
	case workbench.PipelineStatusAction:
		w.statuses++
		n, script = w.statuses, w.status
	case workbench.PipelineAckAction:
		if w.ack != nil {
			script = func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) { return w.ack(f) }
		}
	case workbench.PipelineCancelAction:
		w.cancels++
		n, script = w.cancels, w.cancel
	}
	w.mu.Unlock()

	if script != nil {
		if resp, servedBy, err := script(n, f); resp != nil || err != nil {
			return resp, servedBy, err
		}
	}
	if req.GetAction() != workbench.PipelineStepAction {
		// Status, ack and cancel answer at once unless scripted otherwise.
		return &nodev1.WorkbenchForwardResponse{RequestId: req.GetRequestId(), PayloadJson: defaultReply(req.GetAction())}, "workbench-a", nil
	}
	<-ctx.Done()
	close(f.ended)
	return nil, "workbench-a", ctx.Err()
}

func defaultReply(action string) []byte {
	switch action {
	case workbench.PipelineStatusAction:
		return mustMarshal(StatusReply{State: StateRunning})
	case workbench.PipelineCancelAction:
		return []byte(`{"jobsDeleted":1}`)
	}
	return nil
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// sent returns the forwards of one action, in order.
func (w *scriptedWorkbench) sent(action string) []*exForward {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []*exForward
	for _, f := range w.forwards {
		if f.req.GetAction() == action {
			out = append(out, f)
		}
	}
	return out
}

func outcomeResponse(res pl.StepResult) *nodev1.WorkbenchForwardResponse {
	return &nodev1.WorkbenchForwardResponse{PayloadJson: mustMarshal(res)}
}

func finishedResponse(res pl.StepResult) *nodev1.WorkbenchForwardResponse {
	return &nodev1.WorkbenchForwardResponse{PayloadJson: mustMarshal(StatusReply{State: StateFinished, Result: &res})}
}

func stateResponse(state string) *nodev1.WorkbenchForwardResponse {
	return &nodev1.WorkbenchForwardResponse{PayloadJson: mustMarshal(StatusReply{State: state})}
}

// exOutcome is what the runner answers for a step that passed.
func exOutcome(node string) pl.StepResult {
	return pl.StepResult{
		Status:     pl.OutcomeSucceeded,
		ExitCode:   0,
		StartedAt:  "2026-10-04T12:00:05Z",
		FinishedAt: "2026-10-04T12:03:05Z",
		Where:      pl.Where{Surface: "cluster", NodeID: node, JobName: JobName("run-7f3a", "tests.go-tests#2", 2)},
		LogFileID:  "file-log",
		LogLines:   12,
	}
}

// stepRunOf decodes the StepRun a step forward carries.
func stepRunOf(t *testing.T, f *exForward) StepRun {
	t.Helper()
	var run StepRun
	if err := json.Unmarshal(f.req.GetArgsJson(), &run); err != nil {
		t.Fatalf("the step forward's args are not a StepRun: %v (%s)", err, f.req.GetArgsJson())
	}
	return run
}

func awaitCond(t *testing.T, cond func() bool, failure string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(failure)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// executeAndCommitAsync models the driver accepting and committing an outcome.
// Raw Execute tests separately verify that returning an outcome never ACKs it.
func executeAndCommitAsync(e *Executor, ctx context.Context, req pl.StepRequest) <-chan pl.StepResult {
	out := make(chan pl.StepResult, 1)
	go func() {
		res, err := e.Execute(ctx, req)
		if err != nil {
			res = pl.StepResult{Status: "error: " + pl.Outcome(err.Error())}
		} else if res.Status != pl.OutcomeCancelled {
			e.AcknowledgeReceipt(ctx, req)
		}
		out <- res
	}()
	return out
}

func awaitResult(t *testing.T, ch <-chan pl.StepResult, what string) pl.StepResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(5 * time.Second):
		t.Fatalf("Execute did not return: %s", what)
		return pl.StepResult{}
	}
}

// fakeFleet records the steps handed to the fleet.
type fakeFleet struct {
	mu    sync.Mutex
	calls []StepRun
	// block, when set, parks RunStep until its context ends.
	block bool
}

func (f *fakeFleet) RunStep(ctx context.Context, req pl.StepRequest, run StepRun) (pl.StepResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, run)
	block := f.block
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return pl.StepResult{Status: pl.OutcomeCancelled, ExitCode: -1,
			Failure: &pl.Failure{Code: pl.CodeStepCancelled, Message: "cancelled"}}, nil
	}
	return pl.StepResult{Status: pl.OutcomeSucceeded, Where: pl.Where{Surface: "fleet", WorkerID: "machine-1"}}, nil
}

func (f *fakeFleet) runs() []StepRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]StepRun(nil), f.calls...)
}

func wantFailure(t *testing.T, res pl.StepResult, status pl.Outcome, code string) {
	t.Helper()
	if res.Status != status || res.Failure == nil || res.Failure.Code != code {
		t.Fatalf("result = %+v (failure %+v), want %s with %s", res, res.Failure, status, code)
	}
	if strings.TrimSpace(res.Failure.Message) == "" {
		t.Errorf("the %s failure has no sentence; the check run shows it to a person", code)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1: the step's command never reported", res.ExitCode)
	}
}

// ---------------------------------------------------------------------------
// Refusals and the deadline
// ---------------------------------------------------------------------------

// TestExecuteRefusesAnUnknownNeedAndAnUnconsentedFleetStep: the executor never
// routes on a name it does not know, never sends a step to a laptop its
// pipeline did not consent to, and runs command steps only.
func TestExecuteRefusesAnUnknownNeedAndAnUnconsentedFleetStep(t *testing.T) {
	cases := []struct {
		name    string
		edit    func(*pl.StepRequest)
		outcome pl.Outcome
		code    string
	}{
		{"a need outside the closed set", func(r *pl.StepRequest) {
			r.Compute, r.Step.Needs = pl.ComputeClusterAndFleet, []string{"docker", "teleporter"}
			r.Step.Execution, r.Step.Image = pl.ExecutionNative, ""
		}, pl.OutcomeRefused, pl.CodeNeedUnknown},
		{"a need on a cluster-only pipeline", func(r *pl.StepRequest) {
			r.Compute, r.Step.Needs = pl.ComputeCluster, []string{"docker"}
			r.Step.Execution, r.Step.Image = pl.ExecutionNative, ""
		}, pl.OutcomeRefused, pl.CodeFleetNotConsented},
		{"a need where compute is absent, which means cluster", func(r *pl.StepRequest) {
			r.Compute, r.Step.Needs = "", []string{"gpu"}
			r.Step.Execution, r.Step.Image = pl.ExecutionNative, ""
		}, pl.OutcomeRefused, pl.CodeFleetNotConsented},
		{"a notify step, which is the driver's", func(r *pl.StepRequest) {
			r.Step.Kind, r.Step.Run, r.Step.Channel = pl.StepNotify, "", "znas-instance"
		}, pl.OutcomeRefused, pl.CodeExecutorError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wb := &scriptedWorkbench{}
			fleet := &fakeFleet{}
			e := newTestExecutor(wb, fleet)
			req := exRequest()
			c.edit(&req)

			res, err := e.Execute(context.Background(), req)
			if err != nil {
				t.Fatalf("Execute: %v -- a refusal is a typed outcome, not an executor error", err)
			}
			wantFailure(t, res, c.outcome, c.code)
			if n := len(wb.sent(workbench.PipelineStepAction)) + len(fleet.runs()); n != 0 {
				t.Fatalf("a refused step was handed on (%d time(s)): to the workbench %d, to the fleet %d",
					n, len(wb.sent(workbench.PipelineStepAction)), len(fleet.runs()))
			}
		})
	}

	// THE REACHABLE POSITIVES, so each refusal above is about its own rule:
	// the same need on a consenting pipeline goes to the fleet, and a step with
	// no need goes to the workbench.
	t.Run("a known need on a consenting pipeline goes to the fleet", func(t *testing.T) {
		wb := &scriptedWorkbench{}
		fleet := &fakeFleet{}
		e := newTestExecutor(wb, fleet)
		req := exRequest()
		req.Compute, req.Step.Needs = pl.ComputeClusterAndFleet, []string{"docker"}
		req.Step.Execution, req.Step.Image = pl.ExecutionNative, ""
		res, err := e.Execute(context.Background(), req)
		if err != nil || res.Status != pl.OutcomeSucceeded {
			t.Fatalf("Execute = %+v, %v; want the fleet's success", res, err)
		}
		if runs := fleet.runs(); len(runs) != 1 || runs[0].StepKey != req.StepKey {
			t.Fatalf("fleet runs = %v, want the step once", runs)
		}
		if n := len(wb.sent(workbench.PipelineStepAction)); n != 0 {
			t.Fatalf("a fleet step was also forwarded to the workbench %d time(s)", n)
		}
	})
	t.Run("a step with no need goes to the workbench", func(t *testing.T) {
		wb := &scriptedWorkbench{step: func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
			return outcomeResponse(exOutcome("workbench-a")), "workbench-a", nil
		}}
		fleet := &fakeFleet{}
		e := newTestExecutor(wb, fleet)
		req := exRequest()
		req.Compute = pl.ComputeClusterAndFleet
		res, err := e.Execute(context.Background(), req)
		if err != nil || res.Status != pl.OutcomeSucceeded {
			t.Fatalf("Execute = %+v, %v; want the workbench's success", res, err)
		}
		if len(fleet.runs()) != 0 {
			t.Fatal("a step with no need went to the fleet")
		}
	})
}

func TestFleetRecoveryDoesNotRedispatchAnUncertainEffect(t *testing.T) {
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
	if len(fleet.runs()) != 0 {
		t.Fatal("a replacement worker could have repeated an external effect")
	}
}

// TestExecuteRefusesPastTheRunCeiling: once the run's wall-clock ceiling has
// passed, a step fails pipeline_run_ceiling without a Job -- and so does one
// that would be given less than a second, which no Job deadline can express.
func TestExecuteRefusesPastTheRunCeiling(t *testing.T) {
	for _, c := range []struct {
		name    string
		started time.Time
	}{
		{"a minute past the ceiling", exNow.Add(-121 * time.Minute)},
		{"exactly at the ceiling", exNow.Add(-120 * time.Minute)},
		{"under a second before it", exNow.Add(-120 * time.Minute).Add(500 * time.Millisecond)},
	} {
		t.Run(c.name, func(t *testing.T) {
			wb := &scriptedWorkbench{}
			fleet := &fakeFleet{}
			e := newTestExecutor(wb, fleet)
			req := exRequest()
			req.RunStartedAt = c.started.Format(time.RFC3339Nano)
			res, err := e.Execute(context.Background(), req)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			wantFailure(t, res, pl.OutcomeFailed, pl.CodeRunCeiling)
			if len(wb.forwards) != 0 || len(fleet.runs()) != 0 {
				t.Fatal("a step past the run's ceiling was handed on; it must fail without a Job")
			}
		})
	}

	t.Run("a run start the executor cannot read is not an unbounded run", func(t *testing.T) {
		wb := &scriptedWorkbench{}
		e := newTestExecutor(wb, &fakeFleet{})
		req := exRequest()
		req.RunStartedAt = "yesterday"
		res, err := e.Execute(context.Background(), req)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		wantFailure(t, res, pl.OutcomeFailed, pl.CodeExecutorError)
		if len(wb.forwards) != 0 {
			t.Fatal("a step whose ceiling cannot be computed was forwarded")
		}
	})
}

// TestExecuteNamesTheBindingDeadline: the effective timeout is the lesser of
// the step's own and what is left of the run's ceiling, and DeadlineCode
// names whichever bound it is -- the code the step fails with when it passes.
func TestExecuteNamesTheBindingDeadline(t *testing.T) {
	for _, c := range []struct {
		name        string
		started     time.Duration // before exNow
		stepTimeout int
		needs       []string
		wantSeconds int
		wantCode    string
		runCeiling  time.Duration
	}{
		{"the step's own timeout binds", 10 * time.Minute, 900, nil, 900, pl.CodeStepTimeout, 0},
		{"the run's ceiling binds", 110 * time.Minute, 900, nil, 600, pl.CodeRunCeiling, 0},
		{"a step with no timeout gets the default", 10 * time.Minute, 0, nil, 1200, pl.CodeStepTimeout, 0},
		{"a tie is the step's own", 105 * time.Minute, 900, nil, 900, pl.CodeStepTimeout, 0},
		{"the fleet is handed the same bound", 110 * time.Minute, 900, []string{"docker"}, 600, pl.CodeRunCeiling, 0},
		{"a long step does not raise the default run ceiling", 10 * time.Minute, 21600, nil, 6600, pl.CodeRunCeiling, 0},
		{"an explicit long run admits a six-hour step", 10 * time.Minute, 21600, nil, 21600, pl.CodeStepTimeout, 8 * time.Hour},
		{"earlier stages consume the explicit long run budget", 7 * time.Hour, 21600, nil, 3600, pl.CodeRunCeiling, 8 * time.Hour},
	} {
		t.Run(c.name, func(t *testing.T) {
			wb := &scriptedWorkbench{step: func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
				return outcomeResponse(exOutcome("workbench-a")), "workbench-a", nil
			}}
			fleet := &fakeFleet{}
			e := newTestExecutor(wb, fleet)
			if c.runCeiling > 0 {
				e.cfg.RunCeiling = c.runCeiling
			}
			req := exRequest()
			req.RunStartedAt = exNow.Add(-c.started).Format(time.RFC3339)
			req.Step.TimeoutSeconds = c.stepTimeout
			if len(c.needs) > 0 {
				req.Compute, req.Step.Needs = pl.ComputeClusterAndFleet, c.needs
				req.Step.Execution, req.Step.Image = pl.ExecutionNative, ""
			}
			if _, err := e.Execute(context.Background(), req); err != nil {
				t.Fatalf("Execute: %v", err)
			}

			var run StepRun
			if len(c.needs) > 0 {
				runs := fleet.runs()
				if len(runs) != 1 {
					t.Fatalf("fleet runs = %d, want 1", len(runs))
				}
				run = runs[0]
			} else {
				steps := wb.sent(workbench.PipelineStepAction)
				if len(steps) != 1 {
					t.Fatalf("step forwards = %d, want 1", len(steps))
				}
				run = stepRunOf(t, steps[0])
				if got := steps[0].req.GetTimeoutSec(); int(got) != c.wantSeconds {
					t.Errorf("the forward's timeout hint = %d, want %d", got, c.wantSeconds)
				}
			}
			if run.TimeoutSeconds != c.wantSeconds || run.DeadlineCode != c.wantCode {
				t.Errorf("TimeoutSeconds, DeadlineCode = %d, %q; want %d, %q",
					run.TimeoutSeconds, run.DeadlineCode, c.wantSeconds, c.wantCode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The cluster path
// ---------------------------------------------------------------------------

// TestExecuteForwardsAClusterStepAndAcks: the step travels as a StepRun under
// the engine's own SYSTEM-class assertion, the runner's outcome is the step's,
// and the agent then acks so the Job and its Secret go now rather than at
// their TTL.
func TestExecuteRetainsItsResultUntilReceiptAcknowledgement(t *testing.T) {
	outcome := exOutcome("")
	outcome.Where.NodeID = "" // a runner that left it blank: the replica that answered is named
	wb := &scriptedWorkbench{step: func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
		return outcomeResponse(outcome), "workbench-b", nil
	}}
	e := newTestExecutor(wb, nil)
	req := exRequest()

	res, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != pl.OutcomeSucceeded || res.LogFileID != "file-log" || res.LogLines != 12 {
		t.Fatalf("result = %+v, want the runner's outcome", res)
	}
	if res.Where.Surface != "cluster" || res.Where.NodeID != "workbench-b" || res.Where.JobName != JobName(req.RunID, req.StepKey, req.Attempt) {
		t.Errorf("Where = %+v, want the cluster, the replica that answered and the step's Job", res.Where)
	}

	steps := wb.sent(workbench.PipelineStepAction)
	if len(steps) != 1 {
		t.Fatalf("step forwards = %d, want 1", len(steps))
	}
	f := steps[0]
	if !f.watched {
		t.Error("the step was forwarded unwatched: a replica that dies mid-step would leave the agent waiting out the deadline")
	}
	if f.pin != "" {
		t.Errorf("the first forward is pinned to %q; any healthy replica may create the Job", f.pin)
	}
	wantSystemAssertion(t, f.req)
	if f.req.GetRequestId() == "" {
		t.Error("the forward carries no request id")
	}
	if f.req.GetRunId() != req.RunID || f.req.GetStepId() != req.StepKey {
		t.Errorf("forward runId/stepId = %q/%q, want %q/%q", f.req.GetRunId(), f.req.GetStepId(), req.RunID, req.StepKey)
	}
	run := stepRunOf(t, f)
	want := stepRunFor(req, 900, pl.CodeStepTimeout)
	if run.RunID != want.RunID || run.StepKey != want.StepKey || run.Attempt != want.Attempt || run.Command != want.Command ||
		run.Secrets["NPM_TOKEN"] != plantedNPM || run.Env["MEMQL_RUN_ID"] != req.RunID {
		t.Errorf("the forwarded StepRun = %v, want %v", run, want)
	}

	if len(wb.sent(workbench.PipelineAckAction)) != 0 {
		t.Fatal("Execute discarded evidence before a durable receipt")
	}
	e.AcknowledgeReceipt(context.Background(), req)
	awaitCond(t, func() bool { return len(wb.sent(workbench.PipelineAckAction)) == 1 },
		"the outcome was never acked: the Job and its Secret would hold a slot of the ceiling until their TTL")
	ack := wb.sent(workbench.PipelineAckAction)[0]
	wantSystemAssertion(t, ack.req)
	var ackReq AckRequest
	if err := json.Unmarshal(ack.req.GetArgsJson(), &ackReq); err != nil || ackReq.JobName != JobName(req.RunID, req.StepKey, req.Attempt) {
		t.Errorf("ack args = %s (%v), want the step's Job name", ack.req.GetArgsJson(), err)
	}
	if ack.req.GetRequestId() == f.req.GetRequestId() {
		t.Error("the ack reuses the step forward's request id")
	}
	select {
	case <-f.ended:
		t.Error("the answered forward was cancelled")
	default:
	}
}

// ackEnd is how one step's ack ended, as Executor.onAcked is told.
type ackEnd struct {
	job      string
	attempts int
	err      error
}

// TestALostAckIsSentAgain (final review, M1): the ack is what deletes a
// finished step's Job and its Secret now, and a finished Job counts against
// the pipelines ceiling's quota until it is deleted -- so an ack lost to a
// stream that dropped mid-answer would hold a slot a queued step waits for,
// until the Job's TTL. It is sent again, each time under a fresh request id
// and the engine's own assertion, until a replica confirms it -- and only so
// many times.
func TestALostAckIsSentAgain(t *testing.T) {
	run := func(t *testing.T, ack func(n int) (*nodev1.WorkbenchForwardResponse, string, error)) (ackEnd, []*exForward) {
		t.Helper()
		var n atomic.Int32
		wb := &scriptedWorkbench{
			step: func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
				return outcomeResponse(exOutcome("workbench-a")), "workbench-a", nil
			},
			ack: func(*exForward) (*nodev1.WorkbenchForwardResponse, string, error) { return ack(int(n.Add(1))) },
		}
		e := newTestExecutor(wb, nil)
		ended := make(chan ackEnd, 1)
		e.onAcked = func(job string, attempts int, err error) { ended <- ackEnd{job, attempts, err} }
		if res, err := e.Execute(context.Background(), exRequest()); err != nil || res.Status != pl.OutcomeSucceeded {
			t.Fatalf("Execute = %+v, %v; want the runner's outcome", res, err)
		}
		e.AcknowledgeReceipt(context.Background(), exRequest())
		select {
		case end := <-ended:
			return end, wb.sent(workbench.PipelineAckAction)
		case <-time.After(5 * time.Second):
			t.Fatal("the ack never ended")
			return ackEnd{}, nil
		}
	}
	job := JobName(exRequest().RunID, exRequest().StepKey, exRequest().Attempt)

	t.Run("lost, then refused, then confirmed", func(t *testing.T) {
		end, sent := run(t, func(n int) (*nodev1.WorkbenchForwardResponse, string, error) {
			switch n {
			case 1:
				return nil, "workbench-a", context.DeadlineExceeded // the stream dropped mid-answer
			case 2:
				return &nodev1.WorkbenchForwardResponse{ErrorCode: "ack_failed", ErrorMessage: "the API server did not answer"}, "workbench-b", nil
			}
			return &nodev1.WorkbenchForwardResponse{}, "workbench-b", nil
		})
		if end.err != nil || end.attempts != 3 || end.job != job {
			t.Fatalf("the ack ended %+v, want confirmed for %s on its third send", end, job)
		}
		if len(sent) != 3 {
			t.Fatalf("%d acks were sent, want 3: two that went unconfirmed and the one a replica confirmed", len(sent))
		}
		ids := map[string]bool{}
		for i, f := range sent {
			wantSystemAssertion(t, f.req)
			var ar AckRequest
			if err := json.Unmarshal(f.req.GetArgsJson(), &ar); err != nil || ar.JobName != job {
				t.Errorf("ack %d names %s (%v), want the step's Job %s", i+1, f.req.GetArgsJson(), err, job)
			}
			ids[f.req.GetRequestId()] = true
		}
		if len(ids) != 3 {
			t.Errorf("the acks went under %d request ids, want 3: a repeat under one id collides in the router's in-flight table", len(ids))
		}
	})

	t.Run("never confirmed: sent ackAttempts times, then left to the TTL", func(t *testing.T) {
		end, sent := run(t, func(int) (*nodev1.WorkbenchForwardResponse, string, error) {
			return nil, "", workbench.ErrNoWorkbenchPeer
		})
		if end.err == nil || end.attempts != ackAttempts {
			t.Fatalf("the ack ended %+v, want unconfirmed after %d sends", end, ackAttempts)
		}
		if len(sent) != ackAttempts {
			t.Errorf("%d acks were sent, want exactly %d: the retry is bounded", len(sent), ackAttempts)
		}
	})
}

// wantSystemAssertion: every pipeline forward carries the engine's own
// SYSTEM-class assertion -- the workbench refuses the four actions under any
// other class -- naming no person.
func wantSystemAssertion(t *testing.T, req *nodev1.WorkbenchForwardRequest) {
	t.Helper()
	a := node.ForwardedAuthorityFromProto(req.GetAuthority())
	if _, err := auth.VerifyForwardedAuthority(a, time.Now()); err != nil {
		t.Fatalf("%s: the assertion does not verify: %v", req.GetAction(), err)
	}
	if a.CredentialClass != auth.ForwardedClassSystem || a.Kind != auth.ForwardedPrincipalSystem {
		t.Errorf("%s: assertion class %q kind %v, want the system class", req.GetAction(), a.CredentialClass, a.Kind)
	}
	if strings.HasPrefix(a.Subject, "v1:identity:user:") || a.Subject == "" {
		t.Errorf("%s: assertion subject %q, want the engine, not a person", req.GetAction(), a.Subject)
	}
	if a.OriginNodeId != exAgent {
		t.Errorf("%s: assertion origin %q, want this node %q", req.GetAction(), a.OriginNodeId, exAgent)
	}
}

// TestExecuteTakesAFinishedStatusWhenTheReplyWasLost: a stream flap is
// invisible to the watched forward -- the request rode the reconnecting
// outbox and the reply on the old attempt is gone -- so the step's own status,
// read every poll, is the only way the outcome is ever seen. Finished is
// taken as the answer while the forward is still waiting; the forward is
// abandoned, never cancelled to get there.
func TestExecuteTakesAFinishedStatusWhenTheReplyWasLost(t *testing.T) {
	persisted := exOutcome("workbench-a")
	persisted.Status, persisted.ExitCode = pl.OutcomeFailed, 3
	var cancelledFirst bool
	wb := &scriptedWorkbench{}
	wb.status = func(n int, _ *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
		if n < 3 {
			return stateResponse(StateRunning), "workbench-b", nil
		}
		select {
		case <-wb.sent(workbench.PipelineStepAction)[0].ended:
			cancelledFirst = true
		default:
		}
		return finishedResponse(persisted), "workbench-b", nil
	}
	e := newTestExecutor(wb, nil)
	req := exRequest()

	res := awaitResult(t, executeAndCommitAsync(e, context.Background(), req), "the status said finished")
	if res.Status != pl.OutcomeFailed || res.ExitCode != 3 || res.LogFileID != "file-log" {
		t.Fatalf("result = %+v, want the persisted outcome the status carried", res)
	}
	if cancelledFirst {
		t.Fatal("the step forward was cancelled before its outcome was taken from the status. Cancelling is how " +
			"a forward stops waiting AND how the runner is told to delete the Job.")
	}
	if n := len(wb.sent(workbench.PipelineStepAction)); n != 1 {
		t.Errorf("step forwards = %d, want 1: a finished step is never forwarded again", n)
	}
	for _, s := range wb.sent(workbench.PipelineStatusAction) {
		wantSystemAssertion(t, s.req)
		var sr StatusRequest
		if err := json.Unmarshal(s.req.GetArgsJson(), &sr); err != nil || sr.JobName != JobName(req.RunID, req.StepKey, req.Attempt) {
			t.Fatalf("status args = %s, want the step's Job name", s.req.GetArgsJson())
		}
	}
	awaitCond(t, func() bool { return len(wb.sent(workbench.PipelineAckAction)) == 1 },
		"a finished status was taken and never acked")
}

// TestExecuteReforwardsWhenTheReplicaIsLostOrStale: the two signals that the
// replica running a step is gone -- the watched forward's own, and a stale
// heartbeat on the Job -- each send the step again, unpinned, under a fresh
// request id, so another replica adopts the Job by its name.
func TestExecuteReforwardsWhenTheReplicaIsLostOrStale(t *testing.T) {
	t.Run("the watched forward lost its replica", func(t *testing.T) {
		wb := &scriptedWorkbench{}
		wb.step = func(n int, _ *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
			if n == 1 {
				return nil, "workbench-a", workbench.ErrWorkbenchPeerLost
			}
			return outcomeResponse(exOutcome("workbench-b")), "workbench-b", nil
		}
		e := newTestExecutor(wb, nil)
		res, err := e.Execute(context.Background(), exRequest())
		if err != nil || res.Status != pl.OutcomeSucceeded || res.Where.NodeID != "workbench-b" {
			t.Fatalf("Execute = %+v, %v; want the adopting replica's outcome", res, err)
		}
		wantReforwarded(t, wb)
	})

	t.Run("the Job's heartbeat went stale", func(t *testing.T) {
		wb := &scriptedWorkbench{}
		var clock *exClock
		wb.status = func(n int, _ *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
			if n == 1 {
				// The patience runs out exactly once, on the first reading.
				clock.advance(time.Minute)
			}
			return staleResponse("workbench-a"), "workbench-b", nil
		}
		wb.step = func(n int, f *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
			if n == 1 {
				return nil, "", nil // waits on its context, as a forward to a wedged replica does
			}
			return outcomeResponse(exOutcome("workbench-b")), "workbench-b", nil
		}
		e := newTestExecutor(wb, nil)
		clock = withClock(e)
		e.stalePatience = time.Minute
		res := awaitResult(t, executeAndCommitAsync(e, context.Background(), exRequest()), "the stale Job was forwarded again")
		if res.Status != pl.OutcomeSucceeded {
			t.Fatalf("result = %+v; want the adopting replica's outcome", res)
		}
		wantReforwarded(t, wb)
		// AWAY FROM the replica whose runner went quiet: the status named it.
		if steps := wb.sent(workbench.PipelineStepAction); steps[0].exclude != "" || steps[1].exclude != "workbench-a" {
			t.Errorf("excluded %q then %q, want nothing on the first forward and workbench-a -- where the runner "+
				"went quiet -- on the re-forward", steps[0].exclude, steps[1].exclude)
		}
	})

	t.Run("a stale reading inside the patience is the adoption on its way", func(t *testing.T) {
		// A replica adopting the Job proves isolation, then annotates it; a
		// stale reading inside that window is the adoption still on its way,
		// not a second loss -- so no further forward within the patience. The
		// patience is the executor's clock, which this test alone moves, so
		// no amount of load on the machine running it can end it early.
		polls := make(chan struct{}, 1024)
		wb := &scriptedWorkbench{}
		wb.status = func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
			polls <- struct{}{}
			return staleResponse("workbench-a"), "workbench-b", nil
		}
		released := make(chan struct{})
		wb.step = func(n int, f *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
			if n == 1 {
				return nil, "", nil
			}
			<-released
			return outcomeResponse(exOutcome("workbench-b")), "workbench-b", nil
		}
		e := newTestExecutor(wb, nil)
		clock := withClock(e)
		e.stalePatience = time.Minute
		e.statusEvery = 2 * time.Millisecond
		done := executeAndCommitAsync(e, context.Background(), exRequest())
		awaitPolls(t, polls, 10)
		if n := len(wb.sent(workbench.PipelineStepAction)); n != 1 {
			t.Fatalf("step forwards = %d after ten stale readings inside the patience, want 1", n)
		}
		// Past the patience the stale readings are believed: one re-forward,
		// and none after it while the clock stands still again.
		clock.advance(time.Minute)
		awaitCond(t, func() bool { return len(wb.sent(workbench.PipelineStepAction)) == 2 },
			"stale readings past the patience never re-forwarded the step")
		awaitPolls(t, polls, 10)
		if n := len(wb.sent(workbench.PipelineStepAction)); n != 2 {
			t.Fatalf("step forwards = %d: the re-forward was itself forwarded again inside its own patience", n)
		}
		close(released)
		awaitResult(t, done, "the re-forward answered")
	})
}

// TestExecuteKeepsAvoidingTheQuietReplicaWhenAReforwardFails: a stale
// re-forward that fails itself -- the replica it reached went away, none was
// reachable for a moment, the send failed -- is tried again, and the retry
// still steers away from the replica whose runner went quiet. A retry that
// dropped the exclusion would hand the step straight back to it.
func TestExecuteKeepsAvoidingTheQuietReplicaWhenAReforwardFails(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
	}{
		{"the replica the re-forward reached went away", workbench.ErrWorkbenchPeerLost},
		{"no replica was reachable for a moment", workbench.ErrNoWorkbenchPeer},
		{"the send failed", errors.New("stream reset")},
	} {
		t.Run(c.name, func(t *testing.T) {
			wb := &scriptedWorkbench{}
			var clock *exClock
			wb.status = func(n int, _ *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
				if n == 1 {
					// The patience runs out exactly once, on the first reading.
					clock.advance(time.Minute)
				}
				return staleResponse("workbench-a"), "workbench-b", nil
			}
			wb.step = func(n int, _ *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
				switch n {
				case 1:
					return nil, "", nil // waits on its context, as a forward to a wedged replica does
				case 2:
					return nil, "workbench-b", c.err // the stale re-forward fails
				}
				return outcomeResponse(exOutcome("workbench-b")), "workbench-b", nil
			}
			e := newTestExecutor(wb, nil)
			clock = withClock(e)
			e.stalePatience = time.Minute

			res := awaitResult(t, executeAndCommitAsync(e, context.Background(), exRequest()), "the retried re-forward answered")
			if res.Status != pl.OutcomeSucceeded || res.Where.NodeID != "workbench-b" {
				t.Fatalf("result = %+v; want the adopting replica's outcome", res)
			}
			steps := wb.sent(workbench.PipelineStepAction)
			if len(steps) != 3 {
				t.Fatalf("step forwards = %d, want 3: the first, the stale re-forward and its retry", len(steps))
			}
			for i, want := range []string{"", "workbench-a", "workbench-a"} {
				if steps[i].exclude != want {
					t.Errorf("forward %d excluded %q, want %q: the retry of a failed stale re-forward still avoids the "+
						"replica whose runner went quiet", i+1, steps[i].exclude, want)
				}
			}
		})
	}
}

// awaitPolls waits for n more status readings.
func awaitPolls(t *testing.T, polls <-chan struct{}, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-polls:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d status readings arrived", i, n)
		}
	}
}

func staleResponse(runner string) *nodev1.WorkbenchForwardResponse {
	return &nodev1.WorkbenchForwardResponse{PayloadJson: mustMarshal(StatusReply{State: StateStale, Runner: runner})}
}

// TestExecuteNamesTheRunsDeadline (ruling R31b): every forward of a step --
// the first and each re-forward -- carries when its run reaches its ceiling,
// computed once on the agent from the run's start and the ceiling the driver
// reads too, so the runner bounds the step's wait for a slot by the moment
// the agent waits on it until. The step's own timeout travels whole: it runs
// from the step's Job's creation, so nothing of it is spent before.
func TestExecuteNamesTheRunsDeadline(t *testing.T) {
	wb := &scriptedWorkbench{}
	var clock *exClock
	wb.step = func(n int, _ *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
		clock.advance(time.Minute) // each forward goes later than the one before
		if n == 1 {
			return nil, "workbench-a", workbench.ErrWorkbenchPeerLost
		}
		return outcomeResponse(exOutcome("workbench-b")), "workbench-b", nil
	}
	e := newTestExecutor(wb, nil)
	clock = withClock(e)
	// The run started ten minutes before exNow, under a two-hour ceiling.
	want := exNow.Add(110 * time.Minute).UTC().Format(time.RFC3339Nano)

	if res, err := e.Execute(context.Background(), exRequest()); err != nil || res.Status != pl.OutcomeSucceeded {
		t.Fatalf("Execute = %+v, %v; want the adopting replica's outcome", res, err)
	}
	steps := wb.sent(workbench.PipelineStepAction)
	if len(steps) != 2 {
		t.Fatalf("step forwards = %d, want 2", len(steps))
	}
	for i, f := range steps {
		run := stepRunOf(t, f)
		if run.RunDeadline != want {
			t.Errorf("forward %d names the run's deadline %q, want %q: its start plus its ceiling", i+1, run.RunDeadline, want)
		}
		if run.TimeoutSeconds != 900 {
			t.Errorf("forward %d gives the step %ds, want its whole 900", i+1, run.TimeoutSeconds)
		}
		if strings.Contains(string(f.req.GetArgsJson()), "handedAt") {
			t.Errorf("forward %d still names a hand-over moment: nothing counts the step's timeout from it", i+1)
		}
	}
}

// TestExecuteReforwardsAStepWhoseJobStaysAbsent: a Job no runner has made
// long after the forward is a request that may never have reached one (lost
// to a stream drop on the way), so the step is forwarded again -- a runner
// finds or creates the Job by its name. An absent Job is most often one
// waiting for a free slot under the ceiling, which another forward cannot
// speed up, so each forward an absence causes waits twice as long.
func TestExecuteReforwardsAStepWhoseJobStaysAbsent(t *testing.T) {
	polls := make(chan struct{}, 1024)
	wb := &scriptedWorkbench{}
	wb.status = func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
		polls <- struct{}{}
		return stateResponse(StateAbsent), "workbench-b", nil
	}
	released := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(released) }) })
	wb.step = func(n int, f *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
		if n < 3 {
			return nil, "", nil // lost on the way: nothing answers it
		}
		// The third forward reaches a runner, which answers once the test
		// has seen it sent -- so the step stays in flight, and polled, until
		// then.
		<-released
		return outcomeResponse(exOutcome("workbench-a")), "workbench-a", nil
	}
	e := newTestExecutor(wb, nil)
	clock := withClock(e)
	e.stalePatience = time.Minute
	e.statusEvery = 2 * time.Millisecond
	done := executeAndCommitAsync(e, context.Background(), exRequest())
	steps := func() int { return len(wb.sent(workbench.PipelineStepAction)) }

	awaitPolls(t, polls, 10)
	if n := steps(); n != 1 {
		t.Fatalf("step forwards = %d inside the patience, want 1: an absent Job is first a runner minting, proving or queueing", n)
	}
	clock.advance(time.Minute)
	awaitCond(t, func() bool { return steps() == 2 }, "a Job absent past the patience was never forwarded again")

	// The second forward waits twice as long: one patience is not enough.
	clock.advance(time.Minute)
	awaitPolls(t, polls, 10)
	if n := steps(); n != 2 {
		t.Fatalf("step forwards = %d one patience after the re-forward, want 2: each absence-driven forward waits twice as long", n)
	}
	clock.advance(time.Minute)
	awaitCond(t, func() bool { return steps() == 3 }, "the absence was never forwarded again after twice the patience")
	release.Do(func() { close(released) })

	res := awaitResult(t, done, "the third forward answered")
	if res.Status != pl.OutcomeSucceeded {
		t.Fatalf("result = %+v, want the outcome the runner reached on the third forward", res)
	}
	for i, f := range wb.sent(workbench.PipelineStepAction) {
		if f.exclude != "" || f.pin != "" {
			t.Errorf("forward %d pinned %q excluding %q; an absent Job names no replica to avoid", i+1, f.pin, f.exclude)
		}
		if a, b := stepRunOf(t, f), stepRunOf(t, wb.sent(workbench.PipelineStepAction)[0]); a.Attempt != b.Attempt || a.StepKey != b.StepKey {
			t.Errorf("forward %d names another attempt or step: a runner would create a second Job", i+1)
		}
	}
}

func wantReforwarded(t *testing.T, wb *scriptedWorkbench) {
	t.Helper()
	steps := wb.sent(workbench.PipelineStepAction)
	if len(steps) != 2 {
		t.Fatalf("step forwards = %d, want 2: the first, and one re-forward", len(steps))
	}
	if steps[1].pin != "" {
		t.Errorf("the re-forward is pinned to %q; it must go to any healthy replica", steps[1].pin)
	}
	if steps[0].req.GetRequestId() == steps[1].req.GetRequestId() {
		t.Error("the re-forward reuses the first request id: the router only fills an empty one, and two " +
			"forwards under one id collide in its in-flight table and in the handler's")
	}
	if a, b := stepRunOf(t, steps[0]), stepRunOf(t, steps[1]); a.Attempt != b.Attempt || a.StepKey != b.StepKey {
		t.Error("the re-forward names another attempt or step: the adopting replica would create a second Job")
	}
	wantSystemAssertion(t, steps[1].req)
}

// TestExecuteReadsAWorkbenchRefusal: an answer that is not an outcome ends the
// step with a typed failure rather than a hang or a guess.
func TestExecuteReadsAWorkbenchRefusal(t *testing.T) {
	for _, c := range []struct {
		name   string
		resp   *nodev1.WorkbenchForwardResponse
		status pl.Outcome
		code   string
	}{
		{"a replica with no runner", &nodev1.WorkbenchForwardResponse{
			ErrorCode: workbench.ErrCodePipelinesNotConfigured, ErrorMessage: "no runner"}, pl.OutcomeFailed, pl.CodeRunnerUnavailable},
		{"a refused assertion", &nodev1.WorkbenchForwardResponse{
			ErrorCode: "forwarded_authority_refused", ErrorMessage: "class"}, pl.OutcomeFailed, pl.CodeExecutorError},
		{"a payload that is not an outcome", &nodev1.WorkbenchForwardResponse{
			PayloadJson: []byte(`{"state":"running"`)}, pl.OutcomeFailed, pl.CodeExecutorError},
		{"an outcome with no status", &nodev1.WorkbenchForwardResponse{
			PayloadJson: []byte(`{"exitCode":0}`)}, pl.OutcomeFailed, pl.CodeExecutorError},
	} {
		t.Run(c.name, func(t *testing.T) {
			wb := &scriptedWorkbench{step: func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
				return c.resp, "workbench-a", nil
			}}
			e := newTestExecutor(wb, nil)
			res, err := e.Execute(context.Background(), exRequest())
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			wantFailure(t, res, c.status, c.code)
		})
	}

	t.Run("an agent with no route to a workbench", func(t *testing.T) {
		e := newTestExecutor(nil, nil)
		res, err := e.Execute(context.Background(), exRequest())
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		wantFailure(t, res, pl.OutcomeFailed, pl.CodeRunnerUnavailable)
	})
}

// stillWaiting waits for two status readings that began after it was called
// -- whatever was read before is drained first -- which is one whole turn of
// the executor's loop, its give-up check and then its reading, on the clock
// as the caller left it. It fails the test if the step ended instead: it was
// given up too early.
func stillWaiting(t *testing.T, polls chan struct{}, done <-chan pl.StepResult, why string) {
	t.Helper()
	for drained := false; !drained; {
		select {
		case <-polls:
		default:
			drained = true
		}
	}
	for seen := 0; seen < 2; {
		select {
		case <-polls:
			seen++
		case res := <-done:
			t.Fatalf("%s, but it was given up: %+v (failure %+v)", why, res, res.Failure)
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 2 status readings arrived", seen)
		}
	}
}

// runningReply is a status of a step whose runner holds it: with its Job's
// creation time, or none -- the runner is waiting for a free slot under the
// ceiling, and no Job exists yet.
func runningReply(created string) *nodev1.WorkbenchForwardResponse {
	return &nodev1.WorkbenchForwardResponse{PayloadJson: mustMarshal(StatusReply{State: StateRunning, Runner: "workbench-a", JobCreatedAt: created})}
}

// TestExecuteWaitsForAQueuedStepUntilItsRunsCeiling (ruling R31b, Review
// Focus 4): a step whose Job does not exist yet -- its runner holds it,
// waiting for a free slot under the ceiling: running, with no creation time
// -- is waited on past its own timeout and the grace, until its run's ceiling
// and the grace past it. Then one last look, and the step fails
// pipeline_run_ceiling, never started; its Secret is deleted with the ack.
func TestExecuteWaitsForAQueuedStepUntilItsRunsCeiling(t *testing.T) {
	polls := make(chan struct{}, 1024)
	wb := &scriptedWorkbench{}
	wb.status = func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
		polls <- struct{}{}
		return runningReply(""), "workbench-a", nil
	}
	e := newTestExecutor(wb, nil)
	clock := withClock(e)
	e.lostGrace = time.Minute
	e.statusEvery = 2 * time.Millisecond
	// Its own timeout is fifteen minutes; its run's ceiling is 110 away.
	done := executeAndCommitAsync(e, context.Background(), exRequest())
	awaitPolls(t, polls, 1) // handed over, on the clock as it stands

	clock.set(exNow.Add(15*time.Minute + time.Minute + time.Second))
	stillWaiting(t, polls, done, "past its own timeout and the grace, where R31 gave it up, a step waiting for a slot is still waited on")
	clock.set(exNow.Add(110*time.Minute + time.Minute - time.Second))
	stillWaiting(t, polls, done, "a second short of its run's ceiling and the grace, a step waiting for a slot is still waited on")

	clock.advance(2 * time.Second)
	res := awaitResult(t, done, "its run's ceiling and the grace passed")
	wantFailure(t, res, pl.OutcomeFailed, pl.CodeRunCeiling)
	if !strings.Contains(res.Failure.Message, "never started") || !strings.Contains(res.Failure.Message, "2h0m0s ceiling") {
		t.Errorf("failure %q, want it to say the step never started before its run's 2h0m0s ceiling", res.Failure.Message)
	}
	awaitCond(t, func() bool { return len(wb.sent(workbench.PipelineAckAction)) == 1 },
		"the step was given up on and its Secret left: the give-up must ack")
}

// TestExecuteGivesUpOnACreatedJobByItsCreation (ruling R31b): once its Job
// exists, a step is given up its own timeout and the grace after the Job's
// CREATION -- which the runner reports with the step's status -- however long
// it queued before, and never past its run's ceiling and the grace. The
// give-up moves with the creation time.
func TestExecuteGivesUpOnACreatedJobByItsCreation(t *testing.T) {
	for _, c := range []struct {
		name     string
		created  time.Duration // after exNow, when the step was handed over
		giveUpAt time.Duration // after exNow
		code     string
	}{
		{"a Job created at once: its timeout and the grace from then", 0, 15*time.Minute + time.Minute, pl.CodeStepTimeout},
		{"a Job created after half an hour in the queue: the same, from then", 30 * time.Minute, 46 * time.Minute, pl.CodeStepTimeout},
		{"a Job created near its run's ceiling: never past it and the grace", 100 * time.Minute, 111 * time.Minute, pl.CodeRunCeiling},
		// A creation time ahead of the hand-over on this clock is a clock
		// that cannot vouch for it: the hand-over bounds it.
		{"a Job reported created before the step was handed over: from the hand-over", -time.Hour, 16 * time.Minute, pl.CodeStepTimeout},
	} {
		t.Run(c.name, func(t *testing.T) {
			polls := make(chan struct{}, 1024)
			created := exNow.Add(c.created).UTC().Format(time.RFC3339)
			wb := &scriptedWorkbench{}
			wb.status = func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
				polls <- struct{}{}
				return runningReply(created), "workbench-a", nil
			}
			e := newTestExecutor(wb, nil)
			clock := withClock(e)
			e.lostGrace = time.Minute
			e.statusEvery = 2 * time.Millisecond
			done := executeAndCommitAsync(e, context.Background(), exRequest())
			awaitPolls(t, polls, 1) // handed over at exNow

			clock.set(exNow.Add(c.giveUpAt - time.Second))
			stillWaiting(t, polls, done, "a second short of its give-up, the step is still waited on")
			clock.advance(2 * time.Second)
			res := awaitResult(t, done, "the Job's deadline and the grace passed")
			wantFailure(t, res, pl.OutcomeFailed, c.code)
			if !strings.Contains(res.Failure.Message, "settling overran") {
				t.Errorf("failure %q, want it to say the runner still held the step: settling overran", res.Failure.Message)
			}
		})
	}
}

// TestExecuteGivesUpPastTheDeadline: nothing waits forever. Past the bound it
// waits for -- its run's ceiling while no Job exists, its Job's deadline once
// one does -- and the grace beyond it, one last status decides what the step
// is reported as, and the Job and its Secret, if they are still there, are
// deleted.
func TestExecuteGivesUpPastTheDeadline(t *testing.T) {
	created := exNow.UTC().Format(time.RFC3339)
	for _, c := range []struct {
		name string
		// look answers the n-th status: the first is the executor's own
		// reading, the second the give-up's last look.
		look func(n int) (*nodev1.WorkbenchForwardResponse, string, error)
		want pl.Outcome
		code string
		says string // what the failure says
	}{
		{"no replica answers for the step", func(int) (*nodev1.WorkbenchForwardResponse, string, error) {
			return nil, "", workbench.ErrNoWorkbenchPeer
		}, pl.OutcomeFailed, pl.CodeNodeLost, "went away"},
		{"the Job's runner went quiet", func(int) (*nodev1.WorkbenchForwardResponse, string, error) {
			return &nodev1.WorkbenchForwardResponse{PayloadJson: mustMarshal(StatusReply{State: StateStale, Runner: "workbench-a", JobCreatedAt: created})}, "workbench-b", nil
		}, pl.OutcomeFailed, pl.CodeNodeLost, "went away"},
		{"the Job's runner still holds it: settling overran", func(int) (*nodev1.WorkbenchForwardResponse, string, error) {
			return runningReply(created), "workbench-b", nil
		}, pl.OutcomeFailed, pl.CodeStepTimeout, "settling overran"},
		{"the step never left the queue", func(int) (*nodev1.WorkbenchForwardResponse, string, error) {
			return runningReply(""), "workbench-b", nil
		}, pl.OutcomeFailed, pl.CodeRunCeiling, "never started"},
		{"no Job was ever made for it", func(int) (*nodev1.WorkbenchForwardResponse, string, error) {
			return stateResponse(StateAbsent), "workbench-b", nil
		}, pl.OutcomeFailed, pl.CodeRunCeiling, "never started"},
		{"the last look found the outcome", func(n int) (*nodev1.WorkbenchForwardResponse, string, error) {
			if n == 1 {
				return runningReply(created), "workbench-b", nil
			}
			return finishedResponse(exOutcome("workbench-a")), "workbench-b", nil
		}, pl.OutcomeSucceeded, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			wb := &scriptedWorkbench{}
			e := newTestExecutor(wb, nil)
			clock := withClock(e)
			e.lostGrace = time.Minute
			e.stalePatience = 24 * time.Hour // a stale or absent reading forwards nothing again here
			wb.status = func(n int, _ *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
				if n == 1 {
					// The executor's first reading: past every bound it
					// waits for, and the grace, from here on.
					clock.set(exNow.Add(110*time.Minute + time.Minute + time.Second))
				}
				return c.look(n)
			}

			res := awaitResult(t, executeAndCommitAsync(e, context.Background(), exRequest()), "every bound and the grace passed")
			if c.code == "" {
				if res.Status != c.want {
					t.Fatalf("result = %+v, want %s", res, c.want)
				}
				return
			}
			wantFailure(t, res, c.want, c.code)
			if !strings.Contains(res.Failure.Message, c.says) {
				t.Errorf("failure %q, want it to say %q", res.Failure.Message, c.says)
			}
			if c.says == "settling overran" && strings.Contains(res.Failure.Message, "free slot") {
				t.Errorf("failure %q blames a wait for a slot, which its Job's deadline never counts", res.Failure.Message)
			}
			awaitCond(t, func() bool { return len(wb.sent(workbench.PipelineAckAction)) == 1 },
				"the step was given up on and its Job left to run: the give-up must delete it")
		})
	}

	t.Run("no workbench replica for longer than the patience", func(t *testing.T) {
		var clock *exClock
		none := func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
			return nil, "", workbench.ErrNoWorkbenchPeer
		}
		step := func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
			clock.advance(10 * time.Second) // each try costs the executor's clock ten seconds
			return nil, "", workbench.ErrNoWorkbenchPeer
		}
		wb := &scriptedWorkbench{step: step, status: none}
		e := newTestExecutor(wb, nil)
		clock = withClock(e)
		e.noPeerPatience = 30 * time.Second
		res := awaitResult(t, executeAndCommitAsync(e, context.Background(), exRequest()), "no replica was ever reachable")
		wantFailure(t, res, pl.OutcomeFailed, pl.CodeRunnerUnavailable)
		if n := len(wb.sent(workbench.PipelineStepAction)); n < 2 {
			t.Errorf("step forwards = %d: a replica missing for a moment (a rolling deploy) must be tried again", n)
		}
	})
}

// TestExecuteReadsTheDriversDeadlineAsTheRunsCeiling (ruling R31b): the
// driver hands a step a context that ends at its run's ceiling and the grace
// past it, so a step whose context ended so failed pipeline_run_ceiling,
// whichever bound its own timeout was.
func TestExecuteReadsTheDriversDeadlineAsTheRunsCeiling(t *testing.T) {
	wb := &scriptedWorkbench{}
	e := newTestExecutor(wb, nil)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	req := exRequest() // its own timeout binds: the StepRun names pipeline_step_timeout

	res := awaitResult(t, executeAndCommitAsync(e, ctx, req), "the driver's deadline had passed")

	wantFailure(t, res, pl.OutcomeFailed, pl.CodeRunCeiling)
	if res.Failure.Message != driverDeadlineMessage {
		t.Errorf("failure %q, want the driver's own sentence %q", res.Failure.Message, driverDeadlineMessage)
	}
}

// ---------------------------------------------------------------------------
// Cancel
// ---------------------------------------------------------------------------

// TestCancelReachesInFlightStepsAndDeletesJobs: a run's cancel stops every
// step of it this node has in flight -- forwarded and fleet alike -- and asks a
// workbench to delete the run's Jobs by label, which reaches the Jobs other
// replicas hold too. Another run's steps are untouched.
func TestCancelReachesInFlightStepsAndDeletesJobs(t *testing.T) {
	wb := &scriptedWorkbench{}
	fleet := &fakeFleet{block: true}
	e := newTestExecutor(wb, fleet)
	e.statusEvery = time.Hour

	first := exRequest()
	second := exRequest()
	second.StepKey, second.Step.Key = "tests.go-tests#3", "tests.go-tests#3"
	onFleet := exRequest()
	onFleet.StepKey, onFleet.Step.Key = "tests.docker", "tests.docker"
	onFleet.Compute, onFleet.Step.Needs = pl.ComputeClusterAndFleet, []string{"docker"}
	onFleet.Step.Execution, onFleet.Step.Image = pl.ExecutionNative, ""
	other := exRequest()
	other.RunID = "run-9b9b"

	ctx := context.Background()
	otherCtx, stopOther := context.WithCancel(ctx)
	defer stopOther()
	r1, r2, r3, r4 := executeAndCommitAsync(e, ctx, first), executeAndCommitAsync(e, ctx, second), executeAndCommitAsync(e, ctx, onFleet), executeAndCommitAsync(e, otherCtx, other)
	awaitCond(t, func() bool { return len(wb.sent(workbench.PipelineStepAction)) == 3 && len(fleet.runs()) == 1 },
		"the four steps never all started")

	// The first attempt at the run-wide delete is lost to a stream drop; the
	// cancel is retried, because a run's Jobs left behind hold slots until
	// their deadline.
	wb.mu.Lock()
	wb.cancel = func(n int, _ *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
		if n == 1 {
			return nil, "workbench-a", context.DeadlineExceeded
		}
		return &nodev1.WorkbenchForwardResponse{PayloadJson: []byte(`{"jobsDeleted":2}`)}, "workbench-b", nil
	}
	wb.mu.Unlock()

	cctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	if err := e.Cancel(cctx, first.RunID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	for _, ch := range []<-chan pl.StepResult{r1, r2, r3} {
		res := awaitResult(t, ch, "a step of the cancelled run")
		if res.Status != pl.OutcomeCancelled || res.Failure == nil || res.Failure.Code != pl.CodeStepCancelled {
			t.Errorf("a step of the cancelled run ended %+v (failure %+v), want cancelled / %s", res, res.Failure, pl.CodeStepCancelled)
		}
	}
	steps := wb.sent(workbench.PipelineStepAction)
	var others []*exForward
	for _, f := range steps {
		run := stepRunOf(t, f)
		if run.RunID != first.RunID {
			others = append(others, f)
			continue
		}
		select {
		case <-f.ended:
		case <-time.After(5 * time.Second):
			t.Errorf("the forward of %s was never cancelled: the replica running it keeps its Job", run.StepKey)
		}
	}
	if len(others) != 1 {
		t.Fatalf("the other run's step forwards = %d, want 1", len(others))
	}
	select {
	case <-others[0].ended:
		t.Error("the other run's step forward was cancelled")
	default:
	}
	select {
	case res := <-r4:
		t.Fatalf("the other run's step ended (%+v) when only the first run was cancelled", res)
	default:
	}

	cancels := wb.sent(workbench.PipelineCancelAction)
	if len(cancels) != 2 {
		t.Fatalf("pipelineCancel forwards = %d, want 2: the lost one and its retry", len(cancels))
	}
	for _, c := range cancels {
		wantSystemAssertion(t, c.req)
		var cr CancelRequest
		if err := json.Unmarshal(c.req.GetArgsJson(), &cr); err != nil || cr.RunID != first.RunID {
			t.Errorf("cancel args = %s, want the run id %q", c.req.GetArgsJson(), first.RunID)
		}
		if c.watched {
			t.Error("the run-wide delete was sent watched; it answers at once")
		}
	}
	if cancels[0].req.GetRequestId() == cancels[1].req.GetRequestId() {
		t.Error("the retried cancel reuses the lost one's request id")
	}

	t.Run("a run with nothing in flight is still deleted by label", func(t *testing.T) {
		wb := &scriptedWorkbench{}
		e := newTestExecutor(wb, nil)
		if err := e.Cancel(context.Background(), "run-idle"); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if n := len(wb.sent(workbench.PipelineCancelAction)); n != 1 {
			t.Fatalf("pipelineCancel forwards = %d, want 1: a predecessor's Jobs may still run on some replica", n)
		}
	})

	t.Run("a cancel no workbench ever answers reports it", func(t *testing.T) {
		wb := &scriptedWorkbench{cancel: func(int, *exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
			return nil, "", workbench.ErrNoWorkbenchPeer
		}}
		e := newTestExecutor(wb, nil)
		ctx, done := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer done()
		if err := e.Cancel(ctx, "run-idle"); err == nil {
			t.Fatal("Cancel = nil, though no workbench deleted anything; the driver logs this")
		}
	})

	t.Run("a blank run id is refused", func(t *testing.T) {
		wb := &scriptedWorkbench{}
		e := newTestExecutor(wb, nil)
		if err := e.Cancel(context.Background(), " "); err == nil {
			t.Fatal("Cancel accepted a blank run id: the delete is by label")
		}
		if len(wb.forwards) != 0 {
			t.Fatal("a blank run id was forwarded")
		}
	})
}

// TestExecuteStopsWhenItsContextEnds: the driver's own deadline, or the
// drive ending around the step, stops the wait at once and tells the replica
// -- the forward's context ends, which is the router's cancel.
func TestExecuteStopsWhenItsContextEnds(t *testing.T) {
	wb := &scriptedWorkbench{}
	e := newTestExecutor(wb, nil)
	e.statusEvery = time.Hour
	ctx, stop := context.WithCancel(context.Background())
	done := executeAndCommitAsync(e, ctx, exRequest())
	awaitCond(t, func() bool { return len(wb.sent(workbench.PipelineStepAction)) == 1 }, "the step was never forwarded")
	stop()
	res := awaitResult(t, done, "the context ended")
	if res.Status != pl.OutcomeCancelled || res.Failure == nil || res.Failure.Message != driveEndedMessage {
		t.Fatalf("result = %+v (failure %+v), want cancelled with the drive-ended sentence %q: the run was not "+
			"cancelled, its caller stopped waiting", res, res.Failure, driveEndedMessage)
	}
	select {
	case <-wb.sent(workbench.PipelineStepAction)[0].ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the forward kept waiting after the step's context ended")
	}
}

// TestExecuteWithNoFleetFailsAFleetStep: an agent node whose dispatcher was
// never wired cannot reach a machine, and says so rather than sending the
// step to the cluster, which cannot meet its need.
func TestExecuteWithNoFleetFailsAFleetStep(t *testing.T) {
	wb := &scriptedWorkbench{}
	e := newTestExecutor(wb, nil)
	req := exRequest()
	req.Compute, req.Step.Needs = pl.ComputeClusterAndFleet, []string{"display"}
	req.Step.Execution, req.Step.Image = pl.ExecutionNative, ""
	res, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	wantFailure(t, res, pl.OutcomeFailed, pl.CodeRunnerUnavailable)
	if len(wb.forwards) != 0 {
		t.Fatal("a step needing a display was forwarded to the cluster")
	}
}

// TestCancelledStepsAreTheRunsNotTheCallers: Cancel's own cause is what marks
// a step cancelled by its run; a step whose caller merely stopped waiting is
// cancelled too, and either way no Job is left behind by this node.
func TestCancelledStepsAreTheRunsNotTheCallers(t *testing.T) {
	wb := &scriptedWorkbench{}
	e := newTestExecutor(wb, nil)
	e.statusEvery = time.Hour
	done := executeAndCommitAsync(e, context.Background(), exRequest())
	awaitCond(t, func() bool { return len(wb.sent(workbench.PipelineStepAction)) == 1 }, "the step was never forwarded")
	if err := e.Cancel(context.Background(), "run-7f3a"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	res := awaitResult(t, done, "the run was cancelled")
	if res.Status != pl.OutcomeCancelled || res.Failure == nil || res.Failure.Code != pl.CodeStepCancelled ||
		res.Failure.Message != runCancelledMessage {
		t.Fatalf("result = %+v (failure %+v), want cancelled with the run's own sentence %q -- not the one a caller "+
			"that stopped waiting gets", res, res.Failure, runCancelledMessage)
	}
	if e.inflightCount("run-7f3a") != 0 {
		t.Error("the cancelled step is still tracked: a later cancel would reach a context nothing waits on")
	}
}
