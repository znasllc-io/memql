//go:build computeruse && !darwin

package worker

// preflightScreenCaptureAccess, requestScreenCaptureAccess, and
// preflightAccessibilityAccess are macOS-only -- there's no
// equivalent system-level TCC ledger on Linux or Windows. The
// non-darwin computeruse builds always report "true" for all three so the
// wizard treats the TCC steps as granted-by-default; on Linux the
// meaningful preflight is the display-server check
// (display_preflight_linux.go), and any remaining failures surface
// as runtime errors rather than pre-flight denials.
func preflightScreenCaptureAccess() bool { return true }
func requestScreenCaptureAccess() bool   { return true }
func preflightAccessibilityAccess() bool { return true }
