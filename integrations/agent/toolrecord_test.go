package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

type workerFailureHistoryProvider struct {
	calls    int
	messages []common.ChatMessage
}

func (p *workerFailureHistoryProvider) CallChatWithTools(_ context.Context, messages []common.ChatMessage, _ []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	p.calls++
	p.messages = append([]common.ChatMessage(nil), messages...)
	calls := []common.ToolCall{{ID: fmt.Sprintf("worker-call-%d", p.calls), Name: "workerHost", Arguments: `{"action":"exec","args":{"cmd":"example"}}`}}
	if p.calls == 3 {
		calls = append(calls, common.ToolCall{ID: "later-proposal", Name: "workerHost", Arguments: `{"action":"fs_write","args":{"path":"not-executed"}}`})
	}
	return &common.ToolCallingChatResult{ToolCalls: calls}, nil
}

func (p *workerFailureHistoryProvider) CallChatStreamWithTools(ctx context.Context, messages []common.ChatMessage, tools []common.ToolDefinition) (<-chan common.StreamToolChunk, error) {
	result, err := p.CallChatWithTools(ctx, messages, tools)
	if err != nil {
		return nil, err
	}
	ch := make(chan common.StreamToolChunk, 2)
	var deltas []common.ToolCallDelta
	for index, call := range result.ToolCalls {
		deltas = append(deltas, common.ToolCallDelta{Index: index, ID: call.ID, Name: call.Name, Arguments: call.Arguments})
	}
	ch <- common.StreamToolChunk{ToolCalls: deltas}
	ch <- common.StreamToolChunk{Done: true}
	close(ch)
	return ch, nil
}

type workerFailureCheckpointEngine struct {
	workPromptEngine
	saves int
	fail  bool
}

func (e *workerFailureCheckpointEngine) SaveWorkContinuation(ctx context.Context, messages []common.ChatMessage) error {
	e.saves++
	if e.fail && e.saves == 3 {
		return errors.New("checkpoint storage refused")
	}
	return e.workPromptEngine.SaveWorkContinuation(ctx, messages)
}

type workerFailureSink struct {
	captureSink
	results map[string]string
	errors  map[string]string
}

func (s *workerFailureSink) ToolResult(id, result, failure string) {
	s.results[id], s.errors[id] = result, failure
}

func TestReportedWorkerFailureReachesBothLoopHistories(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, failCheckpoint := range []bool{false, true} {
			t.Run(fmt.Sprintf("streaming=%t/checkpointFailure=%t", streaming, failCheckpoint), func(t *testing.T) {
				r := testReplier()
				engine := &workerFailureCheckpointEngine{fail: failCheckpoint}
				r.engine = engine
				exec := &scriptedExecutor{fn: func(call int, _ string) (string, error) {
					text := fmt.Sprintf(`{"nodes":[{"concept":"integration:worker:result","payload":{"ok":false,"errorCode":"exec_timeout","errorMessage":"Inspect before repeating","output":{"stdout":"partial effect evidence %d"}}}]}`, call+1)
					return text, memql.WorkerToolResultFailure(text)
				}}
				r.stamper = newToolRecorder(exec, r.logger)
				rec := &recordingRecorder{}
				r.stamper.SetRecorder(rec)
				provider := &workerFailureHistoryProvider{}
				ctx := auth.ContextWithUserActor(context.Background(), "owner")
				ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "worker-result-history", GoalId: "goal", OwnerUserId: "owner", StepKey: "step"})
				sink := &workerFailureSink{results: map[string]string{}, errors: map[string]string{}}
				var err error
				if streaming {
					_, err = r.runStreamingToolLoop(ctx, provider, nil, nil, sink, time.Now(), "worker-result", turnContext{})
				} else {
					_, err = r.runNonStreamingToolLoop(ctx, provider, nil, nil, nil, sink, time.Now(), "worker-result", turnContext{})
				}
				require.Error(t, err, "repeated failures must trip the existing breaker")
				require.Equal(t, 3, provider.calls)
				require.Equal(t, provider.calls, exec.calls, "no automatic dispatch retry between model calls")
				require.Equal(t, 3, engine.saves, "the final actual result must be saved before abort")
				require.Contains(t, sink.results["worker-call-3"], "partial effect evidence 3")
				require.NotEmpty(t, sink.errors["worker-call-3"])
				require.NotContains(t, sink.results, "later-proposal")
				if failCheckpoint {
					require.ErrorIs(t, err, memql.ErrWorkCheckpointRequired)
					require.ErrorContains(t, err, "checkpoint storage refused")
				} else {
					paired := map[string]string{}
					for _, msg := range engine.savedContinuation {
						if msg.Role == "tool" {
							paired[msg.ToolCallId] = msg.Content
						}
					}
					require.Len(t, paired, 4)
					require.Contains(t, paired["worker-call-3"], "partial effect evidence 3")
					require.Contains(t, paired["later-proposal"], "Not executed before the saved checkpoint")
				}
				results := 0
				for _, msg := range provider.messages {
					if msg.Role == "tool" {
						results++
						require.Contains(t, msg.Content, "partial effect evidence")
						require.Contains(t, msg.Content, `"retryable":false`)
					}
				}
				require.Equal(t, 2, results, "both prior failed tool results reached the next model request")
				require.Len(t, rec.all(), 3)
				for _, record := range rec.all() {
					require.True(t, record.IsError)
				}
			})
		}
	}
}

func TestReportedWorkerFailureIsRecordedWithoutRedispatch(t *testing.T) {
	failure := &memql.StructuredToolError{Type: memql.ToolErrorPermission, Message: "Call requestComputerUseScope", Retryable: false}
	exec := &countingExecutor{err: failure}
	rec := &recordingRecorder{}
	r := newToolRecorder(exec, nil)
	r.SetRecorder(rec)
	out, err := r.ExecuteToolByName(runCtx("run", "owner"), "workerHost", map[string]any{"action": "exec"})
	if !errors.Is(err, failure) || exec.calls != 1 || out != "result of workerHost" {
		t.Fatalf("outcome changed or repeated: %v, calls=%d", err, exec.calls)
	}
	got := rec.all()
	if len(got) != 1 || !got[0].IsError || got[0].Error != failure.Error() {
		t.Fatalf("worker refusal recorded as success: %+v", got)
	}
}

type recordingRecorder struct {
	mu   sync.Mutex
	seen []ToolInvocation
	err  error
}

func (r *recordingRecorder) RecordToolInvocation(_ context.Context, inv ToolInvocation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, inv)
	return r.err
}

func (r *recordingRecorder) all() []ToolInvocation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ToolInvocation(nil), r.seen...)
}

// countingExecutor answers every dispatch and counts them.
type countingExecutor struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (e *countingExecutor) ExecuteToolByName(_ context.Context, name string, _ map[string]any) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	return "result of " + name, e.err
}

func runCtx(runId, owner string) context.Context {
	return common.ContextWithRun(context.Background(), common.RunContext{
		RunId:       runId,
		StepKey:     "step-a",
		OwnerUserId: owner,
		Mode:        common.RunModeLive,
	})
}

// TestATurnWithNToolCallsProducesNRecords is memql#5050's acceptance
// criterion, stated directly.
func TestATurnWithNToolCallsProducesNRecords(t *testing.T) {
	exec := &countingExecutor{}
	rec := &recordingRecorder{}
	r := newToolRecorder(exec, nil)
	r.SetRecorder(rec)

	ctx := runCtx("run-1", "user-1")
	const n = 5
	for i := 0; i < n; i++ {
		if _, err := r.ExecuteToolByName(ctx, "workbenchHost", map[string]any{"i": i}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	got := rec.all()
	if len(got) != n {
		t.Fatalf("recorded %d invocations for %d tool calls", len(got), n)
	}
	if exec.calls != n {
		t.Fatalf("dispatched %d tools for %d calls", exec.calls, n)
	}
	// Ordering is what the transcript reproduces, so seq must be dense and
	// monotonic rather than merely present.
	for i, inv := range got {
		if inv.Seq != i {
			t.Errorf("call %d recorded seq %d; a transcript sorted on seq would reorder the run", i, inv.Seq)
		}
		if inv.RunId != "run-1" || inv.OwnerUserId != "user-1" || inv.StepKey != "step-a" {
			t.Errorf("call %d lost its run context: %+v", i, inv)
		}
	}
}

// TestSeqIsPerRunNotPerProcess: two runs interleaving through one replier must
// each get their own dense ordering, or one run's transcript is shuffled by
// the other run's traffic.
func TestSeqIsPerRunNotPerProcess(t *testing.T) {
	rec := &recordingRecorder{}
	r := newToolRecorder(&countingExecutor{}, nil)
	r.SetRecorder(rec)

	a, b := runCtx("run-A", "u"), runCtx("run-B", "u")
	for _, ctx := range []context.Context{a, b, a, b, a} {
		if _, err := r.ExecuteToolByName(ctx, "t", nil); err != nil {
			t.Fatal(err)
		}
	}

	perRun := map[string][]int{}
	for _, inv := range rec.all() {
		perRun[inv.RunId] = append(perRun[inv.RunId], inv.Seq)
	}
	for runId, seqs := range perRun {
		for i, s := range seqs {
			if s != i {
				t.Errorf("%s got seqs %v, want dense 0..n-1", runId, seqs)
				break
			}
		}
	}
}

// TestNoRunMeansNoRecord is the deliberate behaviour change from taskstamp,
// which minted a synthetic ad-hoc Plan so every chat-driven tool call had a
// parent. Minting a RUN would be worse than the Plan was: a run row names an
// automation and would be claimed and executed by the run dispatcher.
func TestNoRunMeansNoRecord(t *testing.T) {
	exec := &countingExecutor{}
	rec := &recordingRecorder{}
	r := newToolRecorder(exec, nil)
	r.SetRecorder(rec)

	if _, err := r.ExecuteToolByName(context.Background(), "workbenchHost", nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if exec.calls != 1 {
		t.Fatalf("the tool must still run outside a run; calls=%d", exec.calls)
	}
	if got := rec.all(); len(got) != 0 {
		t.Fatalf("recorded %d invocations for a turn with no run", len(got))
	}
}

// TestAFailedToolIsStillRecorded: the record is of what HAPPENED, and a failed
// call happened. The transcript excludes it on read (isError), which is a
// different decision made in a different place.
func TestAFailedToolIsStillRecorded(t *testing.T) {
	boom := errors.New("tool exploded")
	rec := &recordingRecorder{}
	r := newToolRecorder(&countingExecutor{err: boom}, nil)
	r.SetRecorder(rec)

	if _, err := r.ExecuteToolByName(runCtx("run-1", "u"), "brokenTool", nil); !errors.Is(err, boom) {
		t.Fatalf("the tool's error must reach the caller unchanged, got %v", err)
	}
	got := rec.all()
	if len(got) != 1 || !got[0].IsError || got[0].Error != boom.Error() {
		t.Fatalf("a failed call must be recorded as failed: %+v", got)
	}
}

// TestARecordingFailureNeverFailsTheToolCall keeps taskstamp's one genuinely
// good property: bookkeeping must not be able to break the product.
func TestARecordingFailureNeverFailsTheToolCall(t *testing.T) {
	rec := &recordingRecorder{err: errors.New("database is on fire")}
	r := newToolRecorder(&countingExecutor{}, nil)
	r.SetRecorder(rec)

	out, err := r.ExecuteToolByName(runCtx("run-1", "u"), "workbenchHost", nil)
	if err != nil {
		t.Fatalf("a recording failure broke the tool call: %v", err)
	}
	if out != "result of workbenchHost" {
		t.Fatalf("the tool's result was altered: %q", out)
	}
}

// TestNoRecorderStillDispatches: a node with no work integration runs tools
// exactly as before.
func TestNoRecorderStillDispatches(t *testing.T) {
	exec := &countingExecutor{}
	r := newToolRecorder(exec, nil)
	// deliberately no SetRecorder

	if _, err := r.ExecuteToolByName(runCtx("run-1", "u"), "workbenchHost", nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if exec.calls != 1 {
		t.Fatalf("calls=%d, want 1", exec.calls)
	}
}
