//go:build darwin && computeruse

package worker

/*
#cgo LDFLAGS: -framework CoreGraphics

#include <CoreGraphics/CoreGraphics.h>
#include <stdbool.h>

// preflightScreenCapture returns true when this process has a
// granted Screen Recording entry in TCC -- including the case
// where the grant was inherited from the parent (e.g. Terminal).
// The CG API is synchronous and never blocks; it just reads the
// process's TCC state. macOS 10.15+.
static bool preflightScreenCapture(void) {
    return CGPreflightScreenCaptureAccess();
}

// requestScreenCapture asks macOS to put the binary on the prompt
// queue. Returns true when access is already granted (no prompt
// shown), false otherwise. Use this AFTER preflight returns false
// to surface the system prompt; the user can then approve via the
// dialog.
static bool requestScreenCapture(void) {
    return CGRequestScreenCaptureAccess();
}
*/
import "C"

// preflightScreenCaptureAccess wraps CGPreflightScreenCaptureAccess.
// Returns true when this process is approved (or has inherited an
// approval from its parent), false otherwise. The call is synchronous
// and never blocks.
func preflightScreenCaptureAccess() bool {
	return bool(C.preflightScreenCapture())
}

// requestScreenCaptureAccess wraps CGRequestScreenCaptureAccess.
// Returns true when access is already granted; otherwise queues the
// system prompt and returns false. The first call after the binary
// gains a fresh hash typically returns false and triggers the
// "[binary] would like to record this computer's screen" dialog.
func requestScreenCaptureAccess() bool {
	return bool(C.requestScreenCapture())
}
