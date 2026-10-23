//go:build computeruse && darwin

#import <AppKit/AppKit.h>
#import <CoreGraphics/CoreGraphics.h>
#include <dlfcn.h>
#include <pthread.h>

typedef void (*windowLocationSetter)(CGEventRef, CGPoint);
typedef CGPoint (*windowLocationGetter)(CGEventRef);
static windowLocationSetter setWindowLocation;
static windowLocationGetter getWindowLocation;
static pthread_once_t windowLocationOnce=PTHREAD_ONCE_INIT;
static void resolveWindowLocation(void) {
    // macOS private coordinate accessors. Public CGEventSetLocation invalidates
    // the window-local point, and PID posting does not rebuild it. Refuse
    // pointer packets if either accessor is absent; never post globally.
    setWindowLocation=(windowLocationSetter)dlsym(RTLD_DEFAULT,"CGEventSetWindowLocation");
    getWindowLocation=(windowLocationGetter)dlsym(RTLD_DEFAULT,"CGEventGetWindowLocation");
}

int targetWindowCoordinateAccessorsAvailable(void) {
    pthread_once(&windowLocationOnce,resolveWindowLocation);
    return setWindowLocation && getWindowLocation;
}

// AppKit's explicit window number survives conversion to Quartz. The public
// Quartz "window under pointer" fields alone decode as windowNumber=0 and do
// not establish the AppKit destination when posting directly to a PID.
// This constructs an immutable packet only: no NSWindow, activation, handling
// of the app's event queue, cursor warp or posting. The caller serializes input.
CGEventRef targetCreateWindowMouse(CGEventSourceRef source, CGEventType type, CGPoint point, CGMouseButton button, uint32_t wid, CGRect bounds) {
    @autoreleasepool {
        pthread_once(&windowLocationOnce,resolveWindowLocation);
        if (!setWindowLocation || !getWindowLocation || !wid || bounds.size.width<=0 || bounds.size.height<=0 || !CGRectContainsPoint(bounds,point)) return NULL;
        CGPoint local=CGPointMake(point.x-bounds.origin.x,point.y-bounds.origin.y);
        NSEvent *event=[NSEvent mouseEventWithType:(NSEventType)type location:NSZeroPoint
            modifierFlags:0 timestamp:NSProcessInfo.processInfo.systemUptime windowNumber:wid
            context:nil eventNumber:0 clickCount:type==kCGEventMouseMoved ? 0 : 1 pressure:0];
        if (!event || !event.CGEvent) return NULL;
        CGEventRef packet=CGEventCreateCopy(event.CGEvent);
        if (!packet) return NULL;
        CGEventSetSource(packet,source);
        CGEventSetLocation(packet,point); // public Quartz global display coordinates
        setWindowLocation(packet,local); // Quartz window-local, top-left origin
        CGEventSetIntegerValueField(packet,kCGMouseEventButtonNumber,button);
        if ([NSEvent eventWithCGEvent:packet].windowNumber!=wid || !CGPointEqualToPoint(CGEventGetLocation(packet),point) || !CGPointEqualToPoint(getWindowLocation(packet),local)) { CFRelease(packet); return NULL; }
        return packet;
    }
}

// Packet inspection only, shared with regression tests. No event is posted.
long targetEventAppKitWindow(CGEventRef event) {
    @autoreleasepool { return [NSEvent eventWithCGEvent:event].windowNumber; }
}

CGPoint targetEventWindowPoint(CGEventRef event) {
    pthread_once(&windowLocationOnce,resolveWindowLocation);
    return getWindowLocation ? getWindowLocation(event) : CGPointMake(NAN,NAN);
}
