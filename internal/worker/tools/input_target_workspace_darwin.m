//go:build computeruse && darwin

#import <AppKit/AppKit.h>

// Public, read-only application identity. No activation or UI inspection.
pid_t targetKeyboardPID(void) {
    @autoreleasepool {
        NSRunningApplication *app = [[NSWorkspace sharedWorkspace] frontmostApplication];
        return app ? [app processIdentifier] : 0;
    }
}
