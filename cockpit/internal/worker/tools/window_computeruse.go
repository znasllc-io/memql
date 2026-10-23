//go:build computeruse

package tools

import (
	"fmt"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// window_list / window_focus handlers (memql-cockpit#167). The
// platform-specific work lives behind a two-function seam so this
// file stays portable:
//
//	listWindows() ([]WindowInfo, *memqlv1.Failure)
//	focusWindow(id uint64) (app, method string, fail *memqlv1.Failure)
//
// implemented by window_computeruse_darwin.go (CGWindowList + app
// activation), window_computeruse_linux.go (EWMH over X11; structured
// unsupported error on Wayland), and window_computeruse_other.go (structured
// unsupported error elsewhere).

// guiWindowList enumerates on-screen top-level windows.
//
// Result: {"windows": [{id, title, app, pid, x, y, width, height,
// focused}, ...]} -- coordinates in LOGICAL space (the RobotGo input
// space), see WindowInfo in window.go.
func guiWindowList(_ map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	wins, fail := listWindows()
	if fail != nil {
		return nil, fail
	}
	preview := fmt.Sprintf("%d windows", len(wins))
	for _, w := range wins {
		if w.Focused {
			preview = fmt.Sprintf("%d windows (focused: %s)", len(wins), w.App)
			break
		}
	}
	return successComputerJSON(windowListPayload(wins), preview, 0), nil
}

// guiWindowFocus brings the window identified by `windowId` (number
// or numeric string, from window_list) to the foreground.
//
// Result: {"focused": true, "id": <id>, "app": <owner>, "method":
// <platform mechanism>}. Failure codes: bad_request (missing/invalid
// windowId), window_not_found (id not in the current enumeration),
// window_focus_failed (platform call failed), or
// unsupported_on_platform (Wayland / other platforms).
//
// Focus semantics vary by platform -- see the `method` field:
//   - "app_activate" (macOS): the OWNING APPLICATION is activated,
//     which raises that app's frontmost window. Per-window raise
//     within a multi-window app needs the Accessibility (AX) API and
//     is deferred; v1 semantics are app-level focus.
//   - "ewmh_active_window" (Linux/X11): a true per-window
//     _NET_ACTIVE_WINDOW request to the window manager.
func guiWindowFocus(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	id, fail := parseWindowID(args)
	if fail != nil {
		return nil, fail
	}
	app, method, fail := focusWindow(id)
	if fail != nil {
		return nil, fail
	}
	return successComputerJSON(map[string]any{
		"focused": true,
		"id":      id,
		"app":     app,
		"method":  method,
	}, fmt.Sprintf("focused window %d (%s)", id, app), 0), nil
}
