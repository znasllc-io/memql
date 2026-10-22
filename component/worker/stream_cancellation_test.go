package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// A parent drains output before returning. Asking an unreachable worker to
// cancel is not enough: the local stream must end even if no End arrives.
func TestCancelledCallsReleaseOutputDrainers(t *testing.T) {
	t.Run("app", func(t *testing.T) {
		session, _ := newAppSessionTestSession(t)
		h, err := session.worker.StartAppSession(context.Background(), AppSessionRequest{
			SessionId: "cancel-no-end", App: AppIdCodex, Kind: AppSessionKindRun,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := h.Wait(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait = %v", err)
		}
		select {
		case _, ok := <-h.Chunks():
			if ok {
				t.Fatal("cancelled stream returned an unexpected chunk")
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("cancel returned but its output drainer still waits for a worker End")
		}
	})
	t.Run("model", func(t *testing.T) {
		h, _ := newTestModelCall(t, ModelCallLimits{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := h.Wait(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait = %v", err)
		}
		select {
		case _, ok := <-h.Deltas():
			if ok {
				t.Fatal("cancelled stream returned an unexpected delta")
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("cancel returned but its output drainer still waits for a worker End")
		}
	})
}

// The shared receive loop cannot observe disconnection while a full output
// buffer holds its handler. A replacement worker must release that old handler.
func TestDisconnectedStreamReleasesBackpressuredApp(t *testing.T) {
	session, _ := newAppSessionTestSession(t)
	h, err := session.worker.StartAppSession(context.Background(), AppSessionRequest{
		SessionId: "full-buffer", App: AppIdCodex, Kind: AppSessionKindRun,
	})
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < cap(h.chunks); n++ {
		session.handleAppSessionChunk(&memqlv1.AppSessionChunk{SessionId: h.SessionId(), Seq: uint64(n + 1)})
	}
	returned := make(chan struct{})
	go func() {
		session.handleAppSessionChunk(&memqlv1.AppSessionChunk{SessionId: h.SessionId(), Seq: uint64(cap(h.chunks) + 1)})
		close(returned)
	}()
	session.cancel()
	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("full app output buffer kept the disconnected receive handler alive")
	}
	h.finish(AppSessionOutcome{Error: "worker_disconnected"}, ErrWorkerDisconnected)
}

func TestModelWatchdogsReleaseOutputDrainers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits ModelCallLimits
		code   string
	}{
		{"whole call", ModelCallLimits{Timeout: time.Millisecond, IdleTimeout: 40 * time.Millisecond}, "call_timeout"},
		{"idle", ModelCallLimits{Timeout: time.Hour, IdleTimeout: 10 * time.Millisecond}, "idle_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestModelCall(t, tc.limits)
			if _, err := h.Wait(context.Background()); err == nil {
				t.Fatal("watchdog did not stop the call")
			}
			select {
			case _, ok := <-h.Deltas():
				if ok {
					t.Fatal("watchdog returned unexpected output")
				}
			case <-time.After(100 * time.Millisecond):
				t.Fatal("watchdog left its output drain open")
			}
			// A late remote success cannot replace the local stop decision.
			h.finish(ModelCallOutcome{FinishReason: ModelFinishStop}, nil)
			out, err := h.Wait(context.Background())
			if err == nil || out.ErrorCode != tc.code {
				t.Fatalf("late end replaced watchdog: %+v, %v", out, err)
			}
		})
	}
}

func TestDisconnectedStreamReleasesBackpressuredModel(t *testing.T) {
	h, _ := newTestModelCall(t, ModelCallLimits{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.streamDone = ctx.Done()
	for n := 0; n < cap(h.deltas); n++ {
		h.deliverDelta(ModelCallDelta{Seq: uint64(n + 1)})
	}
	returned := make(chan struct{})
	go func() {
		h.deliverDelta(ModelCallDelta{Seq: uint64(cap(h.deltas) + 1)})
		close(returned)
	}()
	cancel()
	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("full model buffer held the disconnected receive handler")
	}
	h.finish(ModelCallOutcome{Error: "worker_disconnected"}, ErrWorkerDisconnected)
}

// Exercise both orders under -race: shutdown wins, or an in-flight producer
// reaches a full buffer first. No shutdown may close a channel beneath a send.
func TestOutputDeliveryAndFinishAreSafeTogether(t *testing.T) {
	for n := 0; n < 100; n++ {
		app := &AppSessionHandle{chunks: make(chan AppSessionChunk, 1), done: make(chan struct{})}
		model, _ := newTestModelCall(t, ModelCallLimits{})
		model.deltas = make(chan ModelCallDelta, 1)
		app.chunks <- AppSessionChunk{}
		model.deltas <- ModelCallDelta{}
		var wg sync.WaitGroup
		wg.Add(4)
		go func() { defer wg.Done(); app.deliverChunk(AppSessionChunk{Seq: 1}) }()
		go func() { defer wg.Done(); app.finish(AppSessionOutcome{}, nil) }()
		go func() { defer wg.Done(); model.deliverDelta(ModelCallDelta{Seq: 1}) }()
		go func() { defer wg.Done(); model.finish(ModelCallOutcome{}, nil) }()
		wg.Wait()
	}
}
