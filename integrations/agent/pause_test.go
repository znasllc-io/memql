package agent

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql/taskstamp"
	"github.com/znasllc-io/memql/core/common"
)

func TestPauseRegistry(t *testing.T) {
	const id = "req-reg-1"
	ClearPause(id) // start clean (registry is process-global)

	if PauseRequested(id) {
		t.Fatal("fresh id must not be paused")
	}
	RequestPause(id)
	if !PauseRequested(id) {
		t.Fatal("RequestPause should flag the id")
	}
	ClearPause(id)
	if PauseRequested(id) {
		t.Fatal("ClearPause should remove the flag")
	}
	// Empty id is a no-op, not a panic.
	RequestPause("")
	if PauseRequested("") {
		t.Fatal("empty id must never be reported paused")
	}
}

// successExecutor is a taskstamp.Executor whose tool dispatch always
// succeeds, so the loop completes a tool round and advances to the next
// iteration (where the pause checkpoint is then observed).
type successExecutor struct{}

func (successExecutor) ExecuteToolByName(_ context.Context, _ string, _ map[string]any) (string, error) {
	return "ok", nil
}
func (successExecutor) Execute(_ context.Context, _ string) (any, error) { return nil, nil }

// funcProvider runs a closure per CallChatWithTools so a test can trigger
// side effects (like an out-of-band pause) at a precise call.
type funcProvider struct {
	fn func() (*common.ToolCallingChatResult, error)
}

func (p *funcProvider) CallChatWithTools(_ context.Context, _ []common.ChatMessage, _ []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	return p.fn()
}

// A pass requested BEFORE the turn starts pauses at the very first
// checkpoint -- the model is never called, no tokens burned.
func TestRunNonStreamingToolLoop_PauseBeforeStart(t *testing.T) {
	r := testReplier()
	const reqID = "req-pause-pre"
	ClearPause(reqID)
	RequestPause(reqID)

	prov := &scriptedToolProvider{steps: []scriptStep{respondToUserStep("should never run")}}
	res, err := r.runNonStreamingToolLoop(context.Background(), prov, nil, nil, nil, &captureSink{}, time.Now(), reqID, turnContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Paused {
		t.Fatal("turn should be marked Paused")
	}
	if prov.calls != 0 {
		t.Fatalf("model must not be called when paused before start, got %d calls", prov.calls)
	}
	// The loop's deferred ClearPause must have cleaned the flag.
	if PauseRequested(reqID) {
		t.Fatal("loop should clear the pause flag on exit")
	}
}

// A pass that arrives DURING the first tool round is honored at the next
// checkpoint: the in-flight round completes, then the loop pauses instead
// of starting another model call.
func TestRunNonStreamingToolLoop_PauseAtCheckpointMidTurn(t *testing.T) {
	r := testReplier()
	r.stamper = taskstamp.New(successExecutor{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const reqID = "req-pause-mid"
	ClearPause(reqID)

	calls := 0
	prov := &funcProvider{fn: func() (*common.ToolCallingChatResult, error) {
		calls++
		if calls == 1 {
			// Simulate the planner's "pass" landing during the first round.
			RequestPause(reqID)
			return &common.ToolCallingChatResult{
				ToolCalls: []common.ToolCall{{ID: "t1", Name: "noop", Arguments: "{}"}},
			}, nil
		}
		// Would terminate the turn -- but the checkpoint should pause first.
		return respondToUserStep("should not reach here").result, nil
	}}

	res, err := r.runNonStreamingToolLoop(context.Background(), prov, nil, nil, nil, &captureSink{}, time.Now(), reqID, turnContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Paused {
		t.Fatal("turn should be marked Paused after a mid-turn pass")
	}
	if calls != 1 {
		t.Fatalf("model should be called once (round 1), then pause at the next checkpoint; got %d calls", calls)
	}
	if PauseRequested(reqID) {
		t.Fatal("loop should clear the pause flag on exit")
	}
}

// A turn that runs to completion without a pass is never marked Paused.
func TestRunNonStreamingToolLoop_NoPauseWhenNotRequested(t *testing.T) {
	r := testReplier()
	const reqID = "req-no-pause"
	ClearPause(reqID)
	prov := &scriptedToolProvider{steps: []scriptStep{respondToUserStep("done")}}
	res, err := r.runNonStreamingToolLoop(context.Background(), prov, nil, nil, nil, &captureSink{}, time.Now(), reqID, turnContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Paused {
		t.Fatal("turn without a pass request must not be marked Paused")
	}
	if res.FinalText != "done" {
		t.Fatalf("FinalText = %q, want done", res.FinalText)
	}
}
