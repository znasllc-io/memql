//go:build computeruse && !darwin && !linux

package tools

import (
	"fmt"
	"runtime"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// Window enumeration / activation ships for macOS (CGWindowList) and
// Linux/X11 (EWMH) only -- see window_computeruse_darwin.go /
// window_computeruse_linux.go. Other computeruse-tagged platforms get the structured
// unsupported error; the actions are still routed (and therefore
// advertised by the capability descriptor) so the error is honest
// and actionable rather than unknown_action.

func listWindows() ([]WindowInfo, *memqlv1.Failure) {
	return nil, failure("unsupported_on_platform",
		fmt.Sprintf("window_list is not implemented on %s (macOS + Linux/X11 only)", runtime.GOOS))
}

func focusWindow(uint64) (app, method string, fail *memqlv1.Failure) {
	return "", "", failure("unsupported_on_platform",
		fmt.Sprintf("window_focus is not implemented on %s (macOS + Linux/X11 only)", runtime.GOOS))
}
