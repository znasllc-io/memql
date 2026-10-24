package tools

import (
	"context"
	"errors"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

type fakeActivationBackend struct {
	*fakeInputBackend
	target                     fakeInputFocus
	identity, observedIdentity int
	activity, observedActivity int
	calls, closed              int
	err                        error
	waitBeforeActivation       time.Duration
}
type fakeActivation struct {
	backend            *fakeActivationBackend
	target             fakeInputFocus
	identity, activity int
}

func (b *fakeActivationBackend) observeWindow(_ context.Context, w WindowInfo) (windowActivation, error) {
	if !sameWindowRecord(w, b.target.w) {
		return nil, errors.New("ambiguous or absent AX window")
	}
	return &fakeActivation{backend: b, target: b.target, identity: b.identity, activity: b.activity}, nil
}
func (a *fakeActivation) activate(ctx context.Context) (inputFocus, error) {
	b := a.backend
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if a.identity != b.identity || a.activity != b.activity {
		return nil, errors.New("identity or activity changed")
	}
	if b.waitBeforeActivation > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(b.waitBeforeActivation):
		}
	}
	b.calls++
	if b.err != nil {
		return nil, b.err
	}
	b.current = b.target
	return b.focus()
}
func (a *fakeActivation) close() { a.backend.closed++ }
func activationFixture(t *testing.T) (*inputTargetGuard, *fakeActivationBackend, context.Context, map[string]any) {
	g, b, ctx := newInputFixture(t)
	a := &fakeActivationBackend{fakeInputBackend: b, target: b.current, identity: 1}
	g.backend = a
	b.current.w.ID = 7
	_, fail := g.windowList(ctx, map[string]any{"windowId": 42, "pid": 100}, func() ([]WindowInfo, *memqlv1.Failure) { return []WindowInfo{b.current.w, a.target.w}, nil })
	if fail != nil {
		t.Fatal(fail)
	}
	t.Cleanup(g.clearSelection)
	return g, a, ctx, map[string]any{"windowId": 42, "pid": 100, "focusTarget": g.selection.id}
}

func TestWindowSelectionRequiresFreshScreenshotAndCannotRepeat(t *testing.T) {
	g, b, ctx, args := activationFixture(t)
	success, fail := g.input(ctx, "window_focus", args)
	if fail != nil || success == nil || g.anchor == nil || g.frame != nil || b.calls != 1 || len(b.events) != 0 {
		t.Fatalf("selection: %v %v calls=%d", success, fail, b.calls)
	}
	if _, fail = g.input(ctx, "key_type", map[string]any{"text": "x"}); fail == nil || len(b.events) != 0 {
		t.Fatal("activation authorized typing without a screenshot")
	}
	if _, fail = g.input(ctx, "window_focus", args); fail == nil || b.calls != 1 {
		t.Fatal("selection replayed")
	}
	token := inputToken(t, g, ctx)
	if _, fail = g.input(ctx, "key_type", map[string]any{"text": "x", "inputTarget": token}); fail != nil || len(b.events) != 2 {
		t.Fatalf("fresh screenshot input: %v", fail)
	}
}

func TestWindowSelectionRefusesInvalidReceipts(t *testing.T) {
	for _, kind := range []string{"run", "dispatcher", "token", "pid", "window", "expired", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			g, b, ctx, args := activationFixture(t)
			switch kind {
			case "run":
				owner, _ := inputOwner(ctx)
				owner.run = "other"
				ctx = context.WithValue(ctx, inputScopeContextKey{}, owner)
			case "dispatcher":
				owner, _ := inputOwner(ctx)
				owner.dispatcher = &Dispatcher{}
				ctx = context.WithValue(ctx, inputScopeContextKey{}, owner)
			case "token":
				args["focusTarget"] = "other"
			case "pid":
				args["pid"] = 101
			case "window":
				args["windowId"] = 7 // same process, different window
			case "expired":
				g.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			if _, fail := g.input(ctx, "window_focus", args); fail == nil || b.calls != 0 || len(b.events) != 0 {
				t.Fatalf("%s accepted: %v", kind, fail)
			}
		})
	}
}

func TestWindowSelectionInterruptionsLatchAndCannotBeRefreshed(t *testing.T) {
	for _, kind := range []string{"identity", "activity", "geometry", "otherWindow", "failed", "halted"} {
		t.Run(kind, func(t *testing.T) {
			g, b, ctx, args := activationFixture(t)
			switch kind {
			case "identity":
				b.identity++ // replaced AX window/process, same numeric ID
			case "activity":
				b.activity++ // interrupted between observation and activation
			case "geometry":
				b.target.w.Width++
			case "otherWindow":
				b.target.w.ID = 7 // backend accidentally activates app's other window
			case "failed":
				b.err = errors.New("activation refused")
			case "halted":
				g.halted = true
			}
			if _, fail := g.input(ctx, "window_focus", args); fail == nil || !g.halted || g.anchor != nil || len(b.events) != 0 {
				t.Fatalf("%s failed open: %v", kind, fail)
			}
			before := b.calls
			if _, fail := g.windowList(ctx, map[string]any{"windowId": 42, "pid": 100}, func() ([]WindowInfo, *memqlv1.Failure) { return []WindowInfo{b.target.w}, nil }); fail == nil || !g.halted {
				t.Fatal("new list cleared interruption")
			}
			if _, fail := g.input(ctx, "window_focus", args); fail == nil || b.calls != before {
				t.Fatal("activation retried after interruption")
			}
		})
	}
}

func TestWindowSelectionInvalidatesInputReceiptAndPreservesAnchor(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "bound", true: "interrupted"}[interrupt], func(t *testing.T) {
			g, b, ctx, args := activationFixture(t)
			b.current = b.target
			inputToken(t, g, ctx)
			anchor := g.anchor
			if interrupt {
				b.current.w.ID = 7
			}
			if _, fail := g.input(ctx, "window_focus", args); fail == nil || b.calls != 0 || g.frame != nil || g.anchor != anchor || g.halted != interrupt {
				t.Fatalf("anchor was bypassed: %v", fail)
			}
		})
	}
}

func TestWindowSelectionConcurrentReceiptHasOneActivation(t *testing.T) {
	g, b, ctx, args := activationFixture(t)
	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() { g.input(ctx, "window_focus", args); done <- struct{}{} }()
	}
	<-done
	<-done
	if b.calls != 1 || len(b.events) != 0 {
		t.Fatalf("activation calls=%d", b.calls)
	}
}

func TestWindowSelectionExpiringWhileQueuedCannotActivate(t *testing.T) {
	g, b, ctx, args := activationFixture(t)
	g.selection.expires = time.Now().Add(10 * time.Millisecond)
	b.waitBeforeActivation = 60 * time.Millisecond
	if _, fail := g.input(ctx, "window_focus", args); fail == nil || b.calls != 0 || !g.halted {
		t.Fatalf("expired while waiting but activated: fail=%v calls=%d halted=%t", fail, b.calls, g.halted)
	}
}
