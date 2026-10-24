//go:build computeruse && darwin

package tools

/*
#cgo LDFLAGS: -framework CoreGraphics -framework CoreFoundation

#include <CoreGraphics/CoreGraphics.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdint.h>
#include <string.h>

// cockpitWindow is the flat, cgo-friendly projection of one
// CGWindowList dictionary entry. Fixed-size string buffers keep the
// marshaling trivial; window titles / app names longer than 255
// bytes are clamped.
typedef struct {
	uint32_t wid;
	int32_t  pid;
	int32_t  layer;
	int32_t  x;
	int32_t  y;
	int32_t  width;
	int32_t  height;
	char     title[256];
	char     owner[256];
} cockpitWindow;

static void cockpitCopyCFString(CFDictionaryRef dict, CFStringRef key, char *buf, size_t buflen) {
	buf[0] = '\0';
	const void *v = CFDictionaryGetValue(dict, key);
	if (v == NULL || CFGetTypeID(v) != CFStringGetTypeID()) {
		return;
	}
	CFStringGetCString((CFStringRef)v, buf, (CFIndex)buflen, kCFStringEncodingUTF8);
}

static int32_t cockpitDictInt(CFDictionaryRef dict, CFStringRef key, int32_t def) {
	const void *v = CFDictionaryGetValue(dict, key);
	int32_t out = def;
	if (v == NULL || CFGetTypeID(v) != CFNumberGetTypeID()) {
		return def;
	}
	CFNumberGetValue((CFNumberRef)v, kCFNumberSInt32Type, &out);
	return out;
}

// cockpitListWindows fills out[0..max) from
// CGWindowListCopyWindowInfo, FRONT-TO-BACK z-order, on-screen
// windows only, desktop elements excluded. Returns the number of
// entries written, or -1 when the window server refused the query
// (no window-server session, e.g. an ssh login without Aqua).
//
// kCGWindowBounds is in CG global display coordinates -- POINTS, the
// same logical space RobotGo input uses -- so no DPI mapping is
// needed downstream.
static int cockpitListWindows(cockpitWindow *out, int max) {
	CFArrayRef arr = CGWindowListCopyWindowInfo(
		kCGWindowListOptionOnScreenOnly | kCGWindowListExcludeDesktopElements,
		kCGNullWindowID);
	if (arr == NULL) {
		return -1;
	}
	int n = 0;
	CFIndex count = CFArrayGetCount(arr);
	for (CFIndex i = 0; i < count && n < max; i++) {
		const void *entry = CFArrayGetValueAtIndex(arr, i);
		if (entry == NULL || CFGetTypeID(entry) != CFDictionaryGetTypeID()) {
			continue;
		}
		CFDictionaryRef d = (CFDictionaryRef)entry;
		cockpitWindow *w = &out[n];
		memset(w, 0, sizeof(*w));
		w->wid   = (uint32_t)cockpitDictInt(d, kCGWindowNumber, 0);
		w->pid   = cockpitDictInt(d, kCGWindowOwnerPID, 0);
		w->layer = cockpitDictInt(d, kCGWindowLayer, 0);
		const void *bv = CFDictionaryGetValue(d, kCGWindowBounds);
		if (bv != NULL && CFGetTypeID(bv) == CFDictionaryGetTypeID()) {
			CGRect r = CGRectZero;
			if (CGRectMakeWithDictionaryRepresentation((CFDictionaryRef)bv, &r)) {
				w->x      = (int32_t)r.origin.x;
				w->y      = (int32_t)r.origin.y;
				w->width  = (int32_t)r.size.width;
				w->height = (int32_t)r.size.height;
			}
		}
		cockpitCopyCFString(d, kCGWindowName, w->title, sizeof(w->title));
		cockpitCopyCFString(d, kCGWindowOwnerName, w->owner, sizeof(w->owner));
		n++;
	}
	CFRelease(arr);
	return n;
}
*/
import "C"

import (
	"fmt"

	"github.com/go-vgo/robotgo"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// maxDarwinWindows bounds the CGWindowList copy. 512 on-screen
// windows is far beyond any realistic desktop; anything past it is
// silently truncated rather than error.
const maxDarwinWindows = 512

// listWindows enumerates on-screen, layer-0 (normal) windows via
// CGWindowListCopyWindowInfo. Coordinates are CG global display
// POINTS -- the RobotGo logical input space (see WindowInfo).
//
// Title caveat: kCGWindowName for other apps' windows is only
// readable with the Screen Recording permission (macOS 10.15+).
// Without it Title comes back empty -- App (kCGWindowOwnerName) is
// always available, so rows stay identifiable.
//
// Focused detection: CGWindowList returns windows front-to-back, so
// the FIRST layer-0 row is the frontmost normal window -- that's the
// one marked Focused. This is an approximation (the true key window
// needs the AX API) but matches user perception for all common cases.
func listWindows() ([]WindowInfo, *memqlv1.Failure) {
	buf := make([]C.cockpitWindow, maxDarwinWindows)
	n := int(C.cockpitListWindows(&buf[0], C.int(len(buf))))
	if n < 0 {
		return nil, failure("window_list_failed",
			"CGWindowListCopyWindowInfo returned no window list -- is this process attached to a window-server session?")
	}
	wins := make([]WindowInfo, 0, n)
	focusedAssigned := false
	for i := 0; i < n; i++ {
		w := buf[i]
		// Layer 0 is the normal-window layer; everything else
		// (menu bar, Dock, overlays, status items) is chrome the
		// model shouldn't try to focus.
		if w.layer != 0 {
			continue
		}
		info := WindowInfo{
			ID:     uint64(w.wid),
			Title:  C.GoString(&w.title[0]),
			App:    C.GoString(&w.owner[0]),
			PID:    int(w.pid),
			X:      int(w.x),
			Y:      int(w.y),
			Width:  int(w.width),
			Height: int(w.height),
		}
		if !focusedAssigned {
			info.Focused = true
			focusedAssigned = true
		}
		wins = append(wins, info)
	}
	return wins, nil
}

// focusWindow resolves the CGWindowID to its owning pid and
// activates that APPLICATION (robotgo.ActivePid -> AX raise with a
// SetFrontProcess fallback). v1 semantics: the app's frontmost
// window comes forward; raising a specific non-front window within
// a multi-window app needs per-window AX targeting and is deferred.
// The result's `method` field says "app_activate" so callers can
// tell. Requires the Accessibility permission the worker already
// needs for input.
func focusWindow(id uint64) (app, method string, fail *memqlv1.Failure) {
	wins, fail := listWindows()
	if fail != nil {
		return "", "", fail
	}
	for _, w := range wins {
		if w.ID != id {
			continue
		}
		if w.PID <= 0 {
			return "", "", failure("window_focus_failed",
				fmt.Sprintf("window %d has no owning pid; cannot activate", id))
		}
		if err := robotgo.ActivePid(w.PID); err != nil {
			return "", "", failure("window_focus_failed",
				fmt.Sprintf("activate pid %d (%s): %v", w.PID, w.App, err))
		}
		return w.App, "app_activate", nil
	}
	return "", "", failure("window_not_found",
		fmt.Sprintf("no on-screen window with id %d -- re-run window_list (ids change as windows open/close)", id))
}
