package tools

import (
	"context"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// setPreflightHooks swaps the package-level preflight hooks for the
// duration of a test and restores the previous values on cleanup.
// The hooks are package-level injection points (see preflight.go /
// tcc_preflight.go); in the test binary the worker package's init
// wiring never runs, so they start nil and each test sets exactly
// what it needs.
func setPreflightHooks(t *testing.T, access func() bool, capture func() bool, display func() string) {
	t.Helper()
	prevAccess := AccessibilityPreflightHook
	prevCapture := ScreenCapturePreflightHook
	prevDisplay := DisplayServerPreflightHook
	AccessibilityPreflightHook = access
	ScreenCapturePreflightHook = capture
	DisplayServerPreflightHook = display
	t.Cleanup(func() {
		AccessibilityPreflightHook = prevAccess
		ScreenCapturePreflightHook = prevCapture
		DisplayServerPreflightHook = prevDisplay
	})
}

func dispatchComputerAction(t *testing.T, d *Dispatcher, action string) (*memqlv1.Success, *memqlv1.Failure) {
	t.Helper()
	return d.Dispatch(context.Background(), &memqlv1.ToolDispatch{
		Tool:     "workerComputer",
		Action:   action,
		ArgsJson: []byte(`{}`),
		CallId:   "preflight-" + action,
	})
}

// computerInputActions is the full input-action surface the
// Accessibility preflight must gate. Kept in the test (not derived
// from isComputerInputAction) so a handler added to the router
// without a matching preflight classification shows up as a
// conscious decision here.
var computerInputActions = []string{
	"mouse_move", "mouse_click", "mouse_down", "mouse_up", "mouse_drag", "mouse_scroll",
	"key_type", "key_combo", "key_hold",
}

// TestDispatcher_AccessibilityPreflightDeniesInputActions: when the
// Accessibility hook reports false (grant revoked mid-session),
// every input action short-circuits with a structured
// permission_denied + remediation hint BEFORE any RobotGo code runs
// -- on both build variants (memql-cockpit#164).
func TestDispatcher_AccessibilityPreflightDeniesInputActions(t *testing.T) {
	setPreflightHooks(t, func() bool { return false }, nil, nil)
	d := NewDispatcher(quietLogger(), DefaultPolicy(), nil)

	for _, action := range computerInputActions {
		success, failure := dispatchComputerAction(t, d, action)
		if success != nil {
			t.Fatalf("%s: expected nil success when Accessibility preflight denies; got %+v", action, success)
		}
		if failure == nil || failure.GetErrorCode() != "permission_denied" {
			t.Errorf("%s: expected permission_denied; got %+v", action, failure)
			continue
		}
		if !strings.Contains(failure.GetErrorMessage(), "Accessibility") {
			t.Errorf("%s: message should name the Accessibility permission; got %q", action, failure.GetErrorMessage())
		}
		if !strings.Contains(failure.GetErrorMessage(), "System Settings") {
			t.Errorf("%s: message should carry the System Settings remediation hint; got %q", action, failure.GetErrorMessage())
		}
	}
}

// TestDispatcher_AccessibilityPreflightLeavesScreenshotAlone: the
// Accessibility gate applies to input actions only. screenshot is
// gated by the Screen Recording hook instead -- with both hooks
// denying, screenshot must fail with the Screen Recording message,
// never the Accessibility one.
func TestDispatcher_AccessibilityPreflightLeavesScreenshotAlone(t *testing.T) {
	setPreflightHooks(t, func() bool { return false }, func() bool { return false }, nil)
	d := NewDispatcher(quietLogger(), DefaultPolicy(), nil)

	_, failure := dispatchComputerAction(t, d, "screenshot")
	if failure == nil || failure.GetErrorCode() != "permission_denied" {
		t.Fatalf("expected permission_denied from the Screen Recording preflight; got %+v", failure)
	}
	if !strings.Contains(failure.GetErrorMessage(), "Screen Recording") {
		t.Errorf("screenshot denial should name Screen Recording; got %q", failure.GetErrorMessage())
	}
	if strings.Contains(failure.GetErrorMessage(), "Accessibility") {
		t.Errorf("screenshot must not be gated by the Accessibility preflight; got %q", failure.GetErrorMessage())
	}
}

// TestDispatcher_AccessibilityPreflightTrueAdmits pins the admit
// path: a hook that reports trusted lets input actions through to
// the per-build router. Headless-only -- on the computeruse build an
// admitted mouse_move would drive the real cursor.
func TestDispatcher_AccessibilityPreflightTrueAdmits(t *testing.T) {
	if buildHasComputerUse {
		t.Skip("admit path would drive real input on the computeruse build; covered headless")
	}
	calls := 0
	setPreflightHooks(t, func() bool { calls++; return true }, nil, nil)
	d := NewDispatcher(quietLogger(), DefaultPolicy(), nil)

	_, failure := dispatchComputerAction(t, d, "mouse_move")
	if calls != 1 {
		t.Errorf("Accessibility hook should be consulted exactly once; got %d", calls)
	}
	if failure == nil || failure.GetErrorCode() != "computeruse_unavailable" {
		t.Errorf("admitted headless mouse_move should reach the router (computeruse_unavailable); got %+v", failure)
	}
}

// TestDispatcher_DisplayServerPreflightBlocksWayland: on a Wayland
// session (linux && computeruse wires the hook to the #162 detection) every
// input AND screenshot action fails with a structured
// display_server_unsupported pointing at an X11/XWayland session,
// instead of an opaque RobotGo failure.
func TestDispatcher_DisplayServerPreflightBlocksWayland(t *testing.T) {
	setPreflightHooks(t, nil, nil, func() string { return "wayland" })
	d := NewDispatcher(quietLogger(), DefaultPolicy(), nil)

	for _, action := range append([]string{"screenshot"}, computerInputActions...) {
		success, failure := dispatchComputerAction(t, d, action)
		if success != nil {
			t.Fatalf("%s: expected nil success on a wayland session; got %+v", action, success)
		}
		if failure == nil || failure.GetErrorCode() != "display_server_unsupported" {
			t.Errorf("%s: expected display_server_unsupported; got %+v", action, failure)
			continue
		}
		if !strings.Contains(failure.GetErrorMessage(), "X11") {
			t.Errorf("%s: message should point at an X11 session; got %q", action, failure.GetErrorMessage())
		}
		if !strings.Contains(failure.GetErrorMessage(), "Wayland") {
			t.Errorf("%s: message should name the detected Wayland session; got %q", action, failure.GetErrorMessage())
		}
	}
}

// TestDispatcher_DisplayServerPreflightBlocksNone: with no display
// server at all the same structured failure fires, with the
// DISPLAY-unset explanation.
func TestDispatcher_DisplayServerPreflightBlocksNone(t *testing.T) {
	setPreflightHooks(t, nil, nil, func() string { return "none" })
	d := NewDispatcher(quietLogger(), DefaultPolicy(), nil)

	_, failure := dispatchComputerAction(t, d, "mouse_click")
	if failure == nil || failure.GetErrorCode() != "display_server_unsupported" {
		t.Fatalf("expected display_server_unsupported with no display; got %+v", failure)
	}
	if !strings.Contains(failure.GetErrorMessage(), "DISPLAY") {
		t.Errorf("message should explain DISPLAY is unset; got %q", failure.GetErrorMessage())
	}
}

// TestDispatcher_DisplayServerPreflightX11Admits pins the admit
// path: an x11 session passes the gate and the dispatch reaches the
// per-build router. Headless-only for the same reason as the
// Accessibility admit test.
func TestDispatcher_DisplayServerPreflightX11Admits(t *testing.T) {
	if buildHasComputerUse {
		t.Skip("admit path would drive real input on the computeruse build; covered headless")
	}
	setPreflightHooks(t, nil, nil, func() string { return "x11" })
	d := NewDispatcher(quietLogger(), DefaultPolicy(), nil)

	_, failure := dispatchComputerAction(t, d, "mouse_move")
	if failure == nil || failure.GetErrorCode() != "computeruse_unavailable" {
		t.Errorf("admitted headless mouse_move should reach the router (computeruse_unavailable); got %+v", failure)
	}
}

// TestDispatcher_CapabilitiesBypassesPreflight: introspection must
// work even (especially) when every preflight would deny -- the
// dispatcher routes `capabilities` before preflightComputerAction.
func TestDispatcher_CapabilitiesBypassesPreflight(t *testing.T) {
	setPreflightHooks(t,
		func() bool { return false },
		func() bool { return false },
		func() string { return "wayland" },
	)
	d := NewDispatcher(quietLogger(), DefaultPolicy(), nil)

	success, failure := dispatchComputerAction(t, d, "capabilities")
	if failure != nil {
		t.Fatalf("capabilities must never be preflight-gated; got %s: %s", failure.GetErrorCode(), failure.GetErrorMessage())
	}
	if success == nil {
		t.Fatal("expected a success payload from capabilities")
	}
}

func TestIsComputerInputAction(t *testing.T) {
	for _, action := range computerInputActions {
		if !isComputerInputAction(action) {
			t.Errorf("isComputerInputAction(%q) = false, want true", action)
		}
	}
	// wait is deliberately exempt (memql-cockpit#166): a pure sleep
	// posts no input events, so it must never trip the Accessibility
	// or display-server preflights.
	for _, action := range []string{"screenshot", "cursor_position", "display_info", "capabilities", "wait", "window_list", "window_focus", ""} {
		if isComputerInputAction(action) {
			t.Errorf("isComputerInputAction(%q) = true, want false", action)
		}
	}
}

// TestDispatcher_WaitBypassesPreflight: like capabilities, wait is
// build-agnostic and permission-free -- it must succeed even when
// every preflight hook would deny (memql-cockpit#166). Runs on both
// build variants: a sleep drives no real input.
func TestDispatcher_WaitBypassesPreflight(t *testing.T) {
	setPreflightHooks(t,
		func() bool { return false },
		func() bool { return false },
		func() string { return "wayland" },
	)
	d := NewDispatcher(quietLogger(), DefaultPolicy(), nil)

	success, failure := d.Dispatch(context.Background(), &memqlv1.ToolDispatch{
		Tool:     "workerComputer",
		Action:   "wait",
		ArgsJson: []byte(`{"ms": 1}`),
		CallId:   "preflight-wait",
	})
	if failure != nil {
		t.Fatalf("wait must never be preflight-gated; got %s: %s", failure.GetErrorCode(), failure.GetErrorMessage())
	}
	if success == nil {
		t.Fatal("expected a success payload from wait")
	}
}
