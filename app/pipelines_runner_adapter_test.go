package app

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// pipelines_runner_adapter_test.go -- the adapter between the workbench's
// forward handler (JSON in, JSON out) and the substrate's runner (epic
// memql#5478, #5493). What it must get right is the translation: a step
// reaches the runner as the agent sent it, secrets included; the runner's
// answer reaches the agent as the runner gave it; and anything it cannot read
// runs nothing.

// pipelinesFakeStepRunner records what the adapter hands the runner and
// answers with what the test set.
type pipelinesFakeStepRunner struct {
	mu      sync.Mutex
	runs    []pipelinesteps.StepRun
	runCtx  context.Context
	asked   []pipelinesteps.StatusRequest
	acked   []pipelinesteps.AckRequest
	stopped []pipelinesteps.CancelRequest

	result    pl.StepResult
	status    pipelinesteps.StatusReply
	ackErr    error
	deleted   int
	cancelErr error
}

func (f *pipelinesFakeStepRunner) Readiness() pl.RunnerReadiness {
	return pl.RunnerReadiness{NodeID: "workbench-test", Available: true, Isolation: "not_proven"}
}

var _ pipelineStepRunner = (*pipelinesFakeStepRunner)(nil)

func (f *pipelinesFakeStepRunner) Run(ctx context.Context, run pipelinesteps.StepRun) pl.StepResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, run)
	f.runCtx = ctx
	return f.result
}

func (f *pipelinesFakeStepRunner) Status(_ context.Context, req pipelinesteps.StatusRequest) pipelinesteps.StatusReply {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, req)
	return f.status
}

func (f *pipelinesFakeStepRunner) Ack(_ context.Context, req pipelinesteps.AckRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acked = append(f.acked, req)
	return f.ackErr
}

func (f *pipelinesFakeStepRunner) CancelRun(_ context.Context, req pipelinesteps.CancelRequest) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, req)
	return f.deleted, f.cancelErr
}

func (f *pipelinesFakeStepRunner) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs) + len(f.asked) + len(f.acked) + len(f.stopped)
}

type pipelinesAdapterCtxKey struct{}

// TestThePipelineAdapterCarriesAStepToTheRunnerAndItsOutcomeBack. The step's
// secrets travel this hop -- JSON is the forward's wire, the one place they
// do -- so the runner must receive them exactly; and the context is the
// handler's, because the runner reads its end as a CANCEL.
func TestThePipelineAdapterCarriesAStepToTheRunnerAndItsOutcomeBack(t *testing.T) {
	runner := &pipelinesFakeStepRunner{result: pl.StepResult{
		Status: pl.OutcomeSucceeded, ExitCode: 0, LogFileID: "file-log", ArtifactFileIDs: []string{"file-a"},
		Where: pl.Where{Surface: "cluster", NodeID: "workbench-a", JobName: "mp-0123456789abcdef01234567"},
		Notes: []pl.Failure{{Code: pl.CodeArtifactMissing, Message: "dist/missing.txt matched nothing"}},
	}}
	adapter := &pipelinesRunnerAdapter{runner: runner, logger: quietLogger()}
	sent := pipelinesteps.StepRun{
		RunID: "run-1", WorkRunID: "work-1", StepKey: "tests.unit", Attempt: 2, OwnerUserID: pipelinesTestOwner,
		Repository: pl.Repository{Owner: "acme", Name: "widget"}, SHA: "abc123", InstallationID: 42,
		Image: "toolchain:1", Command: "go test ./...", Env: map[string]string{"MEMQL_STEP": "tests.unit"},
		Secrets: map[string]string{"NPM_TOKEN": "s3cret-value"}, Artifacts: []string{"dist/"},
		TimeoutSeconds: 600, DeadlineCode: pl.CodeStepTimeout,
	}
	args, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), pipelinesAdapterCtxKey{}, "the handler's"))
	defer cancel()
	out := adapter.RunStep(ctx, args)

	if len(runner.runs) != 1 {
		t.Fatalf("the runner was handed %d steps, want 1", len(runner.runs))
	}
	if !reflect.DeepEqual(runner.runs[0], sent) {
		t.Errorf("the runner was handed %+v, want what the agent sent: %+v", runner.runs[0], sent)
	}
	var got pl.StepResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("the outcome is not a step's result: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(got, runner.result) {
		t.Errorf("the agent would read %+v, want the runner's %+v", got, runner.result)
	}
	if runner.runCtx.Value(pipelinesAdapterCtxKey{}) != "the handler's" {
		t.Error("the runner did not run under the handler's context")
	}
	cancel()
	if runner.runCtx.Err() == nil {
		t.Error("ending the handler's context did not reach the runner: a WorkbenchForwardCancel could never stop the step")
	}
}

// TestAStepTheAdapterCannotReadRunsNothing. A forward whose step does not
// decode is answered at once as the engine's own failure -- and without
// quoting what it carried, which can be a secret.
func TestAStepTheAdapterCannotReadRunsNothing(t *testing.T) {
	runner := &pipelinesFakeStepRunner{}
	adapter := &pipelinesRunnerAdapter{runner: runner, logger: quietLogger()}
	for _, args := range []string{`{not json`, `{"secrets":{"NPM_TOKEN":"s3cret-value"},"attempt":"two"}`} {
		out := adapter.RunStep(context.Background(), []byte(args))
		var got pl.StepResult
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: the answer is not a step's result: %v\n%s", args, err, out)
		}
		if got.Status != pl.OutcomeFailed || got.ExitCode != -1 || got.Failure == nil || got.Failure.Code != pl.CodeExecutorError {
			t.Errorf("%s: answered %+v (failure %+v), want a failed step coded %s", args, got, got.Failure, pl.CodeExecutorError)
		}
		if strings.Contains(string(out), "s3cret-value") {
			t.Errorf("%s: the answer quotes a secret the step carried:\n%s", args, out)
		}
	}
	if n := runner.calls(); n != 0 {
		t.Errorf("the runner was called %d times for steps nobody could read", n)
	}

	// An outcome that cannot be encoded is still an answer: the agent's
	// executor parks on this reply, and an empty one is no answer at all.
	out := adapter.outcome(pl.StepResult{Status: pl.OutcomeSucceeded, Timings: map[string]float64{"pkg": math.Inf(1)}})
	var got pl.StepResult
	if err := json.Unmarshal(out, &got); err != nil || got.Status != pl.OutcomeFailed || got.Failure == nil ||
		got.Failure.Code != pl.CodeExecutorError {
		t.Errorf("an unencodable outcome was answered %s (%v), want a failed step coded %s", out, err, pl.CodeExecutorError)
	}
}

// TestThePipelineAdapterAnswersStatusAckAndCancel. The three immediate
// actions: the request reaches the runner as sent, the reply comes back as
// the runner gave it, and a refusal is a code on the envelope -- which is
// where the agent reads it -- rather than a payload it would have to parse.
func TestThePipelineAdapterAnswersStatusAckAndCancel(t *testing.T) {
	result := pl.StepResult{Status: pl.OutcomeFailed, ExitCode: 1}
	runner := &pipelinesFakeStepRunner{
		status:  pipelinesteps.StatusReply{State: pipelinesteps.StateFinished, Result: &result, Runner: "workbench-a"},
		deleted: 2,
	}
	adapter := &pipelinesRunnerAdapter{runner: runner, logger: quietLogger()}
	ctx := context.Background()

	reply, code := adapter.Status(ctx, []byte(`{"jobName":"mp-0123456789abcdef01234567"}`))
	var status pipelinesteps.StatusReply
	if code != "" || json.Unmarshal(reply, &status) != nil || !reflect.DeepEqual(status, runner.status) {
		t.Errorf("Status = %s, %q; want the runner's reply %+v and no code", reply, code, runner.status)
	}
	if len(runner.asked) != 1 || runner.asked[0].JobName != "mp-0123456789abcdef01234567" {
		t.Errorf("the runner was asked about %+v", runner.asked)
	}

	if code := adapter.Ack(ctx, []byte(`{"jobName":"mp-0123456789abcdef01234567"}`)); code != "" {
		t.Errorf("an ack the runner took = %q, want no code", code)
	}
	runner.ackErr = errors.New("the API server is unreachable")
	if code := adapter.Ack(ctx, []byte(`{"jobName":"mp-0123456789abcdef01234567"}`)); code != pipelineAckFailed {
		t.Errorf("an ack the runner failed = %q, want %q", code, pipelineAckFailed)
	}
	if len(runner.acked) != 2 || runner.acked[0].JobName != "mp-0123456789abcdef01234567" {
		t.Errorf("the runner was told %+v", runner.acked)
	}

	reply, code = adapter.CancelRun(ctx, []byte(`{"runId":"run-1"}`))
	if code != "" || string(reply) != `{"jobsDeleted":2}` {
		t.Errorf("CancelRun = %s, %q; want {\"jobsDeleted\":2} and no code", reply, code)
	}
	runner.deleted, runner.cancelErr = 1, errors.New("the secrets could not be deleted")
	reply, code = adapter.CancelRun(ctx, []byte(`{"runId":"run-1"}`))
	if code != pipelineCancelFailed || string(reply) != `{"jobsDeleted":1}` {
		t.Errorf("a cancel that deleted one Job and then failed = %s, %q; want the count and %q", reply, code, pipelineCancelFailed)
	}
	if len(runner.stopped) != 2 || runner.stopped[0].RunID != "run-1" {
		t.Errorf("the runner was asked to stop %+v", runner.stopped)
	}

	// An answer that cannot be encoded is a code, never an empty payload with
	// none: the executor reads a cancel that carries no code as done.
	if out, code := adapter.reply(workbench.PipelineCancelAction, math.NaN()); out != nil || code != pipelineReplyUnencodable {
		t.Errorf("an unencodable answer = %s, %q; want no payload and %q", out, code, pipelineReplyUnencodable)
	}

	before := runner.calls()
	if _, code := adapter.Status(ctx, []byte(`{`)); code != pipelineArgsUnreadable {
		t.Errorf("an unreadable status = %q, want %q", code, pipelineArgsUnreadable)
	}
	if code := adapter.Ack(ctx, []byte(`[]`)); code != pipelineArgsUnreadable {
		t.Errorf("an unreadable ack = %q, want %q", code, pipelineArgsUnreadable)
	}
	if _, code := adapter.CancelRun(ctx, []byte(`"run-1"`)); code != pipelineArgsUnreadable {
		t.Errorf("an unreadable cancel = %q, want %q", code, pipelineArgsUnreadable)
	}
	if runner.calls() != before {
		t.Error("the runner was called for a request nobody could read")
	}
}
