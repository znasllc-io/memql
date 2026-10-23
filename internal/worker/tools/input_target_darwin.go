//go:build computeruse && darwin

package tools

/*
#cgo LDFLAGS: -framework ApplicationServices -framework CoreFoundation
#include <ApplicationServices/ApplicationServices.h>
#include <stdlib.h>
#include <math.h>

typedef struct {
    AXUIElementRef app, window, element;
    pid_t pid;
    uint32_t wid;
    CGRect bounds;
} targetFocus;

static void targetFree(targetFocus *f) {
    if (!f) return;
    if (f->app) CFRelease(f->app);
    if (f->window) CFRelease(f->window);
    if (f->element) CFRelease(f->element);
    free(f);
}

static int targetAttribute(AXUIElementRef obj, CFStringRef name, CFTypeID type, CFTypeRef *out) {
    *out = NULL;
    if (AXUIElementCopyAttributeValue(obj, name, out) != kAXErrorSuccess || !*out) return 0;
    if (CFGetTypeID(*out) != type) { CFRelease(*out); *out = NULL; return 0; }
    return 1;
}

// Public AX identities establish actual focus. CG z-order is not focus.
// Match its geometry to exactly one on-screen CG window in the same process;
// ambiguity refuses input. No private AX window-number API or title heuristics.
static targetFocus *targetRead(void) {
    targetFocus *f = calloc(1, sizeof(targetFocus));
    if (!f) return NULL;
    AXUIElementRef sys = AXUIElementCreateSystemWide();
    if (!sys) { targetFree(f); return NULL; }
    int ok = targetAttribute(sys, kAXFocusedApplicationAttribute, AXUIElementGetTypeID(), (CFTypeRef *)&f->app);
    CFRelease(sys);
    if (!ok || AXUIElementGetPid(f->app, &f->pid) != kAXErrorSuccess || f->pid <= 0 ||
        !targetAttribute(f->app, kAXFocusedWindowAttribute, AXUIElementGetTypeID(), (CFTypeRef *)&f->window) ||
        !targetAttribute(f->app, kAXFocusedUIElementAttribute, AXUIElementGetTypeID(), (CFTypeRef *)&f->element)) {
        targetFree(f); return NULL;
    }
    AXValueRef pos = NULL, size = NULL;
    ok = targetAttribute(f->window, kAXPositionAttribute, AXValueGetTypeID(), (CFTypeRef *)&pos) &&
         targetAttribute(f->window, kAXSizeAttribute, AXValueGetTypeID(), (CFTypeRef *)&size);
    CGPoint p = CGPointZero; CGSize s = CGSizeZero;
    if (ok) ok = AXValueGetValue(pos, kAXValueCGPointType, &p) && AXValueGetValue(size, kAXValueCGSizeType, &s);
    if (pos) CFRelease(pos); if (size) CFRelease(size);
    if (!ok || s.width <= 0 || s.height <= 0) { targetFree(f); return NULL; }
    f->bounds = (CGRect){p,s};
    CFArrayRef list = CGWindowListCopyWindowInfo(kCGWindowListOptionOnScreenOnly | kCGWindowListExcludeDesktopElements, kCGNullWindowID);
    if (!list) { targetFree(f); return NULL; }
    int matches = 0;
    for (CFIndex i = 0; i < CFArrayGetCount(list); i++) {
        CFDictionaryRef d = CFArrayGetValueAtIndex(list, i);
        CFNumberRef pid = CFDictionaryGetValue(d, kCGWindowOwnerPID), wid = CFDictionaryGetValue(d, kCGWindowNumber), layer = CFDictionaryGetValue(d, kCGWindowLayer);
        CFDictionaryRef bounds = CFDictionaryGetValue(d, kCGWindowBounds);
        int32_t owner = 0, id = 0, level = -1; CGRect r;
        if (!pid || !wid || !layer || !bounds || !CFNumberGetValue(pid,kCFNumberSInt32Type,&owner) ||
            !CFNumberGetValue(wid,kCFNumberSInt32Type,&id) || !CFNumberGetValue(layer,kCFNumberSInt32Type,&level) ||
            !CGRectMakeWithDictionaryRepresentation(bounds,&r)) continue;
        if (owner == f->pid && level == 0 && CGRectEqualToRect(r,f->bounds)) { f->wid = (uint32_t)id; matches++; }
    }
    CFRelease(list);
    if (matches != 1 || !f->wid) { targetFree(f); return NULL; }
    return f;
}

static int targetSame(targetFocus *a, targetFocus *b, int field) {
    return a && b && a->pid == b->pid && a->wid == b->wid && CGRectEqualToRect(a->bounds,b->bounds) &&
        CFEqual(a->app,b->app) && CFEqual(a->window,b->window) && (!field || CFEqual(a->element,b->element));
}

// All events use a private source (no inherited hardware modifiers) and are
// addressed to the retained PID. There is deliberately no global-post fallback.
// Posting has no delivery acknowledgement and cannot atomically bind an event
// to an AX field: the Go guard samples those identities before every event.
static int targetPost(pid_t pid, int kind, int key, const UniChar *text, int ntext,
                      int down, int button, int x, int y, int dx, int dy, uint64_t flags) {
    if (pid <= 0) return 0;
    CGEventSourceRef source = CGEventSourceCreate(kCGEventSourceStatePrivate);
    if (!source) return 0;
    CGEventRef event = NULL;
    if (kind == 0 || kind == 1) {
        event = CGEventCreateKeyboardEvent(source, (CGKeyCode)key, down);
        if (event && kind == 0) CGEventKeyboardSetUnicodeString(event,ntext,text);
    } else if (kind == 2 || kind == 3) {
        CGEventType type = kCGEventMouseMoved;
        if (kind == 3) type = button == 0 ? (down ? kCGEventLeftMouseDown : kCGEventLeftMouseUp) :
            button == 1 ? (down ? kCGEventRightMouseDown : kCGEventRightMouseUp) : (down ? kCGEventOtherMouseDown : kCGEventOtherMouseUp);
        event = CGEventCreateMouseEvent(source,type,CGPointMake(x,y),(CGMouseButton)button);
        if (event && kind == 3) CGEventSetIntegerValueField(event,kCGMouseEventClickState,1);
    } else if (kind == 4) {
        event = CGEventCreateScrollWheelEvent(source,kCGScrollEventUnitLine,2,dy,dx);
        if (event) CGEventSetLocation(event,CGPointMake(x,y));
    }
    if (event) { CGEventSetFlags(event,(CGEventFlags)flags); CGEventPostToPid(pid,event); CFRelease(event); }
    CFRelease(source);
    return event != NULL;
}
*/
import "C"

import (
	"fmt"
	"unicode/utf16"
)

type darwinInputFocus struct{ ptr *C.targetFocus }

func (f *darwinInputFocus) window() WindowInfo {
	p := f.ptr
	return WindowInfo{ID: uint64(p.wid), PID: int(p.pid), X: int(p.bounds.origin.x), Y: int(p.bounds.origin.y), Width: int(p.bounds.size.width), Height: int(p.bounds.size.height), Focused: true}
}
func (f *darwinInputFocus) same(other inputFocus, element bool) bool {
	o, ok := other.(*darwinInputFocus)
	if !ok {
		return false
	}
	field := C.int(0)
	if element {
		field = 1
	}
	return C.targetSame(f.ptr, o.ptr, field) != 0
}
func (f *darwinInputFocus) close() { C.targetFree(f.ptr); f.ptr = nil }

type darwinInputBackend struct{}

func (darwinInputBackend) focus() (inputFocus, error) {
	p := C.targetRead()
	if p == nil {
		return nil, fmt.Errorf("Cannot establish exact AX application, window and focused element; input refused")
	}
	return &darwinInputFocus{ptr: p}, nil
}
func (darwinInputBackend) emit(target inputFocus, e targetInputEvent) error {
	f, ok := target.(*darwinInputFocus)
	if !ok || f.ptr == nil {
		return fmt.Errorf("invalid target identity")
	}
	kind, key, button := C.int(-1), C.int(0), C.int(0)
	var chars []uint16
	switch e.kind {
	case "text":
		kind = 0
		chars = utf16.Encode([]rune(e.text))
	case "key":
		code, valid := targetKeyCode(e.key)
		if !valid {
			return fmt.Errorf("unsupported key")
		}
		kind, key = 1, C.int(code)
	case "move":
		kind = 2
	case "mouse":
		kind = 3
		if e.button == "right" {
			button = 1
		}
		if e.button == "middle" {
			button = 2
		}
	case "scroll":
		kind = 4
	default:
		return fmt.Errorf("unsupported event")
	}
	flags := C.uint64_t(0)
	for _, mod := range e.modifiers {
		switch mod {
		case "cmd":
			flags |= C.kCGEventFlagMaskCommand
		case "ctrl":
			flags |= C.kCGEventFlagMaskControl
		case "alt":
			flags |= C.kCGEventFlagMaskAlternate
		case "shift":
			flags |= C.kCGEventFlagMaskShift
		}
	}
	down := C.int(0)
	if e.down {
		down = 1
	}
	// An up event does not carry held modifiers into the original process.
	if e.release {
		flags = 0
	}
	var ptr *C.UniChar
	if len(chars) > 0 {
		ptr = (*C.UniChar)(&chars[0])
	}
	if C.targetPost(f.ptr.pid, kind, key, ptr, C.int(len(chars)), down, button, C.int(e.x), C.int(e.y), C.int(e.dx), C.int(e.dy), flags) == 0 {
		return fmt.Errorf("target event creation failed")
	}
	return nil
}

func init() { desktopInputGuard.backend = darwinInputBackend{} }
