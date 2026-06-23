package agent

// realtime_tools_test.go pins the non-blocking in-voice tool-call scheduler
// (#1430, realtime_tools.go): background execution that never blocks the
// conversation, call-order result injection, single-writer announce queueing
// at quiet boundaries, the pending cap, and the timeout failure injection.

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/integrations/openai"
)

// newToolTestExecutor builds a started executor with a tool bridge over the
// given transport and fast scheduler knobs (10ms tick, 20ms announce grace).
// tune (optional) adjusts the executor BEFORE Start.
func newToolTestExecutor(
	t *testing.T,
	transport ToolCallTransport,
	tune func(e *RealtimeExecutor),
) (*RealtimeExecutor, *fakeRealtimeSession) {
	t.Helper()
	fs := newFakeStream()
	c := newTestClient(t, fs)
	rt := newFakeRealtimeSession()
	e := NewRealtimeExecutor(context.Background(), CascadeConfig{PartitionID: "s1", GaAgentID: "s1-ga"},
		c, rt, &recordingSink{}, SessionPersona{}, nil)
	bridge := NewMcpToolBridge(transport, nil,
		[]*memqlv1.ToolDefinition{td("toolA", false, "read"), td("toolB", false, "read")},
		"s1", "s1-ga", nil)
	bridge.RealtimeTools() // populate exposed set
	e.SetToolBridge(bridge)
	e.toolTick = 10 * time.Millisecond
	e.toolAnnounceGrace = 20 * time.Millisecond
	if tune != nil {
		tune(e)
	}
	t.Cleanup(e.Close)
	e.Start()
	return e, rt
}

// callEvent feeds one model function-call completion into the session events.
func callEvent(callID, name string) openai.RealtimeServerEvent {
	return openai.RealtimeServerEvent{
		Kind: openai.EventFunctionArgsDone, CallID: callID, FuncName: name, Arguments: `{"q":"x"}`,
	}
}

// functionResultSnapshot returns a copy of the injected function results.
func functionResultSnapshot(rt *fakeRealtimeSession) []fakeFunctionResult {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]fakeFunctionResult, len(rt.functionResults))
	copy(out, rt.functionResults)
	return out
}

// TestRealtimeToolCalls_ConversationNotBlockedWhilePending is the headline
// #1430 contract: a slow tool holds NOTHING -- no result, no follow-up
// response is forced while it runs, and the conversation (a conductor speak
// directive) proceeds normally with the call still pending.
func TestRealtimeToolCalls_ConversationNotBlockedWhilePending(t *testing.T) {
	release := make(chan struct{})
	transport := func(ctx context.Context, _ string, _ map[string]any) (string, bool, error) {
		select {
		case <-release:
			return "slow tool output", false, nil
		case <-ctx.Done():
			return "", true, ctx.Err()
		}
	}
	e, rt := newToolTestExecutor(t, transport, nil)

	rt.events <- callEvent("call_1", "toolA")

	// The pending tool must not produce a result or a response on its own.
	require.Never(t, func() bool {
		return len(functionResultSnapshot(rt)) > 0 || rt.responseCount() > 0
	}, 150*time.Millisecond, 10*time.Millisecond, "pending tool must not inject or respond")

	// The conversation continues while the call is pending: a conductor speak
	// directive enters assistant-turn and drives its response immediately.
	require.True(t, e.Speak(SpeakDirective{Text: "meanwhile, here is an answer"}),
		"speak directive accepted while a tool call is pending")
	require.Equal(t, 1, rt.responseCount(), "the conversation's response is not gated on the tool")
	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventResponseCreated, ResponseID: "r1"}
	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventResponseDone, ResponseID: "r1", Status: "completed"}

	// Tool completes -> result injected + ONE announce response at the quiet
	// boundary, carrying the tool_result directive.
	close(release)
	require.Eventually(t, func() bool {
		res := functionResultSnapshot(rt)
		return len(res) == 1 && res[0].callID == "call_1" && res[0].output == "slow tool output"
	}, 2*time.Second, 10*time.Millisecond, "completed tool result injected by call_id")
	require.Eventually(t, func() bool {
		return rt.responseCount() == 2
	}, 2*time.Second, 10*time.Millisecond, "announce response fired at the quiet boundary")
	assert.Contains(t, rt.lastResponse(), "tool calls",
		"announce response carries the tool_result directive")
}

// TestRealtimeToolCalls_SingleWriterQueueing pins the GA one-writer
// constraint: a result landing while a response is in flight injects its
// function_call_output immediately (items are legal mid-stream) but the
// announce response queues until response.done.
func TestRealtimeToolCalls_SingleWriterQueueing(t *testing.T) {
	transport := func(context.Context, string, map[string]any) (string, bool, error) {
		return "fast output", false, nil
	}
	_, rt := newToolTestExecutor(t, transport, nil)

	// A response is already streaming (the model is mid-reply).
	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventResponseCreated, ResponseID: "r1"}

	rt.events <- callEvent("call_1", "toolA")

	require.Eventually(t, func() bool {
		return len(functionResultSnapshot(rt)) == 1
	}, 2*time.Second, 10*time.Millisecond, "result item injects mid-stream")

	require.Never(t, func() bool {
		return rt.responseCount() > 0
	}, 150*time.Millisecond, 10*time.Millisecond, "no second writer while a response is in flight")

	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventResponseDone, ResponseID: "r1", Status: "completed"}
	require.Eventually(t, func() bool {
		return rt.responseCount() == 1 && strings.Contains(rt.lastResponse(), "tool calls")
	}, 2*time.Second, 10*time.Millisecond, "announce fires once the response seals")
}

// TestRealtimeToolCalls_NeverInterruptsUserSpeech pins the barge-in-while-
// pending behaviour: a result landing while the user is speaking injects
// silently and the announcement waits for the quiet boundary after
// speech_stopped (+ grace).
func TestRealtimeToolCalls_NeverInterruptsUserSpeech(t *testing.T) {
	transport := func(context.Context, string, map[string]any) (string, bool, error) {
		return "fast output", false, nil
	}
	_, rt := newToolTestExecutor(t, transport, nil)

	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventInputSpeechStarted}
	rt.events <- callEvent("call_1", "toolA")

	require.Eventually(t, func() bool {
		return len(functionResultSnapshot(rt)) == 1
	}, 2*time.Second, 10*time.Millisecond, "result item injects silently during user speech")

	require.Never(t, func() bool {
		return rt.responseCount() > 0
	}, 150*time.Millisecond, 10*time.Millisecond, "no announce while the user holds the floor")

	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventInputSpeechStopped}
	require.Eventually(t, func() bool {
		return rt.responseCount() == 1 && strings.Contains(rt.lastResponse(), "tool calls")
	}, 2*time.Second, 10*time.Millisecond, "announce fires after speech stops (+ grace)")
}

// TestRealtimeToolCalls_OrderingPreserved pins per-session call-order
// injection: a fast second call's result waits for the slow first call, so
// function_call_output items land in the order the model emitted the calls.
func TestRealtimeToolCalls_OrderingPreserved(t *testing.T) {
	releaseA := make(chan struct{})
	transport := func(ctx context.Context, name string, _ map[string]any) (string, bool, error) {
		if name == "toolA" {
			select {
			case <-releaseA:
				return "A output", false, nil
			case <-ctx.Done():
				return "", true, ctx.Err()
			}
		}
		return "B output", false, nil
	}
	_, rt := newToolTestExecutor(t, transport, nil)

	rt.events <- callEvent("call_1", "toolA")
	rt.events <- callEvent("call_2", "toolB")

	// call_2 completes immediately but must wait for call_1's FIFO turn.
	require.Never(t, func() bool {
		return len(functionResultSnapshot(rt)) > 0
	}, 150*time.Millisecond, 10*time.Millisecond, "later result held behind the earlier pending call")

	close(releaseA)
	require.Eventually(t, func() bool {
		res := functionResultSnapshot(rt)
		return len(res) == 2 &&
			res[0].callID == "call_1" && res[0].output == "A output" &&
			res[1].callID == "call_2" && res[1].output == "B output"
	}, 2*time.Second, 10*time.Millisecond, "results inject in call order")
}

// TestRealtimeToolCalls_PendingCap pins the concurrency bound: a call past the
// cap never executes and is answered with a spoken-failure-style busy error,
// still in call order.
func TestRealtimeToolCalls_PendingCap(t *testing.T) {
	releaseA := make(chan struct{})
	var bRan atomic.Bool
	transport := func(ctx context.Context, name string, _ map[string]any) (string, bool, error) {
		if name == "toolB" {
			bRan.Store(true)
			return "B output", false, nil
		}
		select {
		case <-releaseA:
			return "A output", false, nil
		case <-ctx.Done():
			return "", true, ctx.Err()
		}
	}
	_, rt := newToolTestExecutor(t, transport, func(e *RealtimeExecutor) {
		e.ConfigureToolCalls(0, 1)
	})

	rt.events <- callEvent("call_1", "toolA")
	rt.events <- callEvent("call_2", "toolB")

	close(releaseA)
	require.Eventually(t, func() bool {
		return len(functionResultSnapshot(rt)) == 2
	}, 2*time.Second, 10*time.Millisecond, "both call_ids answered")

	res := functionResultSnapshot(rt)
	assert.Equal(t, "call_1", res[0].callID)
	assert.Equal(t, "A output", res[0].output)
	assert.Equal(t, "call_2", res[1].callID)
	assert.Contains(t, res[1].output, "[tool error]", "capped call answered with a failure the model can speak")
	assert.Contains(t, res[1].output, "not started")
	assert.False(t, bRan.Load(), "a capped call never executes")
}

// TestRealtimeToolCalls_TimeoutInjectsSpokenFailure pins the timeout bound: a
// wedged tool is answered with a spoken-failure-style output the model can
// relay, the announce still fires, and the late real result is dropped (its
// call_id was already answered).
func TestRealtimeToolCalls_TimeoutInjectsSpokenFailure(t *testing.T) {
	release := make(chan struct{})
	transport := func(ctx context.Context, _ string, _ map[string]any) (string, bool, error) {
		select {
		case <-release:
			return "late output", false, nil
		case <-ctx.Done():
			// Simulate a transport that ignores cancellation and keeps
			// running -- the scheduler must not wait for it.
			<-release
			return "late output", false, nil
		}
	}
	_, rt := newToolTestExecutor(t, transport, func(e *RealtimeExecutor) {
		e.ConfigureToolCalls(50*time.Millisecond, 0)
	})

	rt.events <- callEvent("call_1", "toolA")

	require.Eventually(t, func() bool {
		res := functionResultSnapshot(rt)
		return len(res) == 1 && res[0].callID == "call_1" &&
			strings.Contains(res[0].output, "[tool error]") &&
			strings.Contains(res[0].output, "did not finish")
	}, 2*time.Second, 10*time.Millisecond, "timeout answered with a spoken-failure-style output")

	require.Eventually(t, func() bool {
		return rt.responseCount() == 1 && strings.Contains(rt.lastResponse(), "tool calls")
	}, 2*time.Second, 10*time.Millisecond, "announce fires for the timeout result")

	// The late real result must be dropped, not double-injected.
	close(release)
	require.Never(t, func() bool {
		return len(functionResultSnapshot(rt)) > 1
	}, 200*time.Millisecond, 10*time.Millisecond, "late result after timeout is dropped")
}

// TestRealtimeToolCalls_GateRoundtripDefersAnnounce pins the conductor-gate
// guard: while a turn's gate round-trip is pending, a ready tool result must
// not steal the default-conversation writer slot the engage is about to take.
func TestRealtimeToolCalls_GateRoundtripDefersAnnounce(t *testing.T) {
	transport := func(context.Context, string, map[string]any) (string, bool, error) {
		return "fast output", false, nil
	}
	e, rt := newToolTestExecutor(t, transport, nil)

	// Hold a gate round-trip open (the fake stream never answers the
	// VoiceAgentTurnRequest until told to).
	e.gatePending.Add(1)
	rt.events <- callEvent("call_1", "toolA")

	require.Eventually(t, func() bool {
		return len(functionResultSnapshot(rt)) == 1
	}, 2*time.Second, 10*time.Millisecond, "result item still injects")
	require.Never(t, func() bool {
		return rt.responseCount() > 0
	}, 150*time.Millisecond, 10*time.Millisecond, "announce deferred while the gate decision is pending")

	e.gatePending.Add(-1)
	e.kickToolLoop()
	require.Eventually(t, func() bool {
		return rt.responseCount() == 1
	}, 2*time.Second, 10*time.Millisecond, "announce fires once the gate settles")
}
