//go:build computeruse && darwin

#import <AppKit/AppKit.h>
#include <stdatomic.h>

static atomic_bool targetLoopRunning;
static atomic_ulong targetLoopTurns;
static CFRunLoopTimerRef targetLoopTimer;
extern void targetActivationWatchStart(void);
extern void targetActivationWatchStop(void);

static void targetLoopTick(CFRunLoopTimerRef timer, void *info) {}

// Initialize the background worker's display connection on the process's main
// thread. No windows, menus, activation, permission prompts or input events.
int targetMainLoopStart(void) {
    if (![NSThread isMainThread]) return -1;
    if (atomic_load(&targetLoopRunning)) return -2;
    @autoreleasepool {
        NSApplication *app = [NSApplication sharedApplication];
        if ([app activationPolicy] != NSApplicationActivationPolicyProhibited &&
            ![app setActivationPolicy:NSApplicationActivationPolicyProhibited]) return -3;
        if ([app activationPolicy] != NSApplicationActivationPolicyProhibited) return -5;
        [NSWorkspace sharedWorkspace];
        targetLoopTimer = CFRunLoopTimerCreate(NULL, CFAbsoluteTimeGetCurrent()+0.05,
            0.05, 0, 0, targetLoopTick, NULL);
        if (!targetLoopTimer) return -4;
        CFRunLoopAddTimer(CFRunLoopGetMain(), targetLoopTimer, kCFRunLoopCommonModes);
        targetActivationWatchStart();
        atomic_store(&targetLoopTurns, 0);
        atomic_store(&targetLoopRunning, true);
        return 1;
    }
}

void targetMainLoopTurn(void) {
    @autoreleasepool {
        CFRunLoopRunInMode(kCFRunLoopDefaultMode, 0.05, true);
        atomic_fetch_add(&targetLoopTurns, 1);
    }
}

void targetMainLoopStop(void) {
    atomic_store(&targetLoopRunning, false);
    targetActivationWatchStop();
    if (targetLoopTimer) {
        CFRunLoopTimerInvalidate(targetLoopTimer);
        CFRelease(targetLoopTimer);
        targetLoopTimer = NULL;
    }
}

int targetMainLoopRunning(void) { return atomic_load(&targetLoopRunning); }
unsigned long targetMainLoopTurns(void) { return atomic_load(&targetLoopTurns); }

// Public, read-only application identity. No activation or UI inspection.
pid_t targetKeyboardPID(void) {
    @autoreleasepool {
        NSRunningApplication *app = [[NSWorkspace sharedWorkspace] frontmostApplication];
        return app ? [app processIdentifier] : 0;
    }
}
