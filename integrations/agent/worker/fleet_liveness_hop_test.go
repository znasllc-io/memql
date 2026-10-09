//go:build agent

package worker

import (
	"context"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"testing"
	"time"
)

func TestFleetLivenessReachesCallerBeforeAnyAnswerLocallyAndAcrossReplica(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "holding replica", true: "other replica"}[remote], func(t *testing.T) {
			activity := make(chan struct{}, 1)
			h := newModelHop(t, func(ctx context.Context, _ workerservice.ModelCallRequest, emit func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
				emit(workerservice.ModelCallDelta{Seq: 1, Keepalive: true})
				select {
				case <-activity:
				case <-ctx.Done():
					return workerservice.ModelCallOutcome{Error: "liveness never reached caller before answer"}
				}
				emit(workerservice.ModelCallDelta{Seq: 2, Content: "answer"})
				return workerservice.ModelCallOutcome{FinishReason: workerservice.ModelFinishStop}
			}, nil)
			f := newFleetInference(t, h.store)
			if remote {
				f.selfNodeId, f.forward = nodeA, h.link.router
			} else {
				f.selfNodeId, f.registry = nodeB, h.link.handler.registry
			}
			ctx, cancel := context.WithTimeout(authorityCtx(t, h.owner), 2*time.Second)
			defer cancel()
			text := ""
			result, err := f.Call(ctx, memqlengine.FleetCallRequest{ActingUserId: h.owner, ModelId: hopModel, Kind: memqlengine.FleetKindChat,
				OnActivity: func() { activity <- struct{}{} }, OnDelta: func(s string) { text += s },
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Content != "answer" || text != "answer" {
				t.Fatalf("liveness affected content: result=%q streamed=%q", result.Content, text)
			}
		})
	}
}
