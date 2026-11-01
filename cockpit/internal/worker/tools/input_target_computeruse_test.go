//go:build computeruse

package tools

import (
	"context"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// Replace the actual input backend before exercising the dispatcher. This
// regression must never type into the developer's desktop, even on the old code.
func TestComputerInputRequiresObservedTarget(t *testing.T) {
	for _, action := range []string{"key_type", "key_combo", "key_hold", "mouse_move", "mouse_click", "mouse_down", "mouse_up", "mouse_drag", "mouse_scroll", "window_focus"} {
		t.Run(action, func(t *testing.T) {
			old := computerActionHandlers[action]
			t.Cleanup(func() { computerActionHandlers[action] = old })
			emitted := 0
			computerActionHandlers[action] = func(map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
				emitted++
				return &memqlv1.Success{}, nil
			}
			d := NewDispatcher(nil, nil, nil)
			_, refusal := d.dispatchComputer(context.Background(), action, map[string]any{"text": "museum inventory", "keys": []any{"enter"}, "windowId": 42})
			if refusal == nil || emitted != 0 {
				t.Fatalf("input without a screenshot-bound target: refusal=%v, emitted=%d; want rejection and zero input", refusal, emitted)
			}
		})
	}
}

// Exercise the production dispatcher/run binding with a fake backend. No
// permission probe or legacy GUI handler can emit real input in this test.
func TestComputerDispatcherPreservesInputOwner(t *testing.T) {
	g, b, _ := newInputFixture(t)
	old := desktopInputGuard
	desktopInputGuard = g
	t.Cleanup(func() { desktopInputGuard = old })
	d := NewDispatcher(quietLogger(), DefaultPolicy(), nil)
	ctx := context.WithValue(context.Background(), inputScopeContextKey{}, inputScope{dispatcher: d, run: "museum-run"})
	token := inputToken(t, g, ctx)
	args := []byte(`{"text":"x","inputTarget":"` + token + `"}`)
	_, fail := d.Dispatch(context.Background(), &memqlv1.ToolDispatch{Tool: "workerComputer", Action: "key_type", RunId: "museum-run", ArgsJson: args, CallId: "target-bound"})
	if fail != nil || len(b.events) != 2 {
		t.Fatalf("owned dispatcher input: %v events=%d", fail, len(b.events))
	}
	token = inputToken(t, g, ctx)
	b.events = nil
	args = []byte(`{"text":"x","inputTarget":"` + token + `"}`)
	_, fail = d.Dispatch(context.Background(), &memqlv1.ToolDispatch{Tool: "workerComputer", Action: "key_type", RunId: "other-run", ArgsJson: args, CallId: "wrong-owner"})
	if fail == nil || len(b.events) != 0 {
		t.Fatal("dispatcher lost run ownership")
	}
}
