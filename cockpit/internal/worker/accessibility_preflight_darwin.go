//go:build darwin && computeruse

package worker

/*
#cgo LDFLAGS: -framework ApplicationServices

#include <ApplicationServices/ApplicationServices.h>
#include <stdbool.h>

// axProcessTrusted returns true when this process holds the
// Accessibility entry in TCC -- including the case where the grant
// is attributed to the responsible parent process (e.g. Terminal).
// The AX API is synchronous, never blocks, and never shows a prompt
// (AXIsProcessTrustedWithOptions with kAXTrustedCheckOptionPrompt
// would; we deliberately use the prompt-free variant for per-call
// checks). macOS 10.4+.
static bool axProcessTrusted(void) {
    return (bool)AXIsProcessTrusted();
}

static void requestAccessibility(void) {
    const void *key = kAXTrustedCheckOptionPrompt;
    const void *value = kCFBooleanTrue;
    CFDictionaryRef options = CFDictionaryCreate(NULL, &key, &value, 1,
        &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
    AXIsProcessTrustedWithOptions(options);
    CFRelease(options);
}
*/
import "C"

// preflightAccessibilityAccess wraps AXIsProcessTrusted. Returns
// true when this process is trusted for Accessibility (synthetic
// mouse + keyboard events), false otherwise. The call is synchronous
// and never blocks, so it is cheap enough to run before every input
// action -- mirroring preflightScreenCaptureAccess
// (tcc_preflight_darwin.go) for Screen Recording.
func preflightAccessibilityAccess() bool {
	return bool(C.axProcessTrusted())
}

func requestAccessibilityAccess() { C.requestAccessibility() }
