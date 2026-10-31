//go:build computeruse && darwin

package tools

/*
#cgo LDFLAGS: -framework ApplicationServices -framework CoreFoundation -framework AppKit
#include <ApplicationServices/ApplicationServices.h>
#include <stdlib.h>
#include <math.h>

extern pid_t targetKeyboardPID(void);
extern long targetEventAppKitWindow(CGEventRef);
extern CGPoint targetEventWindowPoint(CGEventRef);
extern int targetWindowCoordinateAccessorsAvailable(void);
extern int targetInputActivity(uint32_t *, uint64_t *);
extern void targetMainLoopTurn(void);
extern CGEventRef targetCreateWindowMouse(CGEventSourceRef, CGEventType, CGPoint, CGMouseButton, uint32_t, CGRect);

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

static int targetAttribute(AXUIElementRef obj, CFStringRef name, CFTypeID type, CFTypeRef *out, AXError *error) {
    *out = NULL;
    *error=AXUIElementCopyAttributeValue(obj,name,out);
    if (*error != kAXErrorSuccess || !*out) return 0;
    if (CFGetTypeID(*out) != type) { CFRelease(*out); *out = NULL; *error=kAXErrorIllegalArgument; return 0; }
    return 1;
}

// Public AX identities establish actual focus. CG z-order is not focus.
// Match its geometry to exactly one on-screen CG window in the same process;
// ambiguity refuses input. No private AX window-number API or title heuristics.
typedef struct { int stage; AXError code; int matches; } targetReadError;
static targetFocus *targetReadFromPID(targetReadError *problem, pid_t directPID) {
    problem->stage=1; problem->code=kAXErrorSuccess; problem->matches=0;
    targetFocus *f = calloc(1, sizeof(targetFocus));
    if (!f) return NULL;
    problem->stage=2;
    int ok=0;
    if (directPID > 0) {
        f->app=AXUIElementCreateApplication(directPID);
        ok=f->app!=NULL;
    } else {
        AXUIElementRef sys=AXUIElementCreateSystemWide();
        if (!sys) { problem->stage=1; targetFree(f); return NULL; }
        ok=targetAttribute(sys,kAXFocusedApplicationAttribute,AXUIElementGetTypeID(),(CFTypeRef *)&f->app,&problem->code);
        CFRelease(sys);
    }
    if (!ok) { targetFree(f); return NULL; }
    problem->stage=3;
    problem->code=AXUIElementGetPid(f->app,&f->pid);
    if (problem->code!=kAXErrorSuccess || f->pid<=0) { targetFree(f); return NULL; }
    problem->stage=4;
    if (!targetAttribute(f->app,kAXFocusedWindowAttribute,AXUIElementGetTypeID(),(CFTypeRef *)&f->window,&problem->code)) {targetFree(f);return NULL;}
    problem->stage=5;
    if (!targetAttribute(f->app,kAXFocusedUIElementAttribute,AXUIElementGetTypeID(),(CFTypeRef *)&f->element,&problem->code)) {targetFree(f);return NULL;}
    AXValueRef pos=NULL,size=NULL;
    problem->stage=6;
    ok=targetAttribute(f->window,kAXPositionAttribute,AXValueGetTypeID(),(CFTypeRef *)&pos,&problem->code);
    if (ok) {
        problem->stage=7;
        ok=targetAttribute(f->window,kAXSizeAttribute,AXValueGetTypeID(),(CFTypeRef *)&size,&problem->code);
    }
    CGPoint p = CGPointZero; CGSize s = CGSizeZero;
    if (ok) { problem->stage=8; ok = AXValueGetValue(pos, kAXValueCGPointType, &p) && AXValueGetValue(size, kAXValueCGSizeType, &s); }
    if (pos) CFRelease(pos); if (size) CFRelease(size);
    if (!ok || s.width <= 0 || s.height <= 0) { targetFree(f); return NULL; }
    f->bounds = (CGRect){p,s};
    problem->stage=9;
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
    problem->stage=10; problem->matches=matches;
    if (matches != 1 || !f->wid) { targetFree(f); return NULL; }
    problem->stage=0;
    return f;
}

static targetFocus *targetRead(targetReadError *problem) { return targetReadFromPID(problem,0); }

// This alternate read is diagnostic only. It never creates an input receipt.
// NSWorkspace defines frontmostApplication as the receiver of key events.
// Compare that PID after the exact AX window/element/CG-geometry read. AppKit
// can cache application properties between main-run-loop turns, so this is
// explicitly not proof of fresh focus and cannot authorize input.
static targetFocus *targetReadDiagnostic(targetReadError *problem, pid_t *pid) {
    *pid=targetKeyboardPID();
    if (*pid<=0) { problem->stage=11; problem->code=kAXErrorNoValue; problem->matches=0; return NULL; }
    targetFocus *f=targetReadFromPID(problem,*pid);
    if (f && targetKeyboardPID()!=*pid) {
        targetFree(f); problem->stage=12; problem->code=kAXErrorCannotComplete; return NULL;
    }
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
typedef struct { CGEventRef event; pid_t pid; int sourceState; } targetEvent;
static void targetEventFree(targetEvent *e) { if (!e) return; if (e->event) CFRelease(e->event); free(e); }
static targetEvent *targetCreate(pid_t pid, uint32_t wid, int kind, int key, const UniChar *text, int ntext,
                      int down, int button, int x, int y, int dx, int dy, uint64_t flags, CGRect bounds) {
    if (pid <= 0 || wid == 0) return NULL;
    CGEventSourceRef source = CGEventSourceCreate(kCGEventSourceStatePrivate);
    if (!source) return 0;
    int sourceState=(int)CGEventSourceGetSourceStateID(source);
    CGEventRef event = NULL;
    if (kind == 0 || kind == 1) {
        event = CGEventCreateKeyboardEvent(source, (CGKeyCode)key, down);
        if (event && kind == 0) CGEventKeyboardSetUnicodeString(event,ntext,text);
    } else if (kind == 2 || kind == 3) {
        CGEventType type = kCGEventMouseMoved;
        if (kind == 3) type = button == 0 ? (down ? kCGEventLeftMouseDown : kCGEventLeftMouseUp) :
            button == 1 ? (down ? kCGEventRightMouseDown : kCGEventRightMouseUp) : (down ? kCGEventOtherMouseDown : kCGEventOtherMouseUp);
        event = targetCreateWindowMouse(source,type,CGPointMake(x,y),(CGMouseButton)button,wid,bounds);
        if (event && kind == 3) CGEventSetIntegerValueField(event,kCGMouseEventClickState,1);
    } else if (kind == 4) {
        event = CGEventCreateScrollWheelEvent(source,kCGScrollEventUnitLine,2,dy,dx);
        if (event) CGEventSetLocation(event,CGPointMake(x,y));
    }
    CFRelease(source);
    if (!event) return NULL;
    CGEventSetFlags(event,(CGEventFlags)flags);
    CGEventSetIntegerValueField(event,kCGEventTargetUnixProcessID,pid);
    if (kind == 2 || kind == 3) {
        // PID posting bypasses normal global hit-testing. Carry the observed
        // window on mouse events; Quartz ignores these fields on scroll events.
        // These are routing hints, not an AX lock or
        // proof that the application processed the event.
        CGEventSetIntegerValueField(event,kCGMouseEventWindowUnderMousePointer,wid);
        CGEventSetIntegerValueField(event,kCGMouseEventWindowUnderMousePointerThatCanHandleThisEvent,wid);
    }
    targetEvent *out = calloc(1,sizeof(targetEvent));
    if (!out) { CFRelease(event); return NULL; }
    out->event=event; out->pid=pid; out->sourceState=sourceState; return out;
}
static void targetSend(targetEvent *e) { CGEventPostToPid(e->pid,e->event); }
static int64_t targetEventField(targetEvent *e, int field) { return CGEventGetIntegerValueField(e->event,(CGEventField)field); }
static int targetEventSourceState(targetEvent *e) {
    CGEventSourceRef source=CGEventCreateSourceFromEvent(e->event);
    if (!source) return -999;
    int state=(int)CGEventSourceGetSourceStateID(source);
    CFRelease(source); return state;
}

*/
import "C"

import (
	"context"
	"fmt"
	"time"
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

func (darwinInputBackend) inputActivity() (inputActivity, bool) {
	var counters [16]C.uint32_t
	var generation C.uint64_t
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	available := false
	err := desktopMainJobs.call(ctx, func() error {
		C.targetMainLoopTurn()
		available = C.targetInputActivity(&counters[0], &generation) != 0
		return nil
	})
	if err != nil || !available {
		return inputActivity{}, false
	}
	value := inputActivity{activation: uint64(generation)}
	for i := range counters {
		value.counters[i] = uint32(counters[i])
	}
	return value, true
}

func (darwinInputBackend) canPostEvents() bool { return bool(C.CGPreflightPostEventAccess()) }
func (darwinInputBackend) pointerCoordinateAccessors() bool {
	return C.targetWindowCoordinateAccessorsAvailable() != 0
}

func (darwinInputBackend) focus() (inputFocus, error) {
	var problem C.targetReadError
	p := C.targetRead(&problem)
	if p == nil {
		return nil, targetReadFailure(problem)
	}
	return &darwinInputFocus{ptr: p}, nil
}

func targetReadFailure(problem C.targetReadError) error {
	return darwinFocusReadError{problem: problem}
}

type darwinFocusReadError struct{ problem C.targetReadError }

func (e darwinFocusReadError) inputFocusDiagnostic() InputFocusDiagnostic {
	stages := map[int]string{1: "system AX allocation", 2: "focused application", 3: "focused application PID", 4: "focused window", 5: "focused UI element", 6: "window position", 7: "window size", 8: "window geometry", 9: "on-screen CG window list", 10: "unique AX-to-CG window match", 11: "NSWorkspace keyboard recipient", 12: "NSWorkspace keyboard recipient changed"}
	return InputFocusDiagnostic{Stage: stages[int(e.problem.stage)], ErrorCode: int(e.problem.code), MatchingWindows: int(e.problem.matches)}
}

func (e darwinFocusReadError) Error() string {
	d := e.inputFocusDiagnostic()
	return fmt.Sprintf("Cannot establish exact target at %s (AX error %d, matching windows %d); input refused. Do not retry input or activate a different app.", d.Stage, d.ErrorCode, d.MatchingWindows)
}

func (darwinInputBackend) diagnosticProbes() []InputTargetProbe {
	var problem C.targetReadError
	var pid C.pid_t
	started := time.Now()
	p := C.targetReadDiagnostic(&problem, &pid)
	probe := InputTargetProbe{Method: "NSWorkspace snapshot plus direct AX application (diagnostic only; snapshot may be cached)", PID: int(pid), ElapsedMilliseconds: time.Since(started).Milliseconds()}
	probe.MainLoopRunning, probe.MainLoopTurns = DesktopMainLoopProgress()
	if p == nil {
		probe.Error = targetReadFailure(problem).Error()
	} else {
		probe.Available, probe.WindowID = true, uint64(p.wid)
		C.targetFree(p)
	}
	return []InputTargetProbe{probe}
}
func (darwinInputBackend) emit(target inputFocus, e targetInputEvent) error {
	if !(darwinInputBackend{}).canPostEvents() {
		return fmt.Errorf("macOS event-posting access is unavailable; no event was posted (CGPreflightPostEventAccess=false)")
	}
	f, ok := target.(*darwinInputFocus)
	if !ok || f.ptr == nil {
		return fmt.Errorf("invalid target identity")
	}
	event, err := newDarwinTargetEvent(f.window(), e)
	if err != nil {
		return err
	}
	defer event.close()
	C.targetSend(event.ptr)
	return nil
}

// Event construction is separate from posting so platform packet fields can
// be tested without touching the desktop or needing an active application.
type darwinTargetEvent struct{ ptr *C.targetEvent }

func (e *darwinTargetEvent) close() { C.targetEventFree(e.ptr); e.ptr = nil }
func (e *darwinTargetEvent) appKitWindow() int64 {
	return int64(C.targetEventAppKitWindow(e.ptr.event))
}
func (e *darwinTargetEvent) windowPoint() (float64, float64) {
	p := C.targetEventWindowPoint(e.ptr.event)
	return float64(p.x), float64(p.y)
}
func (e *darwinTargetEvent) sourceState() int         { return int(C.targetEventSourceState(e.ptr)) }
func (e *darwinTargetEvent) originalSourceState() int { return int(e.ptr.sourceState) }

func (e *darwinTargetEvent) pointerDescription() (kind int, button int64, x, y float64, flags uint64) {
	p := C.CGEventGetLocation(e.ptr.event)
	return int(C.CGEventGetType(e.ptr.event)), int64(C.CGEventGetIntegerValueField(e.ptr.event, C.kCGMouseEventButtonNumber)), float64(p.x), float64(p.y), uint64(C.CGEventGetFlags(e.ptr.event))
}
func (e *darwinTargetEvent) routing() (pid, window, handlingWindow int64) {
	return int64(C.targetEventField(e.ptr, C.kCGEventTargetUnixProcessID)), int64(C.targetEventField(e.ptr, C.kCGMouseEventWindowUnderMousePointer)), int64(C.targetEventField(e.ptr, C.kCGMouseEventWindowUnderMousePointerThatCanHandleThisEvent))
}
func newDarwinTargetEvent(w WindowInfo, e targetInputEvent) (*darwinTargetEvent, error) {
	kind, key, button := C.int(-1), C.int(0), C.int(0)
	var chars []uint16
	switch e.kind {
	case "text":
		kind = 0
		chars = utf16.Encode([]rune(e.text))
	case "key":
		code, valid := targetKeyCode(e.key)
		if !valid {
			return nil, fmt.Errorf("unsupported key")
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
		return nil, fmt.Errorf("unsupported event")
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
	bounds := C.CGRectMake(C.double(w.X), C.double(w.Y), C.double(w.Width), C.double(w.Height))
	event := C.targetCreate(C.pid_t(w.PID), C.uint32_t(w.ID), kind, key, ptr, C.int(len(chars)), down, button, C.int(e.x), C.int(e.y), C.int(e.dx), C.int(e.dy), flags, bounds)
	if event == nil {
		return nil, fmt.Errorf("target event creation failed")
	}
	return &darwinTargetEvent{ptr: event}, nil
}

func init() { desktopInputGuard.backend = darwinInputBackend{} }
