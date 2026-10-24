//go:build darwin && computeruse

package worker

import (
	"github.com/znasllc-io/memql/cockpit/internal/worker/tools"
)

// init wires the macOS TCC preflight checks into the tools package.
//
// Screen Recording fires before every screenshot dispatch so a
// permission that was approved at setup but later revoked from
// System Settings -> Privacy -> Screen Recording surfaces as a
// clean `permission_denied` failure rather than a confusing
// silent-or-empty screenshot. Accessibility fires before every
// input dispatch (mouse_* / key_*) for the same reason
// (memql-cockpit#164): a mid-session revocation surfaces as a
// structured failure with a remediation hint instead of a raw
// RobotGo error or panic.
//
// macOS-only, computeruse-only. On every other platform / build
// configuration the hooks stay nil and the dispatcher skips the
// checks.
func init() {
	tools.ScreenCapturePreflightHook = preflightScreenCaptureAccess
	tools.AccessibilityPreflightHook = preflightAccessibilityAccess
}
