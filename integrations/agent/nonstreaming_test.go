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

// erroringExecutor is a taskstamp.Executor whose tool dispatch always
// fails, driving the loop's all-errored path (so the model-tiering
// escalation can be exercised). Execute is a benign no-op so the stamper's
// synthetic-plan materialization doesn't get in the way.
type erroringExecutor struct{}

func (erroringExecutor) ExecuteToolByName(_ context.Context, _ string, _ map[string]any) (string, error) {
	return "", errors.New("tool boom")
}
func (erroringExecutor) Execute(_ context.Context, _ string) (any, error) { return nil, nil }

func toolCallStep(name string) scriptStep {
	return scriptStep{result: &common.ToolCallingChatResult{
		ToolCalls: []common.ToolCall{{ID: "t1", Name: name, Arguments: "{}"}},
	}}
}

// scriptedToolProvider is a fake common.ToolCallingChatSIProvider that
// replays a programmed sequence of (result, err) steps -- one per
// CallChatWithTools invocation. It records how many times it was called so
// tests can assert retry / iteration behavior.
type scriptedToolProvider struct {
	steps []scriptStep
	calls int
}

type scriptStep struct {
	result *common.ToolCallingChatResult
	err    error
}

func (p *scriptedToolProvider) CallChatWithTools(
	_ context.Context,
	_ []common.ChatMessage,
	_ []common.ToolDefinition,
) (*common.ToolCallingChatResult, error) {
	if p.calls >= len(p.steps) {
		p.calls++
		return nil, errors.New("scriptedToolProvider: no more steps")
	}
	s := p.steps[p.calls]
	p.calls++
	return s.result, s.err
}

// captureSink records everything the loop emits so tests can assert the
// audit trail (which tool calls were surfaced, what text streamed).
type captureSink struct {
	texts       []string
	toolCalls   []string
	toolResults int
}

func (s *captureSink) TextDelta(t string)         { s.texts = append(s.texts, t) }
func (s *captureSink) ToolCall(_, name, _ string) { s.toolCalls = append(s.toolCalls, name) }
func (s *captureSink) ToolResult(_, _, _ string)  { s.toolResults++ }

func testReplier() *Replier {
	return &Replier{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func respondToUserStep(response string) scriptStep {
	return scriptStep{result: &common.ToolCallingChatResult{
		ToolCalls: []common.ToolCall{{
			ID:        "call_1",
			Name:      RespondToUserToolName,
			Arguments: `{"response":"` + response + `","citations":[]}`,
		}},
	}}
}

func TestRunNonStreamingToolLoop_TextOnlyTerminates(t *testing.T) {
	r := testReplier()
	prov := &scriptedToolProvider{steps: []scriptStep{
		{result: &common.ToolCallingChatResult{AssistantText: "all done, no tools needed"}},
	}}
	sink := &captureSink{}

	res, err := r.runNonStreamingToolLoop(context.Background(), prov, nil, nil, nil, sink, time.Now(), "req-1", turnContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.FinalText != "all done, no tools needed" {
		t.Fatalf("FinalText = %q, want the assistant text", res.FinalText)
	}
	if prov.calls != 1 {
		t.Fatalf("provider called %d times, want 1 (no tool round-trip)", prov.calls)
	}
	if res.Iterations != 1 {
		t.Fatalf("Iterations = %d, want 1", res.Iterations)
	}
}

func TestRunNonStreamingToolLoop_RespondToUserEnvelopeTerminates(t *testing.T) {
	r := testReplier()
	prov := &scriptedToolProvider{steps: []scriptStep{respondToUserStep("here is your file")}}
	sink := &captureSink{}

	res, err := r.runNonStreamingToolLoop(context.Background(), prov, nil, nil, nil, sink, time.Now(), "req-2", turnContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.FinalText != "here is your file" {
		t.Fatalf("FinalText = %q, want the envelope response", res.FinalText)
	}
	// respondToUser is a sentinel: surfaced to the sink as a ToolCall for
	// audit, but NEVER executed (no engine handler), so no ToolResult.
	if sink.toolResults != 0 {
		t.Fatalf("respondToUser must not produce a ToolResult, got %d", sink.toolResults)
	}
	if len(sink.toolCalls) != 1 || sink.toolCalls[0] != RespondToUserToolName {
		t.Fatalf("expected one respondToUser ToolCall on the sink, got %v", sink.toolCalls)
	}
	if prov.calls != 1 {
		t.Fatalf("provider called %d times, want 1", prov.calls)
	}
}

func TestRunNonStreamingToolLoop_RetriesTransientThenSucceeds(t *testing.T) {
	r := testReplier()
	prov := &scriptedToolProvider{steps: []scriptStep{
		{err: errors.New("anthropic: overloaded (529)")}, // transient -> retried in place
		{result: &common.ToolCallingChatResult{AssistantText: "recovered"}},
	}}
	sink := &captureSink{}

	res, err := r.runNonStreamingToolLoop(context.Background(), prov, nil, nil, nil, sink, time.Now(), "req-3", turnContext{})
	if err != nil {
		t.Fatalf("unexpected error after transient retry: %v", err)
	}
	if res.FinalText != "recovered" {
		t.Fatalf("FinalText = %q, want recovered", res.FinalText)
	}
	if prov.calls != 2 {
		t.Fatalf("provider called %d times, want 2 (one transient retry)", prov.calls)
	}
}

func TestRunNonStreamingToolLoop_HardErrorOnFirstStepFailsHonestly(t *testing.T) {
	r := testReplier()
	// A non-transient error on the very first step is a hard start failure:
	// the loop returns an error so the planner marks the Plan failed with a
	// real reason rather than fake-succeeding an empty turn (builds on
	// memql#893).
	prov := &scriptedToolProvider{steps: []scriptStep{
		{err: errors.New("invalid api key")},
	}}
	sink := &captureSink{}

	_, err := r.runNonStreamingToolLoop(context.Background(), prov, nil, nil, nil, sink, time.Now(), "req-4", turnContext{})
	if err == nil {
		t.Fatal("expected a hard error on first-step failure, got nil")
	}
}

// memql#898: when the cheap tier gets stuck (repeated all-errored tool
// rounds), the loop escalates to the stronger tier ONCE and lets it finish.
func TestRunNonStreamingToolLoop_EscalatesToStrongerTierWhenStuck(t *testing.T) {
	r := testReplier()
	r.stamper = taskstamp.New(erroringExecutor{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Cheap tier keeps asking for a tool that always errors -> all-errored
	// rounds accumulate. With the default escalate-after threshold of 2,
	// escalation fires on the 2nd errored round.
	cheap := &scriptedToolProvider{steps: []scriptStep{
		toolCallStep("workbenchHost"),
		toolCallStep("workbenchHost"),
		toolCallStep("workbenchHost"),
	}}
	// The stronger tier recovers and emits the terminal envelope.
	strong := &scriptedToolProvider{steps: []scriptStep{respondToUserStep("recovered on the strong tier")}}
	sink := &captureSink{}

	res, err := r.runNonStreamingToolLoop(context.Background(), cheap, strong, nil, nil, sink, time.Now(), "req-esc", turnContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.FinalText != "recovered on the strong tier" {
		t.Fatalf("FinalText = %q, want the strong-tier envelope response", res.FinalText)
	}
	// Cheap tier ran exactly twice (errored round 1 -> continue; errored
	// round 2 -> hits the threshold and escalates), then the strong tier
	// took over and finished in one call.
	if cheap.calls != 2 {
		t.Fatalf("cheap provider called %d times, want 2 before escalation", cheap.calls)
	}
	if strong.calls != 1 {
		t.Fatalf("strong provider called %d times, want 1 after escalation", strong.calls)
	}
}

// Without an escalation provider, a stuck cheap tier just breaks after the
// hard all-errored cap -- no panic, no escalation.
func TestRunNonStreamingToolLoop_NoEscalationProviderBreaksOnStuck(t *testing.T) {
	r := testReplier()
	r.stamper = taskstamp.New(erroringExecutor{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cheap := &scriptedToolProvider{steps: []scriptStep{
		toolCallStep("workbenchHost"), toolCallStep("workbenchHost"),
		toolCallStep("workbenchHost"), toolCallStep("workbenchHost"),
	}}
	sink := &captureSink{}

	res, err := r.runNonStreamingToolLoop(context.Background(), cheap, nil, nil, nil, sink, time.Now(), "req-noesc", turnContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Hard break after maxConsecutiveAllErrored (3) rounds with no escalation.
	if cheap.calls != 3 {
		t.Fatalf("cheap provider called %d times, want 3 (hard all-errored break)", cheap.calls)
	}
	if res == nil {
		t.Fatal("expected a (best-effort) result, got nil")
	}
}

func TestBackgroundEscalateAfterErroredRounds_Default(t *testing.T) {
	// No env set in the test process -> the built-in default.
	if got := backgroundEscalateAfterErroredRounds(); got != defaultBackgroundEscalateAfterErroredRounds {
		t.Fatalf("backgroundEscalateAfterErroredRounds() = %d, want default %d", got, defaultBackgroundEscalateAfterErroredRounds)
	}
}

func TestIsBackgroundLane(t *testing.T) {
	cases := []struct {
		name  string
		hints map[string]string
		want  bool
	}{
		{"nil hints", nil, false},
		{"absent", map[string]string{"plan_id": "p1"}, false},
		{"interactive explicit", map[string]string{ExecutionLaneHintKey: "interactive"}, false},
		{"background", map[string]string{ExecutionLaneHintKey: ExecutionLaneBackground}, true},
		{"background mixed-case", map[string]string{ExecutionLaneHintKey: "Background"}, true},
		{"background padded", map[string]string{ExecutionLaneHintKey: " background "}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsBackgroundLane(tc.hints); got != tc.want {
				t.Fatalf("IsBackgroundLane(%v) = %v, want %v", tc.hints, got, tc.want)
			}
		})
	}
}
