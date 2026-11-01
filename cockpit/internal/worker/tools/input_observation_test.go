package tools

import (
	"errors"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

type monitoredInputBackend struct {
	*fakeInputBackend
	activityState inputActivity
	unavailable   bool
}

func (b *monitoredInputBackend) inputActivity() (inputActivity, bool) {
	return b.activityState, !b.unavailable
}

func TestCompletedGestureRequiresNewStableObservation(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		g, base, ctx := newInputFixture(t)
		b := &monitoredInputBackend{fakeInputBackend: base}
		g.backend = b
		token := inputToken(t, g, ctx)
		if !delayed {
			b.onEmit = func(e targetInputEvent) {
				if e.release {
					b.current.field++
				}
			}
		}
		if _, fail := g.input(ctx, "key_combo", map[string]any{"inputTarget": token, "keys": []any{"f6"}}); fail != nil {
			t.Fatal(fail)
		}
		if delayed {
			b.current.field++
		}
		if !g.pendingObservation {
			t.Fatal("gesture must await observation")
		}
		if _, fail := g.input(ctx, "key_type", map[string]any{"inputTarget": token, "text": "never"}); fail == nil || len(b.events) != 2 {
			t.Fatal("old authority survived")
		}
		b.onEmit = nil
		fresh := inputToken(t, g, ctx)
		if g.pendingObservation || g.anchor.(*fakeInputFocus).field != 8 {
			t.Fatal("stable observation not bound")
		}
		if _, fail := g.input(ctx, "key_type", map[string]any{"inputTarget": fresh, "text": "x"}); fail != nil {
			t.Fatal(fail)
		}
	}
}

func TestPendingObservationCannotHideInterruption(t *testing.T) {
	for _, mode := range []string{"human", "activation", "monitor", "window", "pid", "geometry", "expired", "other-run", "focus-error", "unstable-capture"} {
		t.Run(mode, func(t *testing.T) {
			g, base, ctx := newInputFixture(t)
			b := &monitoredInputBackend{fakeInputBackend: base}
			g.backend = b
			token := inputToken(t, g, ctx)
			if _, f := g.input(ctx, "key_combo", map[string]any{"inputTarget": token, "keys": []any{"enter"}}); f != nil {
				t.Fatal(f)
			}
			b.current.field++
			capture := fakeInputCapture
			switch mode {
			case "human":
				b.activityState.counters[0]++
			case "activation":
				b.activityState.activation++
			case "monitor":
				b.unavailable = true
			case "window":
				b.current.w.ID++
			case "pid":
				b.current.w.PID++
			case "geometry":
				b.current.w.Width++
			case "expired":
				g.now = func() time.Time { return g.pendingUntil }
			case "other-run":
				g.pendingScope.run = "other"
			case "focus-error":
				b.err = errors.New("fixture focus unavailable")
			case "unstable-capture":
				capture = func(a map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
					b.current.field++
					return fakeInputCapture(a)
				}
			}
			if _, f := g.screenshot(ctx, map[string]any{"windowId": 42}, capture); f == nil || !g.halted || len(b.events) != 2 || g.firstInterruption == nil {
				t.Fatalf("%s bypassed guard: %v", mode, f)
			}
			first := *g.firstInterruption
			g.stop("different", "later", "later")
			if *g.firstInterruption != first {
				t.Fatal("first cause overwritten")
			}
			if _, f := g.screenshot(ctx, map[string]any{"windowId": 42}, fakeInputCapture); f == nil {
				t.Fatal("latch cleared")
			}
		})
	}
}

func TestHumanActivityStopsEveryInputEventAndCapture(t *testing.T) {
	for _, atCapture := range []bool{false, true} {
		g, base, ctx := newInputFixture(t)
		b := &monitoredInputBackend{fakeInputBackend: base}
		g.backend = b
		token := inputToken(t, g, ctx)
		if atCapture {
			_, f := g.screenshot(ctx, map[string]any{"windowId": 42}, func(a map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
				b.activityState.counters[8]++
				return fakeInputCapture(a)
			})
			if f == nil || len(b.events) != 0 || !g.halted {
				t.Fatal("capture activity ignored")
			}
		} else {
			b.onEmit = func(e targetInputEvent) {
				if e.down {
					b.activityState.counters[8]++
				}
			}
			_, f := g.input(ctx, "key_type", map[string]any{"inputTarget": token, "text": "abc"})
			if f == nil || len(b.events) != 2 || !b.events[1].release || b.targets[1].PID != 100 {
				t.Fatal("human activity did not stop sequence with original-target release")
			}
		}
	}
}

func TestUnobservedFieldChangeStillStopsWithoutGesture(t *testing.T) {
	g, base, ctx := newInputFixture(t)
	g.backend = &monitoredInputBackend{fakeInputBackend: base}
	inputToken(t, g, ctx)
	base.current.field++
	if _, f := g.screenshot(ctx, map[string]any{"windowId": 42}, fakeInputCapture); f == nil || !g.halted {
		t.Fatal("unattributed field change adopted")
	}
}

func TestTransitionObservationDeadlineIsCheckedAfterCapture(t *testing.T) {
	g, base, ctx := newInputFixture(t)
	g.backend = &monitoredInputBackend{fakeInputBackend: base}
	token := inputToken(t, g, ctx)
	nearExpiry := g.frame.expires.Add(-time.Millisecond)
	g.now = func() time.Time { return nearExpiry }
	if _, f := g.input(ctx, "key_combo", map[string]any{"inputTarget": token, "keys": []any{"f6"}}); f != nil {
		t.Fatal(f)
	}
	if g.pendingUntil.Sub(nearExpiry) != inputTargetLifetime {
		t.Fatal("observation lifetime reused old input expiry")
	}
	base.current.field++
	_, f := g.screenshot(ctx, map[string]any{"windowId": 42}, func(a map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
		g.now = func() time.Time { return g.pendingUntil }
		return fakeInputCapture(a)
	})
	if f == nil || !g.halted || g.frame != nil || g.firstInterruption.Reason != "transition_observation_expired_or_interrupted" {
		t.Fatal("capture crossed transition deadline")
	}
}

func TestImplementedActivityMonitorMustRemainAvailable(t *testing.T) {
	for _, phase := range []string{"initial", "capture", "input"} {
		t.Run(phase, func(t *testing.T) {
			g, base, ctx := newInputFixture(t)
			b := &monitoredInputBackend{fakeInputBackend: base}
			g.backend = b
			switch phase {
			case "initial":
				b.unavailable = true
				if _, f := g.screenshot(ctx, map[string]any{"windowId": 42}, fakeInputCapture); f == nil || g.frame != nil || g.anchor != nil {
					t.Fatal("unavailable monitor granted initial receipt")
				}
			case "capture":
				if _, f := g.screenshot(ctx, map[string]any{"windowId": 42}, func(a map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
					b.unavailable = true
					return fakeInputCapture(a)
				}); f == nil || g.frame != nil || !g.halted {
					t.Fatal("monitor loss during capture granted receipt")
				}
			case "input":
				token := inputToken(t, g, ctx)
				b.unavailable = true
				if _, f := g.input(ctx, "key_combo", map[string]any{"inputTarget": token, "keys": []any{"enter"}}); f == nil || !g.halted {
					t.Fatal("monitor loss during input ignored")
				}
			}
			if len(b.events) != 0 {
				t.Fatal("unavailable monitor posted input")
			}
		})
	}
}
