//go:build agent

package worker

// THE LEVEL CROSSES THE HOP. A planner's triage is a `fast` call, and the node
// that makes it never holds the machine's stream: the planner resolves
// fleet:qwen3.5:4b, and the agent replica holding the laptop's stream starts
// the call. The level has to survive buildStart, the ModelCallStart JSON on
// NodeService.Stream, and the projection back onto the worker package's
// request on the far replica -- the cockpit reads it there to run a fast call
// on Ollama with the model's hidden thinking off. Dropped anywhere on the way,
// the call still answers; it just spends tens of seconds thinking first.

import (
	"context"
	"testing"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/common"
)

func TestTheLevelReachesTheFleetDoorsModelCallStart(t *testing.T) {
	f := &FleetInference{}
	start := f.buildStart(memqlengine.FleetCallRequest{Kind: memqlengine.FleetKindChat, ModelId: "qwen3.5:4b", Level: "fast"})
	if got := start.GetLevel(); got != "fast" {
		t.Fatalf("ModelCallStart.level = %q, want the level the call resolved at", got)
	}
	if got := modelCallRequestFromProto("local-1", start, time.Minute).Level; got != "fast" {
		t.Fatalf("the receiving replica's request dropped the level, got %q", got)
	}
	plain := f.buildStart(memqlengine.FleetCallRequest{Kind: memqlengine.FleetKindChat, ModelId: "qwen3.5:4b"})
	if got := plain.GetLevel(); got != "" {
		t.Fatalf("a call bound at no level carried %q", got)
	}
}

func TestTheLevelSurvivesTheReplicaHop(t *testing.T) {
	got := make(chan string, 1)
	h := newModelHop(t, func(_ context.Context, req workerservice.ModelCallRequest, _ func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
		got <- req.Level
		return workerservice.ModelCallOutcome{FinishReason: workerservice.ModelFinishStop, Content: `{"complexity":"trivial"}`}
	}, nil)
	f := newFleetInference(t, h.store)
	// The call is made on node A; the machine's stream is held on node B.
	f.selfNodeId, f.forward = nodeA, h.link.router

	if _, err := f.Call(authorityCtx(t, h.owner), memqlengine.FleetCallRequest{
		ActingUserId: h.owner, ModelId: hopModel, Kind: memqlengine.FleetKindChat, Level: "fast",
		Messages: []common.ChatMessage{{Role: "user", Content: "classify: hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case level := <-got:
		if level != "fast" {
			t.Fatalf("the machine on the other replica was started at level %q, want fast", level)
		}
	default:
		t.Fatal("the machine never ran the call")
	}
}
