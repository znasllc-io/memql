package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql/taskstamp"
	"github.com/znasllc-io/memql/core/common"
)

// --- pure breaker unit tests (memql#1128) ---------------------------------

// TestRepeatFailureBreaker_AbortsAtCeiling: the same tool failing the same way
// trips the breaker exactly at the ceiling, not before.
func TestRepeatFailureBreaker_AbortsAtCeiling(t *testing.T) {
	b := &repeatFailureBreaker{max: 3}
	boom := errors.New("workbenchHost: tools are agent-only -- no acting agent on context")

	if trip, count := b.observeFailure("workbenchHost", `{"action":"fs_write"}`, boom); trip || count != 1 {
		t.Fatalf("1st failure: trip=%v count=%d, want false/1", trip, count)
	}
	if trip, count := b.observeFailure("workbenchHost", `{"action":"fs_write"}`, boom); trip || count != 2 {
		t.Fatalf("2nd failure: trip=%v count=%d, want false/2", trip, count)
	}
	if trip, count := b.observeFailure("workbenchHost", `{"action":"fs_write"}`, boom); !trip || count != 3 {
		t.Fatalf("3rd failure: trip=%v count=%d, want true/3", trip, count)
	}
}

// TestRepeatFailureBreaker_SuccessResets: a success between failures clears the
// streak, so a flaky call never accumulates toward the ceiling.
func TestRepeatFailureBreaker_SuccessResets(t *testing.T) {
	b := &repeatFailureBreaker{max: 3}
	boom := errors.New("boom")

	b.observeFailure("workbenchHost", `{"a":1}`, boom) // count 1
	b.observeFailure("workbenchHost", `{"a":1}`, boom) // count 2
	b.observeSuccess()                                 // reset
	if trip, count := b.observeFailure("workbenchHost", `{"a":1}`, boom); trip || count != 1 {
		t.Fatalf("after success-reset: trip=%v count=%d, want false/1", trip, count)
	}
}

// TestRepeatFailureBreaker_DifferentArgsReset: a retry with corrected args is a
// DIFFERENT signature, so it restarts the streak instead of tripping. This is
// the conservative property -- legitimate self-correction is never killed.
func TestRepeatFailureBreaker_DifferentArgsReset(t *testing.T) {
	b := &repeatFailureBreaker{max: 3}
	boom := errors.New("validation: bad path")

	b.observeFailure("workbenchHost", `{"path":"/a"}`, boom)
	b.observeFailure("workbenchHost", `{"path":"/b"}`, boom)
	if trip, count := b.observeFailure("workbenchHost", `{"path":"/c"}`, boom); trip || count != 1 {
		t.Fatalf("different-args failures: trip=%v count=%d, want false/1 (no trip on distinct calls)", trip, count)
	}
}

// TestRepeatFailureBreaker_DifferentToolReset: alternating between two failing
// tools never trips -- each switch restarts the consecutive streak.
func TestRepeatFailureBreaker_DifferentToolReset(t *testing.T) {
	b := &repeatFailureBreaker{max: 3}
	boom := errors.New("boom")
	for i := 0; i < 6; i++ {
		name := "toolA"
		if i%2 == 1 {
			name = "toolB"
		}
		if trip, _ := b.observeFailure(name, `{}`, boom); trip {
			t.Fatalf("alternating tools tripped the breaker at i=%d -- should never trip", i)
		}
	}
}

// TestRepeatFailureBreaker_ArgsCanonicalized: cosmetically-different-but-equal
// args (key order) are the SAME signature, so they accumulate.
func TestRepeatFailureBreaker_ArgsCanonicalized(t *testing.T) {
	b := &repeatFailureBreaker{max: 2}
	boom := errors.New("boom")
	b.observeFailure("t", `{"x":1,"y":2}`, boom)
	if trip, count := b.observeFailure("t", `{"y":2,"x":1}`, boom); !trip || count != 2 {
		t.Fatalf("key-reordered args should be identical: trip=%v count=%d, want true/2", trip, count)
	}
}

// TestRepeatFailureBreaker_ErrorClassDistinguishes: the same call failing two
// genuinely-different ways (validation vs permission) does NOT fuse -- the
// breaker only kills truly identical failures.
func TestRepeatFailureBreaker_ErrorClassDistinguishes(t *testing.T) {
	b := &repeatFailureBreaker{max: 2}
	// memql.ClassifyToolError buckets by message text: "not allowed for caller
	// role" -> permission; "invalid"/"required" -> validation.
	permission := errors.New(`tool "x" is not allowed for caller role "reader"`)
	validation := errors.New("invalid argument: required field missing")
	b.observeFailure("x", `{}`, permission)
	if trip, count := b.observeFailure("x", `{}`, validation); trip || count != 1 {
		t.Fatalf("distinct error classes must not fuse: trip=%v count=%d, want false/1", trip, count)
	}
}

// TestRepeatFailureBreaker_Disabled: a non-positive ceiling disables the
// breaker entirely (env knob set to 0).
func TestRepeatFailureBreaker_Disabled(t *testing.T) {
	b := &repeatFailureBreaker{max: 0}
	boom := errors.New("boom")
	for i := 0; i < 50; i++ {
		if trip, _ := b.observeFailure("t", `{}`, boom); trip {
			t.Fatalf("disabled breaker tripped at i=%d", i)
		}
	}
}

func TestMaxRepeatFailures_Default(t *testing.T) {
	// No env set in the test process -> built-in default.
	if got := maxRepeatFailures(); got != defaultMaxRepeatFailures {
		t.Fatalf("maxRepeatFailures() = %d, want default %d", got, defaultMaxRepeatFailures)
	}
}

// --- loop integration tests ------------------------------------------------

// scriptedExecutor is a taskstamp.Executor whose per-tool outcome is driven by
// a function, so a test can model "fail twice then succeed", "always fail", or
// per-tool-name behavior. Execute is a benign no-op so the stamper's
// synthetic-plan materialization stays out of the way.
type scriptedExecutor struct {
	calls int
	fn    func(call int, name string) (string, error)
}

func (e *scriptedExecutor) ExecuteToolByName(_ context.Context, name string, _ map[string]any) (string, error) {
	c := e.calls
	e.calls++
	return e.fn(c, name)
}
func (e *scriptedExecutor) Execute(_ context.Context, _ string) (any, error) { return nil, nil }

func workbenchStep() scriptStep {
	return scriptStep{result: &common.ToolCallingChatResult{
		ToolCalls: []common.ToolCall{{ID: "t1", Name: "workbenchHost", Arguments: `{"action":"fs_write"}`}},
	}}
}

// TestNonStreamingLoop_BreakerAbortsOnIdenticalFailures: a tool that always
// fails the same way aborts the turn at the ceiling (3), NOT at maxIterations.
func TestNonStreamingLoop_BreakerAbortsOnIdenticalFailures(t *testing.T) {
	r := testReplier()
	r.stamper = taskstamp.New(
		&scriptedExecutor{fn: func(_ int, _ string) (string, error) {
			return "", errors.New("workbenchHost: tools are agent-only -- no acting agent on context")
		}},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	// Far more steps than the breaker ceiling -- if the breaker didn't fire the
	// loop would consume all of them.
	prov := &scriptedToolProvider{steps: []scriptStep{
		workbenchStep(), workbenchStep(), workbenchStep(), workbenchStep(),
		workbenchStep(), workbenchStep(), workbenchStep(), workbenchStep(),
	}}
	sink := &captureSink{}

	_, err := r.runNonStreamingToolLoop(context.Background(), prov, nil, nil, nil, sink, time.Now(), "req-brk", turnContext{})
	if err == nil {
		t.Fatal("expected the breaker to abort with an error, got nil")
	}
	// 3 model round-trips (one per identical failing call) then the abort.
	if prov.calls != 3 {
		t.Fatalf("provider called %d times, want 3 (abort on the 3rd identical failure)", prov.calls)
	}
}

// TestNonStreamingLoop_FailTwiceThenSucceedCompletes: a tool that fails twice
// then succeeds must NOT be killed -- the success resets the breaker and the
// turn finishes normally.
func TestNonStreamingLoop_FailTwiceThenSucceedCompletes(t *testing.T) {
	r := testReplier()
	r.stamper = taskstamp.New(
		&scriptedExecutor{fn: func(call int, _ string) (string, error) {
			if call < 2 {
				return "", errors.New("workbenchHost: transient setup error")
			}
			return `{"ok":true}`, nil
		}},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	// fail, fail, succeed, then the model wraps up with the envelope.
	prov := &scriptedToolProvider{steps: []scriptStep{
		workbenchStep(), workbenchStep(), workbenchStep(),
		respondToUserStep("your file is ready"),
	}}
	sink := &captureSink{}

	res, err := r.runNonStreamingToolLoop(context.Background(), prov, nil, nil, nil, sink, time.Now(), "req-recover", turnContext{})
	if err != nil {
		t.Fatalf("unexpected error -- a fail-then-succeed run must complete: %v", err)
	}
	if res.FinalText != "your file is ready" {
		t.Fatalf("FinalText = %q, want the envelope response", res.FinalText)
	}
	if prov.calls != 4 {
		t.Fatalf("provider called %d times, want 4 (fail, fail, succeed, respond)", prov.calls)
	}
}

// TestNonStreamingLoop_DifferentFailingCallsNotAborted: alternating between two
// distinct failing calls never trips the breaker (each is a fresh streak); the
// turn ends on the existing all-errored guard, not the breaker.
func TestNonStreamingLoop_DifferentFailingCallsNotAborted(t *testing.T) {
	r := testReplier()
	r.stamper = taskstamp.New(
		&scriptedExecutor{fn: func(_ int, _ string) (string, error) {
			return "", errors.New("validation: bad args")
		}},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	// Each step is a DIFFERENT call (distinct args), so the breaker's
	// consecutive-streak never reaches 2. With max=3 the breaker stays silent;
	// the all-errored round guard (also 3) is what stops the loop, with a nil
	// error (graceful best-effort).
	step := func(path string) scriptStep {
		return scriptStep{result: &common.ToolCallingChatResult{
			ToolCalls: []common.ToolCall{{ID: "t1", Name: "workbenchHost", Arguments: `{"path":"` + path + `"}`}},
		}}
	}
	prov := &scriptedToolProvider{steps: []scriptStep{
		step("/a"), step("/b"), step("/c"), step("/d"),
	}}
	sink := &captureSink{}

	_, err := r.runNonStreamingToolLoop(context.Background(), prov, nil, nil, nil, sink, time.Now(), "req-distinct", turnContext{})
	// The breaker must NOT be the thing that fired: distinct calls are not a
	// runaway. The all-errored guard breaks gracefully (nil error) at 3 rounds.
	if err != nil {
		t.Fatalf("distinct failing calls must not trigger the breaker abort: %v", err)
	}
	if prov.calls != 3 {
		t.Fatalf("provider called %d times, want 3 (all-errored guard, not breaker)", prov.calls)
	}
}

// --- streaming-loop coverage ----------------------------------------------

// fakeStreamProvider replays one StreamToolChunk sequence per
// CallChatStreamWithTools call: a single tool call, then Done. Mirrors the
// scriptedToolProvider but on the streaming surface so the breaker can be
// exercised through runStreamingToolLoop too.
type fakeStreamProvider struct {
	toolName string
	rawArgs  string
	calls    int
	maxCalls int
}

func (p *fakeStreamProvider) CallChatStreamWithTools(
	_ context.Context,
	_ []common.ChatMessage,
	_ []common.ToolDefinition,
) (<-chan common.StreamToolChunk, error) {
	p.calls++
	ch := make(chan common.StreamToolChunk, 2)
	ch <- common.StreamToolChunk{
		ToolCalls: []common.ToolCallDelta{{Index: 0, ID: "t1", Name: p.toolName, Arguments: p.rawArgs}},
	}
	ch <- common.StreamToolChunk{Done: true}
	close(ch)
	return ch, nil
}

// TestStreamingLoop_BreakerAbortsOnIdenticalFailures exercises the same breaker
// on the interactive streaming lane.
func TestStreamingLoop_BreakerAbortsOnIdenticalFailures(t *testing.T) {
	r := testReplier()
	r.stamper = taskstamp.New(
		&scriptedExecutor{fn: func(_ int, _ string) (string, error) {
			return "", errors.New("workbenchHost: tools are agent-only -- no acting agent on context")
		}},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	prov := &fakeStreamProvider{toolName: "workbenchHost", rawArgs: `{"action":"fs_write"}`}
	sink := &captureSink{}

	_, err := r.runStreamingToolLoop(context.Background(), prov, nil, nil, sink, time.Now(), "req-stream-brk", turnContext{})
	if err == nil {
		t.Fatal("expected the streaming breaker to abort with an error, got nil")
	}
	if prov.calls != 3 {
		t.Fatalf("stream provider called %d times, want 3 (abort on the 3rd identical failure)", prov.calls)
	}
}
