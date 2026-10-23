//go:build computeruse && darwin

#import <AppKit/AppKit.h>
#import <CoreGraphics/CoreGraphics.h>

// AppKit's explicit window number survives conversion to Quartz. The public
// Quartz "window under pointer" fields alone decode as windowNumber=0 and do
// not establish the AppKit destination when posting directly to a PID.
// This constructs an immutable packet only: no NSWindow, activation, handling
// of the app's event queue, cursor warp or posting. The caller serializes input.
CGEventRef targetCreateWindowMouse(CGEventSourceRef source, CGEventType type, CGPoint point, CGMouseButton button, uint32_t wid) {
    @autoreleasepool {
        NSEvent *event=[NSEvent mouseEventWithType:(NSEventType)type location:NSZeroPoint
            modifierFlags:0 timestamp:NSProcessInfo.processInfo.systemUptime windowNumber:wid
            context:nil eventNumber:0 clickCount:type==kCGEventMouseMoved ? 0 : 1 pressure:0];
        if (!event || !event.CGEvent) return NULL;
        CGEventRef packet=CGEventCreateCopy(event.CGEvent);
        if (!packet) return NULL;
        CGEventSetSource(packet,source);
        CGEventSetLocation(packet,point); // public Quartz global display coordinates
        CGEventSetIntegerValueField(packet,kCGMouseEventButtonNumber,button);
        if ([NSEvent eventWithCGEvent:packet].windowNumber!=wid) { CFRelease(packet); return NULL; }
        return packet;
    }
}

// Packet inspection only, shared with regression tests. No event is posted.
long targetEventAppKitWindow(CGEventRef event) {
    @autoreleasepool { return [NSEvent eventWithCGEvent:event].windowNumber; }
}
