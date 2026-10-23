#import <AppKit/AppKit.h>
#import <CoreGraphics/CoreGraphics.h>
#include <stdio.h>

// Same production translation unit lets this fixture simulate absent/broken
// accessors without a runtime override in the worker.
#include "../input_event_darwin.m"
static void ignoreWindowPoint(CGEventRef event, CGPoint point) { (void)event;(void)point; }

// No activation, focus query or event posting. The producer is another process
// which cannot resolve the receiving process's NSWindow object.
int main(int argc, const char **argv) { @autoreleasepool {
    if (argc == 8) {
        uint32_t wid=(uint32_t)strtoul(argv[1],NULL,10);
        CGRect bounds=CGRectMake(atof(argv[2]),atof(argv[3]),atof(argv[4]),atof(argv[5]));
        CGPoint point=CGPointMake(atof(argv[6]),atof(argv[7]));
        CGEventSourceRef source=CGEventSourceCreate(kCGEventSourceStatePrivate);
        CGEventRef packet=targetCreateWindowMouse(source,kCGEventLeftMouseDown,point,kCGMouseButtonLeft,wid,bounds);
        CFRelease(source);
        if (!packet) return 2;
        NSData *data=CFBridgingRelease(CGEventCreateData(kCFAllocatorDefault,packet));
        fwrite(data.bytes,1,data.length,stdout);
        CFRelease(packet);
        return 0;
    }
    [NSApplication sharedApplication];
    [NSApp setActivationPolicy:NSApplicationActivationPolicyProhibited];
    pthread_once(&windowLocationOnce,resolveWindowLocation);
    windowLocationSetter originalSetter=setWindowLocation;
    windowLocationGetter originalGetter=getWindowLocation;
    CGEventSourceRef source=CGEventSourceCreate(kCGEventSourceStatePrivate);
    for (int failure=0;failure<3;failure++) {
        setWindowLocation=failure==0?NULL:failure==2?ignoreWindowPoint:originalSetter;
        getWindowLocation=failure==1?NULL:originalGetter;
        CGEventRef packet=targetCreateWindowMouse(source,kCGEventLeftMouseDown,CGPointMake(100,200),kCGMouseButtonLeft,42,CGRectMake(0,0,500,500));
        if (packet) { CFRelease(packet);return 7; }
    }
    setWindowLocation=originalSetter;getWindowLocation=originalGetter;CFRelease(source);
    for (NSUInteger style=0;style<2;style++) {
        NSWindow *window=[[NSWindow alloc] initWithContentRect:NSMakeRect(300,400,500,300)
            styleMask:style?NSWindowStyleMaskTitled:NSWindowStyleMaskBorderless backing:NSBackingStoreBuffered defer:NO];
        window.releasedWhenClosed=NO;
        NSRect frame=window.frame;
        CGRect bounds=CGRectMake(frame.origin.x,NSMaxY(NSScreen.mainScreen.frame)-NSMaxY(frame),frame.size.width,frame.size.height);
        CGPoint point=CGPointMake(bounds.origin.x+100,bounds.origin.y+bounds.size.height-80);
        NSTask *task=[NSTask new];
        task.executableURL=[NSURL fileURLWithPath:@(argv[0])];
        task.arguments=@[[NSString stringWithFormat:@"%ld",(long)window.windowNumber],
            @(bounds.origin.x).stringValue,@(bounds.origin.y).stringValue,@(bounds.size.width).stringValue,
            @(bounds.size.height).stringValue,@(point.x).stringValue,@(point.y).stringValue];
        NSPipe *pipe=[NSPipe pipe];task.standardOutput=pipe;
        NSError *error=nil;
        if (![task launchAndReturnError:&error]) { fprintf(stderr,"producer launch failed\n");return 3; }
        NSData *data=[pipe.fileHandleForReading readDataToEndOfFile];[task waitUntilExit];
        if (task.terminationStatus || !data.length) return 4;
        CGEventRef packet=CGEventCreateFromData(kCFAllocatorDefault,(__bridge CFDataRef)data);
        if (!packet) return 5;
        NSEvent *event=[NSEvent eventWithCGEvent:packet];CGPoint global=CGEventGetLocation(packet);
        BOOL ok=event.windowNumber==window.windowNumber && NSEqualPoints(event.locationInWindow,NSMakePoint(100,80)) &&
            CGPointEqualToPoint(global,point) && !window.visible && NSApp.activationPolicy==NSApplicationActivationPolicyProhibited;
        fprintf(stderr,"foreign packet style=%lu local=(%.1f,%.1f) global=(%.1f,%.1f) window=%ld visible=%d\n",
            (unsigned long)style,event.locationInWindow.x,event.locationInWindow.y,global.x,global.y,(long)event.windowNumber,window.visible);
        CFRelease(packet);[window close];
        if (!ok) return 6;
    }
    return 0;
}}
