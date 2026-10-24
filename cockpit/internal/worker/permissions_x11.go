package worker

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

// Session presence is not an X11 grant. A configured X11 display must also
// accept a passive authenticated query; Wayland (including XWayland) remains
// unsupported by this worker's computer-use backend.
func measureLinuxPermissions(displayServer, display string, probe func(string) (permissionDecision, string)) permissionSnapshot {
	snapshot := permissionSnapshot{detail: "Accessibility and Screen Recording TCC checks are not applicable on Linux. "}
	switch displayServer {
	case "wayland":
		snapshot.detail += "Wayland computer use is unsupported; X11 permission is unknown."
	case "none":
		snapshot.x11Display = permissionDenied
		snapshot.detail += "No X11 display is configured for this worker process."
	case "x11":
		var detail string
		snapshot.x11Display, detail = probe(display)
		snapshot.detail += detail
	default:
		snapshot.detail += "Display server is unknown; X11 access was not measured."
	}
	return snapshot
}

// xdpyinfo queries server metadata only: no input events, screenshots or
// permission changes. A bounded subprocess avoids letting an unresponsive
// X server block the worker's heartbeat. Missing tooling/timeout is unknown.
func probeX11Access(display string) (permissionDecision, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := exec.CommandContext(ctx, "xdpyinfo", "-display", display).Run()
	if err == nil {
		return permissionGranted, "Passive X11 connection succeeded for this worker's display and credentials."
	}
	if errors.Is(err, exec.ErrNotFound) {
		return permissionUnknown, "X11 display is configured, but xdpyinfo is unavailable; access is unknown."
	}
	if ctx.Err() != nil {
		return permissionUnknown, "Passive X11 connection timed out; access is unknown."
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return permissionDenied, "Passive X11 connection failed; check DISPLAY, XAUTHORITY and the worker's session access."
	}
	return permissionUnknown, "Passive X11 probe could not run; access is unknown."
}
