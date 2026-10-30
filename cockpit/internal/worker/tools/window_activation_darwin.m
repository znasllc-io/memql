//go:build computeruse && darwin

#import <AppKit/AppKit.h>
#import <ApplicationServices/ApplicationServices.h>
#include <stdatomic.h>

static id activationObserver;
static atomic_ulong activationGeneration;
static atomic_bool activationWatching;
static pid_t lastActivatedPID;
extern void targetMainLoopTurn(void);

void targetActivationWatchStart(void) {
    activationObserver = [[[NSWorkspace sharedWorkspace] notificationCenter]
        addObserverForName:NSWorkspaceDidActivateApplicationNotification object:nil queue:[NSOperationQueue mainQueue]
        usingBlock:^(NSNotification *note) {
            activationGeneration++;
            lastActivatedPID = [note.userInfo[NSWorkspaceApplicationKey] processIdentifier];
        }];
    [activationObserver retain];
    atomic_store(&activationWatching, activationObserver != nil);
}
void targetActivationWatchStop(void) {
    atomic_store(&activationWatching, false);
    if (activationObserver) {
        [[[NSWorkspace sharedWorkspace] notificationCenter] removeObserver:activationObserver];
        [activationObserver release]; activationObserver = nil;
    }
}

// Only counters are retained, never keys or input contents. Count each public
// HID event type: the counter API does not document kCGAnyInputEventType.
static const CGEventType activityTypes[] = {
    kCGEventLeftMouseDown, kCGEventLeftMouseUp, kCGEventRightMouseDown, kCGEventRightMouseUp,
    kCGEventMouseMoved, kCGEventLeftMouseDragged, kCGEventRightMouseDragged,
    kCGEventKeyDown, kCGEventKeyUp, kCGEventFlagsChanged, kCGEventScrollWheel,
    kCGEventTabletPointer, kCGEventTabletProximity, kCGEventOtherMouseDown,
    kCGEventOtherMouseUp, kCGEventOtherMouseDragged
};
#define ACTIVITY_COUNT (sizeof(activityTypes)/sizeof(activityTypes[0]))

typedef struct {
    NSRunningApplication *source, *target;
    AXUIElementRef app, window;
    CGRect bounds;
    pid_t pid;
    uint32_t wid, activity[ACTIVITY_COUNT];
    unsigned long generation;
    atomic_bool cancelled;
} targetActivation;

void targetActivationCancel(void *ptr) { atomic_store(&((targetActivation *)ptr)->cancelled,true); }

void targetActivationFree(void *ptr) {
    targetActivation *a = ptr;
    if (!a) return;
    [a->source release]; [a->target release];
    if (a->app) CFRelease(a->app);
    if (a->window) CFRelease(a->window);
    free(a);
}

static int activationActivitySame(targetActivation *a) {
    for (size_t i=0; i<ACTIVITY_COUNT; i++)
        if (a->activity[i] != CGEventSourceCounterForEventType(kCGEventSourceStateHIDSystemState, activityTypes[i])) return 0;
    return 1;
}

static int activationInputHeld(void) {
    for (CGKeyCode key=0; key<128; key++)
        if (CGEventSourceKeyState(kCGEventSourceStateHIDSystemState,key)) return 1;
    for (CGMouseButton button=0; button<32; button++)
        if (CGEventSourceButtonState(kCGEventSourceStateHIDSystemState,button)) return 1;
    return 0;
}

// Called from the worker transaction thread. No AppKit object is read here;
// the main-loop observer publishes only a monotonic activation generation.
// HID counters deliberately exclude the worker's private event source.
int targetInputActivity(uint32_t *counters, uint64_t *generation) {
    if (!atomic_load(&activationWatching) || activationInputHeld()) return 0;
    unsigned long before=atomic_load(&activationGeneration);
    for (size_t i=0; i<ACTIVITY_COUNT; i++)
        counters[i]=CGEventSourceCounterForEventType(kCGEventSourceStateHIDSystemState,activityTypes[i]);
    *generation=before;
    return atomic_load(&activationWatching) && !activationInputHeld() && before==atomic_load(&activationGeneration);
}

static int activationStagesUndisturbed(targetActivation *a) {
    NSRunningApplication *current=[[NSWorkspace sharedWorkspace] frontmostApplication];
    return !atomic_load(&a->cancelled) && !activationInputHeld() && activationActivitySame(a) &&
        activationGeneration<=a->generation+1 &&
        (activationGeneration==a->generation || lastActivatedPID==a->pid) &&
        ([current isEqual:a->source] || [current isEqual:a->target]);
}

static int activationBounds(AXUIElementRef w, CGRect *bounds) {
    CFTypeRef p=NULL, s=NULL;
    int ok = AXUIElementCopyAttributeValue(w,kAXPositionAttribute,&p)==kAXErrorSuccess &&
        p && CFGetTypeID(p)==AXValueGetTypeID() &&
        AXUIElementCopyAttributeValue(w,kAXSizeAttribute,&s)==kAXErrorSuccess &&
        s && CFGetTypeID(s)==AXValueGetTypeID() &&
        AXValueGetValue(p,kAXValueCGPointType,&bounds->origin) &&
        AXValueGetValue(s,kAXValueCGSizeType,&bounds->size);
    if (p) CFRelease(p); if (s) CFRelease(s);
    return ok && bounds->size.width>0 && bounds->size.height>0;
}

// Resolve by PID and exact geometry only if BOTH the CG list and the app's AX
// window list contain exactly one match. Retained AX identity prevents a
// replacement window with reused ID/geometry from being substituted later.
static AXUIElementRef activationResolve(targetActivation *a) {
    CFArrayRef cg=CGWindowListCopyWindowInfo(kCGWindowListOptionOnScreenOnly|kCGWindowListExcludeDesktopElements,kCGNullWindowID);
    if (!cg) return NULL;
    int matches=0; uint32_t matchedID=0;
    for (CFIndex i=0; i<CFArrayGetCount(cg); i++) {
        CFDictionaryRef d=CFArrayGetValueAtIndex(cg,i);
        CFNumberRef pn=CFDictionaryGetValue(d,kCGWindowOwnerPID), wn=CFDictionaryGetValue(d,kCGWindowNumber), ln=CFDictionaryGetValue(d,kCGWindowLayer);
        CFDictionaryRef bn=CFDictionaryGetValue(d,kCGWindowBounds);
        int32_t pid=0,wid=0,layer=-1; CGRect b;
        if (pn && wn && ln && bn && CFNumberGetValue(pn,kCFNumberSInt32Type,&pid) &&
            CFNumberGetValue(wn,kCFNumberSInt32Type,&wid) && CFNumberGetValue(ln,kCFNumberSInt32Type,&layer) &&
            CGRectMakeWithDictionaryRepresentation(bn,&b) && pid==a->pid && layer==0 && CGRectEqualToRect(b,a->bounds)) {
            matches++; matchedID=(uint32_t)wid;
        }
    }
    CFRelease(cg);
    if (matches!=1 || matchedID!=a->wid) return NULL;
    CFTypeRef windows=NULL;
    if (AXUIElementCopyAttributeValue(a->app,kAXWindowsAttribute,&windows)!=kAXErrorSuccess || !windows) return NULL;
    if (CFGetTypeID(windows)!=CFArrayGetTypeID()) { CFRelease(windows); return NULL; }
    AXUIElementRef found=NULL; matches=0;
    for (CFIndex i=0; i<CFArrayGetCount(windows); i++) {
        AXUIElementRef w=(AXUIElementRef)CFArrayGetValueAtIndex(windows,i); CGRect b;
        if (CFGetTypeID(w)==AXUIElementGetTypeID() && activationBounds(w,&b) && CGRectEqualToRect(b,a->bounds)) { found=w; matches++; }
    }
    if (matches==1) CFRetain(found); else found=NULL;
    CFRelease(windows);
    return found;
}

void *targetActivationObserve(pid_t pid, uint32_t wid, int x, int y, int width, int height) {
    if (![NSThread isMainThread] || !activationObserver || pid<=0 || !wid || width<=0 || height<=0 || activationInputHeld()) return NULL;
    @autoreleasepool {
        targetActivation *a=calloc(1,sizeof(targetActivation));
        if (!a) return NULL;
        atomic_init(&a->cancelled,false);
        a->pid=pid; a->wid=wid; a->bounds=CGRectMake(x,y,width,height);
        a->generation=activationGeneration;
        for (size_t i=0; i<ACTIVITY_COUNT; i++) a->activity[i]=CGEventSourceCounterForEventType(kCGEventSourceStateHIDSystemState,activityTypes[i]);
        a->source=[[[NSWorkspace sharedWorkspace] frontmostApplication] retain];
        a->target=[[NSRunningApplication runningApplicationWithProcessIdentifier:pid] retain];
        if (!a->source || !a->target || a->target.terminated || a->source.terminated) { targetActivationFree(a); return NULL; }
        a->app=AXUIElementCreateApplication(pid);
        if (!a->app) { targetActivationFree(a); return NULL; }
        AXUIElementSetMessagingTimeout(a->app,0.5);
        a->window=activationResolve(a);
        if (!a->window || activationInputHeld() || !activationActivitySame(a) || a->generation!=activationGeneration) { targetActivationFree(a); return NULL; }
        return a;
    }
}

// No keyboard/mouse events, RobotGo, SetFrontProcess, AppleScript or fallback.
// Returns after one attempt. The caller must independently verify AX focus.
int targetActivationPerform(void *ptr) {
    if (![NSThread isMainThread]) return -1;
    targetActivation *a=ptr;
    @autoreleasepool {
        NSRunningApplication *current=[[NSWorkspace sharedWorkspace] frontmostApplication];
        NSRunningApplication *target=[NSRunningApplication runningApplicationWithProcessIdentifier:a->pid];
        if (atomic_load(&a->cancelled) || activationInputHeld() || !activationActivitySame(a) || a->generation!=activationGeneration || ![current isEqual:a->source] ||
            ![target isEqual:a->target] || a->source.terminated || a->target.terminated) return -2;
        AXUIElementRef exact=activationResolve(a);
        int matches=exact && CFEqual(exact,a->window);
        if (exact) CFRelease(exact);
        if (!matches) return -3;
        Boolean settable=false; CFArrayRef actions=NULL;
        if (AXUIElementIsAttributeSettable(a->window,kAXMainAttribute,&settable)!=kAXErrorSuccess || !settable ||
            AXUIElementCopyActionNames(a->window,&actions)!=kAXErrorSuccess || !actions) return -4;
        int canRaise=CFArrayContainsValue(actions,CFRangeMake(0,CFArrayGetCount(actions)),kAXRaiseAction);
        CFRelease(actions);
        if (!canRaise || atomic_load(&a->cancelled) || !activationActivitySame(a) || a->generation!=activationGeneration) return -5;
        if (AXUIElementSetAttributeValue(a->window,kAXMainAttribute,kCFBooleanTrue)!=kAXErrorSuccess) return -6;
        targetMainLoopTurn(); // deliver main-queue activation notifications before each next stage
        if (!activationStagesUndisturbed(a)) return -7;
        if (AXUIElementPerformAction(a->window,kAXRaiseAction)!=kAXErrorSuccess) return -8;
        targetMainLoopTurn();
        if (!activationStagesUndisturbed(a)) return -9;
        if (![a->target activateWithOptions:0]) return -10;
        return 1;
    }
}

int targetActivationUndisturbed(void *ptr) {
    targetActivation *a=ptr;
    return activationStagesUndisturbed(a) && !a->target.terminated;
}
int targetActivationMatches(void *ptr, uintptr_t window, pid_t pid) {
    targetActivation *a=ptr;
    return pid==a->pid && window && CFEqual(a->window,(AXUIElementRef)window);
}
