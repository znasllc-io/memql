//go:build computeruse && linux

package tools

import (
	"fmt"
	"os"
	"runtime"

	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgbutil"
	"github.com/jezek/xgbutil/ewmh"
	"github.com/jezek/xgbutil/icccm"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// Linux window enumeration / activation rides EWMH over X11 via the
// jezek/xgb + jezek/xgbutil stack robotgo already vendors -- no new
// dependencies. Wayland has no portable equivalent (each compositor
// gates window enumeration behind its own portal), so a Wayland
// session gets the structured unsupported error below; the
// capability descriptor's displayServer field ("wayland") tells
// consumers up front.

// windowX11Conn opens a fresh X connection for one window call.
// Per-call connect keeps the handlers stateless (no stale-connection
// handling) -- window_list/window_focus are low-frequency, so the
// connect cost is irrelevant. Caller must Close() the returned
// connection.
func windowX11Conn(action string) (*xgbutil.XUtil, *memqlv1.Failure) {
	// detectDisplayServer is the same probe the capability
	// descriptor uses (capabilities.go) -- keep the runtime answer
	// consistent with what the worker advertises.
	switch detectDisplayServer(runtime.GOOS, os.Getenv) {
	case "wayland":
		return nil, failure("unsupported_on_platform",
			fmt.Sprintf("%s requires X11; this session is Wayland, which has no portable window enumeration/activation API", action))
	case "none":
		return nil, failure("unsupported_on_platform",
			fmt.Sprintf("%s requires X11 and no DISPLAY is set", action))
	}
	xu, err := xgbutil.NewConn()
	if err != nil {
		return nil, failure("x11_connect_failed",
			fmt.Sprintf("%s: connect to X server: %v", action, err))
	}
	return xu, nil
}

// listWindows enumerates the window manager's client list
// (_NET_CLIENT_LIST). Coordinates are root-window pixels -- the
// RobotGo logical input space on X11 (see WindowInfo).
func listWindows() ([]WindowInfo, *memqlv1.Failure) {
	xu, fail := windowX11Conn("window_list")
	if fail != nil {
		return nil, fail
	}
	defer xu.Conn().Close()

	clients, err := ewmh.ClientListGet(xu)
	if err != nil {
		return nil, failure("window_list_failed", "_NET_CLIENT_LIST: "+err.Error())
	}
	// Best-effort: a WM without _NET_ACTIVE_WINDOW support just
	// yields no Focused row.
	active, _ := ewmh.ActiveWindowGet(xu)

	wins := make([]WindowInfo, 0, len(clients))
	for _, win := range clients {
		info := WindowInfo{
			ID:      uint64(win),
			Focused: win != 0 && win == active,
		}
		// Title: prefer the EWMH UTF-8 name, fall back to the ICCCM
		// legacy WM_NAME. Both failing just leaves Title empty.
		if name, err := ewmh.WmNameGet(xu, win); err == nil && name != "" {
			info.Title = name
		} else if name, err := icccm.WmNameGet(xu, win); err == nil {
			info.Title = name
		}
		// PID via _NET_WM_PID; clients that don't set it report 0.
		if pid, err := ewmh.WmPidGet(xu, win); err == nil {
			info.PID = int(pid)
		}
		// App name from WM_CLASS's class field (e.g. "Firefox").
		if class, err := icccm.WmClassGet(xu, win); err == nil && class != nil {
			info.App = class.Class
		}
		// Geometry: size from the window itself, position translated
		// to root coordinates (a reparenting WM nests clients inside
		// frame windows, so the window's own x/y are frame-relative).
		if g, err := xproto.GetGeometry(xu.Conn(), xproto.Drawable(win)).Reply(); err == nil {
			info.Width = int(g.Width)
			info.Height = int(g.Height)
			if tr, err := xproto.TranslateCoordinates(xu.Conn(), win, xu.RootWin(), 0, 0).Reply(); err == nil {
				info.X = int(tr.DstX)
				info.Y = int(tr.DstY)
			}
		}
		wins = append(wins, info)
	}
	return wins, nil
}

// focusWindow asks the window manager to activate the window via an
// _NET_ACTIVE_WINDOW ClientMessage -- a true per-window raise+focus,
// honoring the WM's own focus-stealing policy.
func focusWindow(id uint64) (app, method string, fail *memqlv1.Failure) {
	xu, fail := windowX11Conn("window_focus")
	if fail != nil {
		return "", "", fail
	}
	defer xu.Conn().Close()

	clients, err := ewmh.ClientListGet(xu)
	if err != nil {
		return "", "", failure("window_focus_failed", "_NET_CLIENT_LIST: "+err.Error())
	}
	target := xproto.Window(id)
	found := false
	for _, w := range clients {
		if w == target {
			found = true
			break
		}
	}
	if !found {
		return "", "", failure("window_not_found",
			fmt.Sprintf("no managed window with id %d -- re-run window_list (ids change as windows open/close)", id))
	}
	if class, err := icccm.WmClassGet(xu, target); err == nil && class != nil {
		app = class.Class
	}
	if err := ewmh.ActiveWindowReq(xu, target); err != nil {
		return "", "", failure("window_focus_failed", "_NET_ACTIVE_WINDOW: "+err.Error())
	}
	return app, "ewmh_active_window", nil
}
