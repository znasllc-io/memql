//go:build agent

package worker

import (
	"context"
	"testing"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

func TestModelCapacityIsSharedAcrossLocalAndForwardedCalls(t *testing.T) {
	entered := make(chan string, 3)
	release := make(chan struct{}, 3)
	h := newModelHop(t, func(ctx context.Context, req workerservice.ModelCallRequest, emit func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
		entered <- req.RequestId
		select {
		case <-release:
		case <-ctx.Done():
			return workerservice.ModelCallOutcome{Error: ctx.Err().Error()}
		}
		emit(workerservice.ModelCallDelta{Seq: 1, Content: "answer"})
		return workerservice.ModelCallOutcome{FinishReason: workerservice.ModelFinishStop}
	}, func(c *Candidate, w *workerservice.Worker) {
		w.Labels[workerservice.ModelLabel(hopModel)] = "max=1"
		c.Labels[workerservice.ModelLabel(hopModel)] = "max=1"
		c.SharingMode = "cluster"
		c.OwnerUserId = w.OwnerUserId
	})
	store := &sharedFleet{fakeFleet: h.store.fakeFleet, all: h.store.machines}
	h.link.handler.store = store
	local := newFleetInference(t, store)
	local.selfNodeId, local.registry = nodeB, h.link.handler.registry
	remote := newFleetInference(t, store)
	remote.selfNodeId, remote.forward = nodeA, h.link.router
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan error, 3)
	call := func(f *FleetInference, caller string) {
		callerCtx, stop := context.WithCancel(authorityCtx(t, caller))
		defer stop()
		go func() { <-ctx.Done(); stop() }()
		_, err := f.Call(callerCtx, memqlengine.FleetCallRequest{ActingUserId: caller, ModelId: hopModel, Kind: memqlengine.FleetKindChat})
		results <- err
	}
	go call(local, h.owner)
	<-entered
	go call(remote, "v1:identity:user:michelle")
	go call(local, "v1:identity:user:maria")
	select {
	case <-entered:
		t.Fatal("per-model capacity bypassed by another replica")
	case <-time.After(40 * time.Millisecond):
	}
	for n := 0; n < 3; n++ {
		release <- struct{}{}
		if err := <-results; err != nil {
			t.Fatal(err)
		}
		if n < 2 {
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("queued request did not advance")
			}
		}
	}
	if got := h.link.handler.registry.WorkerById("laptop").ActiveCount(); got != 0 {
		t.Fatalf("slots leaked: %d", got)
	}
}
