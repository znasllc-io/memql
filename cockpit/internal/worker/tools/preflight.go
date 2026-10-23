package tools

import (
	"fmt"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// AccessibilityPreflightHook is a per-call preflight check for the
// macOS Accessibility permission (the TCC entry that gates synthetic
// mouse + keyboard events). Returns true when the calling process is
// trusted (AXIsProcessTrusted), false otherwise.
//
// Set at init time by the worker package's darwin && computeruse build
// (internal/worker/tcc_hook_darwin.go) and nil on
// every other platform / build configuration -- nil means "the
// platform doesn't gate this; proceed". Same injection rationale as
// ScreenCapturePreflightHook (tcc_preflight.go): the dispatcher stays
// platform-agnostic while the worker package owns the cgo detail.
var AccessibilityPreflightHook func() bool

// DisplayServerPreflightHook reports the display server the host
// session exposes ("x11" / "wayland" / "none"). Set at init time by
// the worker package's linux && computeruse build
// (internal/worker/display_preflight_linux.go) --
// RobotGo drives X11 only, so any non-x11 answer means input +
// screenshot actions cannot work and must fail with a structured
// `display_server_unsupported` instead of an opaque RobotGo error.
// Nil on every other platform / build configuration: macOS always
// has Quartz, and the headless build rejects computer-use actions before any
// display question arises.
var DisplayServerPreflightHook func() string

// isComputerInputAction reports whether the workerComputer action
// posts synthetic input events (the operations macOS gates behind
// the Accessibility TCC entry). screenshot / cursor_position /
// display_info are reads and are deliberately excluded, as are the
// build-agnostic `capabilities` and `wait` (memql-cockpit#166) --
// a pure sleep posts no events and needs neither Accessibility nor
// a display server.
func isComputerInputAction(action string) bool {
	switch action {
	case "mouse_move", "mouse_click", "mouse_down", "mouse_up", "mouse_drag", "mouse_scroll",
		"key_type", "key_combo", "key_hold":
		return true
	}
	return false
}

// preflightComputerAction runs the per-call platform preflights for
// a workerComputer action BEFORE it reaches the per-build router.
// Returns nil when the action may proceed, or a structured Failure
// with a remediation hint when the platform would make the action
// fail confusingly (memql-cockpit#164).
//
// Checked per call (each check is a cheap synchronous read) so a
// permission revoked mid-session -- or a session switched out from
// under the worker -- surfaces as a clean error on the very next
// dispatch instead of a raw RobotGo error or panic.
//
// The `capabilities` action never reaches this function: the
// dispatcher routes it first, because introspection must work even
// (especially) when everything else is broken.
func preflightComputerAction(action string) *memqlv1.Failure {
	needsDisplay := isComputerInputAction(action) || action == "screenshot"

	// Linux display-server gate: input + screenshot both require a
	// reachable X11 display.
	if needsDisplay && DisplayServerPreflightHook != nil {
		switch server := DisplayServerPreflightHook(); server {
		case "x11":
			// Reachable X11 display; proceed.
		case "wayland":
			return failure("display_server_unsupported", fmt.Sprintf(
				"workerComputer.%s requires an X11 session but this session is Wayland "+
					"(WAYLAND_DISPLAY / XDG_SESSION_TYPE). RobotGo drives X11 only -- "+
					"log into an X11 (Xorg) session, or run the worker inside an "+
					"XWayland-backed X11 environment, then retry.", action))
		default:
			return failure("display_server_unsupported", fmt.Sprintf(
				"workerComputer.%s requires an X11 session but no display server was "+
					"detected (DISPLAY is unset; detected %q). Start the worker inside "+
					"an X11 session, or register it HEADLESS-only.", action, server))
		}
	}

	// macOS Accessibility gate: synthetic input events require the
	// Accessibility TCC grant.
	if isComputerInputAction(action) && AccessibilityPreflightHook != nil && !AccessibilityPreflightHook() {
		return failure("permission_denied", fmt.Sprintf(
			"workerComputer.%s requires the Accessibility permission, which is not "+
				"granted for this binary. Open System Settings -> Privacy & Security -> "+
				"Accessibility, enable the installed MemQL worker, then retry.", action))
	}

	// macOS Screen Recording gate: same TCC pattern, screenshot only.
	if action == "screenshot" && ScreenCapturePreflightHook != nil && !ScreenCapturePreflightHook() {
		return failure("permission_denied",
			"Screen Recording permission is not granted for this binary. "+
				"Open System Settings -> Privacy & Security -> Screen Recording, "+
				"enable the installed MemQL worker, then re-run.")
	}

	return nil
}
