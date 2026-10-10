package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// The runtime resolves an expired attempt before cleanup. Its typed budget
// refusal must leave both loop implementations without a provider retry.
type exhaustedWorkEngine struct {
	workPromptEngine
	refusal error
}

func (e *exhaustedWorkEngine) WorkCallFailure(ctx context.Context) error {
	if ctx.Err() != nil {
		panic("attempt was cleaned up before budget resolution")
	}
	return e.refusal
}

func TestRunBudgetRefusalDoesNotRetryAProviderTimeout(t *testing.T) {
	refusal := &memql.RunCeilingError{RunId: "run", StepKey: "section", Breach: memql.RunCeilingBreach{Ceiling: "wallClock", Limit: "2700000ms", Actual: "2700001ms"}}
	for _, lane := range []string{"background", "stream-start", "stream-body"} {
		t.Run(lane, func(t *testing.T) {
			r := testReplier()
			r.engine = &exhaustedWorkEngine{refusal: refusal}
			sink := &captureSink{}
			var err error
			var calls int
			if lane == "background" {
				p := &scriptedToolProvider{steps: []scriptStep{{err: context.DeadlineExceeded}}}
				_, err = r.runNonStreamingToolLoop(context.Background(), p, nil, nil, nil, sink, time.Now(), "budget", turnContext{})
				calls = p.calls
			} else {
				p := &errorSequenceStreamProvider{next: func(int) (<-chan common.StreamToolChunk, error) {
					if lane == "stream-start" {
						return nil, context.DeadlineExceeded
					}
					return streamErrorChunks(common.StreamToolChunk{Content: "Saved partial draft"}, common.StreamToolChunk{Error: context.DeadlineExceeded}), nil
				}}
				result, failure := r.runStreamingToolLoop(context.Background(), p, nil, nil, sink, time.Now(), "budget", turnContext{})
				err, calls = failure, p.calls
				if lane == "stream-body" && (result == nil || result.FinalText != "Saved partial draft") {
					t.Fatalf("budget discarded partial output: %+v", result)
				}
			}
			if !errors.Is(err, refusal) || calls != 1 {
				t.Fatalf("budget refusal was lost or retried: %v, %d calls", err, calls)
			}
		})
	}
}
