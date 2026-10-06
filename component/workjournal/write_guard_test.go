package workjournal

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWriteGuardCoversEveryWriteAndLeavesSharedJournalUnchanged(t *testing.T) {
	engine := &countingEngine{}
	base := New(engine, nil, "worker")
	base.beat = time.Hour
	held := false
	refused := errors.New("another owner took over")
	guards := 0
	guarded := base.WithWriteGuard(func(ctx context.Context, write func(context.Context) error) error {
		guards++
		if held {
			return refused
		}
		return write(ctx)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run, err := guarded.Begin(ctx, pipelineWork())
	if err != nil {
		t.Fatal(err)
	}
	defer run.stopHeartbeat()
	if guards != len(engine.calls) || guards < 7 {
		t.Fatalf("unguarded initial writes: %d guards, %d writes", guards, len(engine.calls))
	}
	step, err := run.Step(ctx, "checks/build-vet")
	if err != nil {
		t.Fatal(err)
	}
	held = true
	before := len(engine.calls)
	if err := step.Finish(ctx, Receipt{Status: "done"}); !errors.Is(err, refused) {
		t.Fatalf("stale receipt: %v", err)
	}
	run.Heartbeat(ctx)
	if err := guarded.StampBinding(ctx, "user-1", "step", map[string]any{"nodeId": "old"}); !errors.Is(err, refused) {
		t.Fatalf("stale binding: %v", err)
	}
	reopened := guarded.Reopen("user-1", run.GoalID(), run.RunID(), nil, clockStart)
	if _, err := reopened.Step(ctx, "another"); !errors.Is(err, refused) {
		t.Fatalf("stale reopened intent: %v", err)
	}
	if err := run.Succeeded(ctx, nil); !errors.Is(err, refused) {
		t.Fatalf("stale terminal: %v", err)
	}
	if len(engine.calls) != before {
		t.Fatal("a refused writer reached the engine")
	}
	// The per-run guard must never mutate the shared journal used by other work.
	unguarded := base.Reopen("user-1", run.GoalID(), run.RunID(), nil, clockStart)
	unguarded.Heartbeat(ctx)
	if len(engine.calls) != before+1 {
		t.Fatal("shared journal inherited run guard")
	}
}

func TestWriteGuardsComposeWithoutRemovingPriorAuthority(t *testing.T) {
	engine := &countingEngine{}
	denied := errors.New("first guard refused")
	first := New(engine, nil, "worker").WithWriteGuard(func(context.Context, func(context.Context) error) error { return denied })
	second := first.WithWriteGuard(func(ctx context.Context, write func(context.Context) error) error { return write(ctx) })
	for _, journal := range []*Journal{first, second, second.WithWriteGuard(nil)} {
		if _, err := journal.Begin(context.Background(), work()); !errors.Is(err, denied) {
			t.Fatalf("guard removed: %v", err)
		}
	}
	if len(engine.calls) != 0 {
		t.Fatal("refused writes reached the engine")
	}
}

func TestBackgroundHeartbeatUsesTheWriteGuard(t *testing.T) {
	var writes atomic.Int64
	journal := New(ExecutorFunc(func(context.Context, string) (any, error) { writes.Add(1); return nil, nil }), nil, "worker")
	journal.beat = time.Millisecond
	var mu sync.Mutex
	revoked := false
	rejected := make(chan struct{}, 1)
	guarded := journal.WithWriteGuard(func(ctx context.Context, write func(context.Context) error) error {
		mu.Lock()
		defer mu.Unlock()
		if revoked {
			select {
			case rejected <- struct{}{}:
			default:
			}
			return errors.New("ownership revoked")
		}
		return write(ctx)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run, err := guarded.Begin(ctx, work())
	if err != nil {
		t.Fatal(err)
	}
	defer run.stopHeartbeat()
	mu.Lock()
	revoked = true
	before := writes.Load()
	mu.Unlock()
	select {
	case <-rejected:
	case <-time.After(time.Second):
		t.Fatal("heartbeat never reached guard")
	}
	if writes.Load() != before {
		t.Fatal("background heartbeat wrote after revocation")
	}
}
