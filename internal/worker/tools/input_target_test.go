package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// All guard tests use this backend. It cannot access the desktop.
type fakeInputFocus struct {
	w      WindowInfo
	field  int
	closed bool
}

func (f *fakeInputFocus) window() WindowInfo { return f.w }
func (f *fakeInputFocus) same(other inputFocus, field bool) bool {
	o := other.(*fakeInputFocus)
	return f.w == o.w && (!field || f.field == o.field)
}
func (f *fakeInputFocus) close() {
	if f.closed {
		panic("double close")
	}
	f.closed = true
}

type fakeInputBackend struct {
	current fakeInputFocus
	events  []targetInputEvent
	targets []WindowInfo
	onEmit  func(targetInputEvent)
	err     error
	emitErr func(targetInputEvent) error
}

func (b *fakeInputBackend) focus() (inputFocus, error) {
	if b.err != nil {
		return nil, b.err
	}
	copy := b.current
	return &copy, nil
}
func (b *fakeInputBackend) emit(f inputFocus, e targetInputEvent) error {
	b.events = append(b.events, e)
	b.targets = append(b.targets, f.window())
	if b.onEmit != nil {
		b.onEmit(e)
	}
	if b.emitErr != nil {
		return b.emitErr(e)
	}
	return nil
}
func newInputFixture(t *testing.T) (*inputTargetGuard, *fakeInputBackend, context.Context) {
	t.Helper()
	b := &fakeInputBackend{current: fakeInputFocus{w: WindowInfo{ID: 42, PID: 100, X: 0, Y: 0, Width: 800, Height: 600}, field: 7}}
	g := &inputTargetGuard{backend: b, now: time.Now}
	t.Cleanup(func() {
		g.clearFrame()
		if g.anchor != nil {
			g.anchor.close()
		}
	})
	return g, b, context.WithValue(context.Background(), inputScopeContextKey{}, inputScope{dispatcher: &Dispatcher{}, run: "museum-run"})
}
func fakeInputCapture(map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	return successComputerJSON(map[string]any{"width": 400, "height": 300, "sourceWidth": 1600, "sourceHeight": 1200, "logicalWidth": 800, "logicalHeight": 600, "logicalOriginX": 0, "logicalOriginY": 0}, "fake image", 0), nil
}
func inputToken(t *testing.T, g *inputTargetGuard, ctx context.Context) string {
	t.Helper()
	shot, fail := g.screenshot(ctx, map[string]any{"windowId": 42}, fakeInputCapture)
	if fail != nil {
		t.Fatalf("bind: %v", fail)
	}
	var p struct {
		InputTarget struct {
			ID string `json:"id"`
		} `json:"inputTarget"`
	}
	if err := json.Unmarshal(shot.ResultJson, &p); err != nil || p.InputTarget.ID == "" {
		t.Fatalf("receipt: %v %s", err, shot.ResultJson)
	}
	return p.InputTarget.ID
}
func TestInputReceiptOwnershipExpiryAndConsumption(t *testing.T) {
	for _, mode := range []string{"wrong-token", "other-run", "other-home", "expired", "cancelled", "replayed", "bad-args"} {
		t.Run(mode, func(t *testing.T) {
			g, b, ctx := newInputFixture(t)
			token := inputToken(t, g, ctx)
			args := map[string]any{"inputTarget": token, "text": "catalog"}
			switch mode {
			case "wrong-token":
				args["inputTarget"] = "other"
			case "other-run":
				s, _ := inputOwner(ctx)
				s.run = "another"
				ctx = context.WithValue(ctx, inputScopeContextKey{}, s)
			case "other-home":
				s, _ := inputOwner(ctx)
				s.dispatcher = &Dispatcher{}
				ctx = context.WithValue(ctx, inputScopeContextKey{}, s)
			case "expired":
				g.now = func() time.Time { return g.frame.expires }
			case "cancelled":
				cancelCtx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelCtx
			case "replayed":
				if _, f := g.input(ctx, "key_type", args); f != nil {
					t.Fatal(f)
				}
				b.events = nil
			case "bad-args":
				args["text"] = "catalog\n" // implicit submission must not be hidden in text
			}
			if _, f := g.input(ctx, "key_type", args); f == nil || len(b.events) != 0 {
				t.Fatalf("rejection must emit nothing: %v %v", f, b.events)
			}
			if mode == "bad-args" || mode == "cancelled" {
				if !g.frame.used {
					t.Fatal("failed transaction must consume receipt")
				}
			}
		})
	}
}
func TestInputFocusMismatchLatchesAcrossObservationAndRun(t *testing.T) {
	for _, mode := range []string{"app", "window", "field", "geometry", "focus-unavailable"} {
		t.Run(mode, func(t *testing.T) {
			g, b, ctx := newInputFixture(t)
			token := inputToken(t, g, ctx)
			switch mode {
			case "app":
				b.current.w.PID++
			case "window":
				b.current.w.ID++
			case "field":
				b.current.field++
			case "geometry":
				b.current.w.X++
			case "focus-unavailable":
				b.err = errors.New("unavailable")
			}
			if _, f := g.input(ctx, "key_type", map[string]any{"inputTarget": token, "text": "x"}); f == nil || len(b.events) != 0 || !g.halted {
				t.Fatalf("unsafe focus: %v %v", f, b.events)
			}
			if _, f := g.screenshot(ctx, map[string]any{}, fakeInputCapture); f != nil {
				t.Fatal("read-only screenshot unavailable", f)
			}
			b.err = nil
			b.current.w.ID = 42
			s, _ := inputOwner(ctx)
			s.run = "fresh-run"
			ctx = context.WithValue(ctx, inputScopeContextKey{}, s)
			if _, f := g.screenshot(ctx, map[string]any{"windowId": 42}, fakeInputCapture); f == nil {
				t.Fatal("new run/screenshot cleared interruption")
			}
			if _, f := g.input(ctx, "window_focus", map[string]any{"windowId": 42}); f == nil || len(b.events) != 0 {
				t.Fatal("focus activation bypassed interruption")
			}
		})
	}
}
func TestReadOnlyScreenshotCannotEraseFocusAnchor(t *testing.T) {
	g, b, ctx := newInputFixture(t)
	inputToken(t, g, ctx)
	if _, f := g.screenshot(ctx, map[string]any{}, fakeInputCapture); f != nil {
		t.Fatal(f)
	}
	b.current.w.ID = 99
	if _, f := g.screenshot(ctx, map[string]any{"windowId": 99}, fakeInputCapture); f == nil || !g.halted {
		t.Fatal("read-only observation allowed reacquisition")
	}
}

func TestReadOnlyScreenshotDetectsInterruptionBeforeReturnToOriginalApp(t *testing.T) {
	g, b, ctx := newInputFixture(t)
	inputToken(t, g, ctx)
	b.current.w.PID = 200
	shot, f := g.screenshot(ctx, map[string]any{}, fakeInputCapture)
	if f != nil || shot == nil || !g.halted || !strings.Contains(string(shot.ResultJson), "desktopInputStopped") {
		t.Fatal("read-only screenshot missed interruption")
	}
	b.current.w.PID = 100
	if _, f := g.screenshot(ctx, map[string]any{"windowId": 42}, fakeInputCapture); f == nil {
		t.Fatal("returning to original app cleared stop")
	}
}
func TestScreenshotMismatchAndFailedCaptureInvalidateReceipt(t *testing.T) {
	for _, mode := range []string{"capture-focus-change", "failed-capture", "read-only", "new-shot", "wrong-window"} {
		t.Run(mode, func(t *testing.T) {
			g, b, ctx := newInputFixture(t)
			token := inputToken(t, g, ctx)
			args := map[string]any{"windowId": 42}
			capture := fakeInputCapture
			switch mode {
			case "capture-focus-change":
				capture = func(a map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
					b.current.field++
					return fakeInputCapture(a)
				}
			case "failed-capture":
				capture = func(map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
					return nil, failure("capture_failed", "fixture")
				}
			case "read-only":
				args = map[string]any{}
			case "wrong-window":
				args["windowId"] = 99
			}
			g.screenshot(ctx, args, capture)
			if _, f := g.input(ctx, "key_type", map[string]any{"inputTarget": token, "text": "x"}); f == nil || len(b.events) != 0 {
				t.Fatal("obsolete receipt accepted")
			}
		})
	}
}
func TestInputMidSequenceStopsAndReleasesOriginalTarget(t *testing.T) {
	for _, action := range []string{"key_type", "key_combo", "mouse_click"} {
		t.Run(action, func(t *testing.T) {
			g, b, ctx := newInputFixture(t)
			token := inputToken(t, g, ctx)
			b.onEmit = func(e targetInputEvent) {
				if e.down {
					b.current.w.PID = 200
				}
			}
			args := map[string]any{"inputTarget": token}
			switch action {
			case "key_type":
				args["text"] = "abc"
			case "key_combo":
				args["keys"] = []any{"enter"}
			case "mouse_click":
				args["x"] = 20
				args["y"] = 20
			}
			if _, f := g.input(ctx, action, args); f == nil || !g.halted {
				t.Fatal("interrupted sequence succeeded")
			}
			if len(b.events) != 2 || !b.events[0].down || !b.events[1].release {
				t.Fatalf("want one down and cleanup only: %+v", b.events)
			}
			for _, target := range b.targets {
				if target.PID != 100 {
					t.Fatal("cleanup delivered to new foreground")
				}
			}
		})
	}
}
func TestInputIntentionalFieldChangeAndDelayedEffect(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		t.Run(map[bool]string{false: "synchronous-tab", true: "delayed-tab"}[delayed], func(t *testing.T) {
			g, b, ctx := newInputFixture(t)
			token := inputToken(t, g, ctx)
			if !delayed {
				b.onEmit = func(e targetInputEvent) {
					if e.down {
						b.current.field++
					}
				}
			}
			if _, f := g.input(ctx, "key_combo", map[string]any{"inputTarget": token, "keys": []any{"tab"}}); f != nil {
				t.Fatal(f)
			}
			if len(b.events) != 2 || !b.events[1].release {
				t.Fatal("Tab not released")
			}
			if delayed {
				b.current.field++
			}
			_, f := g.screenshot(ctx, map[string]any{"windowId": 42}, fakeInputCapture)
			if delayed && (f == nil || !g.halted) {
				t.Fatal("unattributed delayed effect must fail closed")
			}
			if !delayed && f != nil {
				t.Fatal("observed action effect rejected", f)
			}
		})
	}
}
func TestTargetPointerUsesCapturedGeometryAndRejectsAmbiguity(t *testing.T) {
	g, b, ctx := newInputFixture(t)
	token := inputToken(t, g, ctx)
	if _, f := g.input(ctx, "mouse_click", map[string]any{"inputTarget": token, "x": 150, "y": 100}); f != nil {
		t.Fatal(f)
	}
	if b.events[0].x != 300 || b.events[0].y != 200 {
		t.Fatalf("captured mapping lost: %+v", b.events)
	}
	for _, args := range []map[string]any{{"x": 1.5, "y": 1}, {"x": 1}, {"x": 400, "y": 1}, {"x": "5", "y": 1}, {"x": 1, "y": 1, "count": 3}, {"x": 1, "y": 1, "modifiers": []any{4}}} {
		args["inputTarget"] = inputToken(t, g, ctx)
		b.events = nil
		if _, f := g.input(ctx, "mouse_click", args); f == nil || len(b.events) != 0 {
			t.Fatalf("invalid point emitted: %v", args)
		}
	}
}
func TestInputConcurrentReceiptHasOneWinner(t *testing.T) {
	g, b, ctx := newInputFixture(t)
	token := inputToken(t, g, ctx)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); g.input(ctx, "key_type", map[string]any{"inputTarget": token, "text": "x"}) }()
	}
	wg.Wait()
	if len(b.events) != 2 {
		t.Fatalf("single-use receipt emitted %d events", len(b.events))
	}
}

func TestInputFinalFieldChangeCannotBecomeANewTarget(t *testing.T) {
	for _, action := range []string{"key_type", "mouse_move", "mouse_scroll", "key_combo"} {
		t.Run(action, func(t *testing.T) {
			g, b, ctx := newInputFixture(t)
			token := inputToken(t, g, ctx)
			args := map[string]any{"inputTarget": token}
			switch action {
			case "key_type":
				args["text"] = "x"
			case "key_combo":
				args["keys"] = []any{"a", "cmd"}
			case "mouse_move":
				args["x"] = 10
				args["y"] = 10
			case "mouse_scroll":
				args["x"] = 10
				args["y"] = 10
				args["dy"] = 1
			}
			b.onEmit = func(e targetInputEvent) {
				if e.release || e.kind == "move" || e.kind == "scroll" {
					b.current.field++
				}
			}
			_, f := g.input(ctx, action, args)
			if f == nil || !g.halted || !strings.Contains(f.GetErrorMessage(), "events") {
				t.Fatalf("final focus change lost partial effects: %v", f)
			}
			if g.anchor.(*fakeInputFocus).field != 7 {
				t.Fatal("unexpected field was adopted")
			}
			if _, f := g.screenshot(ctx, map[string]any{"windowId": 42}, fakeInputCapture); f == nil {
				t.Fatal("fresh screenshot cleared interruption")
			}
		})
	}
}

func TestInputCancellationDuringSequenceReleasesWithoutRetry(t *testing.T) {
	g, b, parent := newInputFixture(t)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	token := inputToken(t, g, ctx)
	b.onEmit = func(e targetInputEvent) {
		if e.down {
			cancel()
		}
	}
	if _, f := g.input(ctx, "key_type", map[string]any{"inputTarget": token, "text": "inventory"}); f == nil {
		t.Fatal("cancelled sequence succeeded")
	}
	if len(b.events) != 2 || !b.events[1].release || b.targets[1].PID != 100 {
		t.Fatalf("wrong cancellation cleanup: %+v", b.events)
	}
}

func TestInputGeometryIncludesRegionAndDisplayOrigin(t *testing.T) {
	g, b, ctx := newInputFixture(t)
	b.current.w.X = -700
	b.current.w.Y = 100
	capture := func(map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
		return successComputerJSON(map[string]any{"width": 250, "height": 150, "sourceWidth": 1000, "sourceHeight": 600, "logicalWidth": 500, "logicalHeight": 300, "logicalOriginX": -600, "logicalOriginY": 120}, "region", 0), nil
	}
	if _, f := g.screenshot(ctx, map[string]any{"windowId": 42}, capture); f != nil {
		t.Fatal(f)
	}
	if _, f := g.input(ctx, "mouse_move", map[string]any{"inputTarget": g.frame.id, "x": 100, "y": 50}); f != nil {
		t.Fatal(f)
	}
	if b.events[0].x != -400 || b.events[0].y != 220 {
		t.Fatalf("origin or scale lost: %+v", b.events[0])
	}
}

func TestInputCleanupFailureIsNotReportedAsReleased(t *testing.T) {
	g, b, ctx := newInputFixture(t)
	token := inputToken(t, g, ctx)
	b.onEmit = func(e targetInputEvent) {
		if e.down {
			b.current.w.PID++
		}
	}
	b.emitErr = func(e targetInputEvent) error {
		if e.release {
			return errors.New("fixture release failure")
		}
		return nil
	}
	success, f := g.input(ctx, "key_type", map[string]any{"inputTarget": token, "text": "x"})
	if success != nil || f == nil || !strings.Contains(f.GetErrorMessage(), "may remain down") || !g.halted {
		t.Fatalf("release failure hidden: %v %v", success, f)
	}
	if b.targets[1].PID != 100 {
		t.Fatal("release retargeted")
	}
}
func TestUnsupportedBackendAndActionsNeverEmit(t *testing.T) {
	g, b, ctx := newInputFixture(t)
	for _, action := range []string{"window_focus", "mouse_down", "mouse_up", "mouse_drag", "key_hold"} {
		token := inputToken(t, g, ctx)
		if _, f := g.input(ctx, action, map[string]any{"inputTarget": token}); f == nil || len(b.events) != 0 {
			t.Fatalf("unsupported %s emitted", action)
		}
	}
	g.backend = nil
	if _, f := g.screenshot(ctx, map[string]any{"windowId": 42}, fakeInputCapture); f == nil {
		t.Fatal("missing targeted backend accepted")
	}
	if _, f := g.screenshot(ctx, map[string]any{}, fakeInputCapture); f != nil {
		t.Fatal("read-only capture refused")
	}
}
