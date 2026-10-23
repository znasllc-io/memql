package tools

import (
	"fmt"
	"strconv"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// WindowInfo is one enumerated top-level window, in the shape the
// platform listers (window_computeruse_darwin.go / window_computeruse_linux.go)
// produce and windowListPayload serializes.
//
// Coordinate contract: X/Y/Width/Height are in LOGICAL coordinates --
// the same space RobotGo input takes (CG global display points on
// macOS, root-window pixels on X11). They are deliberately NOT in the
// emitted-screenshot space mouse_move/mouse_drag targets use; a
// consumer that wants to click inside a window must still map through
// the screenshot scale (see coords.go).
type WindowInfo struct {
	// ID is the platform window handle: kCGWindowNumber on macOS,
	// the X window id on Linux. Stable for the window's lifetime;
	// pass it as screenshot.windowId to request a bound input receipt.
	ID uint64
	// Title is the window's own title (kCGWindowName / _NET_WM_NAME).
	// May be empty -- on macOS reading other apps' window titles
	// requires the Screen Recording permission.
	Title string
	// App is the owning application's name (kCGWindowOwnerName /
	// WM_CLASS class).
	App string
	// PID is the owning process id (0 when the platform didn't
	// report one, e.g. an X11 client without _NET_WM_PID).
	PID int
	// X/Y/Width/Height are the window bounds in logical coordinates
	// (see the type comment).
	X, Y, Width, Height int
	// Focused marks the window that currently has input focus
	// (frontmost on macOS, _NET_ACTIVE_WINDOW on X11).
	Focused bool
}

// windowListPayload maps the platform lister's rows to the
// window_list wire shape: {"windows": [{id, title, app, pid, x, y,
// width, height, focused}, ...]}. Pure so the headless build can
// unit-test the shape without a display.
func windowListPayload(wins []WindowInfo) map[string]any {
	rows := make([]any, 0, len(wins))
	for _, w := range wins {
		rows = append(rows, map[string]any{
			"id":      w.ID,
			"title":   w.Title,
			"app":     w.App,
			"pid":     w.PID,
			"x":       w.X,
			"y":       w.Y,
			"width":   w.Width,
			"height":  w.Height,
			"focused": w.Focused,
		})
	}
	return map[string]any{"windows": rows}
}

// parseWindowID extracts window_focus's `windowId` argument. The
// model may send it as a JSON number (the common case -- ids come
// straight from window_list) or as a string; both are accepted.
// Returns a bad_request failure when the argument is missing,
// negative, fractional, or not a number at all.
func parseWindowID(args map[string]any) (uint64, *memqlv1.Failure) {
	v, ok := args["windowId"]
	if !ok {
		return 0, failure("bad_request", "window_focus: windowId is required (use window_list to enumerate ids)")
	}
	switch n := v.(type) {
	case float64:
		if n < 0 || n != float64(uint64(n)) {
			return 0, failure("bad_request", fmt.Sprintf("window_focus: windowId %v is not a non-negative integer", n))
		}
		return uint64(n), nil
	case int:
		if n < 0 {
			return 0, failure("bad_request", fmt.Sprintf("window_focus: windowId %d is not a non-negative integer", n))
		}
		return uint64(n), nil
	case int64:
		if n < 0 {
			return 0, failure("bad_request", fmt.Sprintf("window_focus: windowId %d is not a non-negative integer", n))
		}
		return uint64(n), nil
	case string:
		id, err := strconv.ParseUint(strings.TrimSpace(n), 10, 64)
		if err != nil {
			return 0, failure("bad_request", fmt.Sprintf("window_focus: windowId %q is not a valid window id", n))
		}
		return id, nil
	}
	return 0, failure("bad_request", fmt.Sprintf("window_focus: windowId has unsupported type %T (send a number or numeric string)", v))
}
