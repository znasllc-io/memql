package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/common"
)

type errorSequenceStreamProvider struct {
	calls int
	next  func(int) (<-chan common.StreamToolChunk, error)
}

func (p *errorSequenceStreamProvider) CallChatStreamWithTools(context.Context, []common.ChatMessage, []common.ToolDefinition) (<-chan common.StreamToolChunk, error) {
	p.calls++
	return p.next(p.calls)
}

func streamErrorChunks(chunks ...common.StreamToolChunk) <-chan common.StreamToolChunk {
	ch := make(chan common.StreamToolChunk, len(chunks))
	for _, chunk := range chunks {
		ch <- chunk
	}
	close(ch)
	return ch
}

func TestStreamingFailurePreservesPartialResultAndError(t *testing.T) {
	cause := errors.New("provider rejected invalid request")
	for _, continuation := range []bool{false, true} {
		t.Run(map[bool]string{false: "first stream", true: "continuation start"}[continuation], func(t *testing.T) {
			r := testReplier()
			r.stamper = newToolRecorder(successExecutor{}, r.logger)
			provider := &errorSequenceStreamProvider{next: func(call int) (<-chan common.StreamToolChunk, error) {
				if continuation {
					if call == 1 {
						return streamErrorChunks(common.StreamToolChunk{Content: "Partial answer", ToolCalls: []common.ToolCallDelta{{Index: 0, ID: "tool", Name: "noop", Arguments: "{}"}}, Done: true}), nil
					}
					return nil, cause
				}
				return streamErrorChunks(common.StreamToolChunk{Content: "Partial answer"}, common.StreamToolChunk{Error: cause, Done: true}), nil
			}}
			sink := &captureSink{}
			result, err := r.runStreamingToolLoop(context.Background(), provider, nil, nil, sink, time.Now(), "stream-failure", turnContext{})
			if !errors.Is(err, cause) {
				t.Fatalf("failed upstream stream reported success: result=%+v error=%v", result, err)
			}
			if result == nil || result.FinalText != "Partial answer" || result.TextChunks == 0 {
				t.Fatalf("partial answer was discarded: %+v", result)
			}
			wantCalls, wantTools := 1, 0
			if continuation {
				wantCalls, wantTools = 2, 1
			}
			if provider.calls != wantCalls || len(result.ToolCalls) != wantTools || sink.toolResults != wantTools {
				t.Fatalf("failure retried or lost completed tool evidence: calls=%d result=%+v sink=%+v", provider.calls, result, sink)
			}
		})
	}
}

func TestStreamingRetryExhaustionReturnsTheUpstreamError(t *testing.T) {
	cause := errors.New("service unavailable")
	for _, startFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "start"}[startFailure], func(t *testing.T) {
			provider := &errorSequenceStreamProvider{next: func(call int) (<-chan common.StreamToolChunk, error) {
				if startFailure {
					return nil, cause
				}
				if call == 1 {
					return streamErrorChunks(common.StreamToolChunk{Content: "Partial answer"}, common.StreamToolChunk{Error: cause}), nil
				}
				return streamErrorChunks(common.StreamToolChunk{Error: cause}), nil
			}}
			result, err := testReplier().runStreamingToolLoop(context.Background(), provider, nil, nil, &captureSink{}, time.Now(), "retry-failure", turnContext{})
			if !errors.Is(err, cause) || result == nil {
				t.Fatalf("retry exhaustion became successful completion: result=%+v error=%v", result, err)
			}
			wantCalls := streamTransientMaxRetries + 1
			if !startFailure {
				wantCalls = 1
			} // never duplicate text already delivered
			if provider.calls != wantCalls {
				t.Fatalf("provider calls=%d", provider.calls)
			}
			if !startFailure && result.FinalText != "Partial answer" {
				t.Fatalf("lost partial result: %+v", result)
			}
		})
	}
}

func TestStreamingRecoversAnIncompleteIterationWithoutRepeatingTools(t *testing.T) {
	for _, cause := range []string{"ollama: stream ended without a completion frame", "ollama: tool call parsing failed"} {
		t.Run(cause, func(t *testing.T) {
			r := testReplier()
			r.stamper = newToolRecorder(successExecutor{}, r.logger)
			provider := &errorSequenceStreamProvider{next: func(call int) (<-chan common.StreamToolChunk, error) {
				switch call {
				case 1:
					return streamErrorChunks(common.StreamToolChunk{ToolCalls: []common.ToolCallDelta{{Index: 0, ID: "tool", Name: "noop", Arguments: "{}"}}, Done: true}), nil
				case 2:
					return streamErrorChunks(common.StreamToolChunk{Error: errors.New(cause)}), nil
				default:
					return streamErrorChunks(common.StreamToolChunk{Content: "Recovered answer", Done: true}), nil
				}
			}}
			sink := &captureSink{}
			result, err := r.runStreamingToolLoop(context.Background(), provider, nil, nil, sink, time.Now(), "runtime-repair", turnContext{})
			if err != nil || result.FinalText != "Recovered answer" || provider.calls != 3 || sink.toolResults != 1 || len(result.ToolCalls) != 1 {
				t.Fatalf("recovery lost progress or repeated effects: result=%+v calls=%d tools=%d err=%v", result, provider.calls, sink.toolResults, err)
			}
		})
	}
}

type cancellingStreamSink struct {
	captureSink
	cancel context.CancelFunc
}

func (s *cancellingStreamSink) TextDelta(text string) {
	s.captureSink.TextDelta(text)
	s.cancel()
}

func TestStreamingCancellationPreservesPartialResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := &errorSequenceStreamProvider{next: func(int) (<-chan common.StreamToolChunk, error) {
		ch := make(chan common.StreamToolChunk, 2)
		ch <- common.StreamToolChunk{Content: "Partial answer."}
		ch <- common.StreamToolChunk{Error: errors.New("service unavailable")}
		// Leave the stream open. The sink cancels when the partial text is
		// flushed, covering cancellation while consuming or backing off.
		return ch, nil
	}}
	result, err := testReplier().runStreamingToolLoop(ctx, provider, nil, nil, &cancellingStreamSink{cancel: cancel}, time.Now(), "cancelled-stream", turnContext{})
	if !errors.Is(err, context.Canceled) || result == nil || result.FinalText != "Partial answer." {
		t.Fatalf("cancelled stream lost partial output or reported completion: result=%+v err=%v", result, err)
	}
	if provider.calls != 1 {
		t.Fatalf("cancelled stream retried %d times", provider.calls)
	}
}

type cancelAwareStreamProvider struct {
	t     *testing.T
	prior context.Context
	calls int
}

func (p *cancelAwareStreamProvider) CallChatStreamWithTools(ctx context.Context, _ []common.ChatMessage, _ []common.ToolDefinition) (<-chan common.StreamToolChunk, error) {
	p.calls++
	if p.prior != nil && p.prior.Err() == nil {
		p.t.Fatal("retry started with the previous generation still alive")
	}
	p.prior = ctx
	if p.calls == 1 {
		return make(chan common.StreamToolChunk), nil
	}
	return streamErrorChunks(common.StreamToolChunk{Content: "Recovered", Done: true}), nil
}
func TestStreamingIdleRetryCancelsPreviousGeneration(t *testing.T) {
	p := &cancelAwareStreamProvider{t: t}
	result, err := testReplier().runStreamingToolLoop(context.Background(), p, nil, nil, &captureSink{}, time.Now(), "idle-cancel", turnContext{StreamIdleBudget: time.Millisecond})
	if err != nil || result.FinalText != "Recovered" || p.calls != 2 {
		t.Fatalf("result=%+v calls=%d err=%v", result, p.calls, err)
	}
	if p.prior.Err() == nil {
		t.Fatal("successful stream left its attempt context alive")
	}
}
func TestFleetStreamBudgetAllowsSilentToolGeneration(t *testing.T) {
	if got := streamIdleBudgetForVendor("fleet"); got < 3*time.Minute {
		t.Fatalf("Fleet idle budget=%s", got)
	}
	if got := streamIdleBudgetForVendor("anthropic"); got != streamIdleTimeout() {
		t.Fatalf("SSE route budget changed to %s", got)
	}
}
