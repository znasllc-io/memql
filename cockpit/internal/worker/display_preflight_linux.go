//go:build linux && computeruse

package worker

import (
	"github.com/znasllc-io/memql/cockpit/internal/worker/tools"
)

// init wires the Linux display-server preflight into the tools
// package -- the non-darwin counterpart of the macOS TCC hooks in
// tcc_hook_darwin.go, so every platform's per-call gating flows
// through the same dispatcher chokepoint (memql-cockpit#164).
//
// RobotGo drives X11 only. The hook reuses the capability
// descriptor's detection (tools.DetectDisplayServer, memql-cockpit#162)
// so the dispatcher's preflight verdict can never drift from what
// `workerComputer.capabilities` advertises: on a Wayland session or
// with no display at all, input + screenshot dispatches fail with a
// structured `display_server_unsupported` instead of an opaque
// RobotGo error.
//
// Linux-only, computeruse-only. On every other platform / build
// configuration the hook stays nil and the dispatcher skips the
// check.
func init() {
	tools.DisplayServerPreflightHook = tools.DetectDisplayServer
}
