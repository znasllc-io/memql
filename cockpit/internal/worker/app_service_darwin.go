//go:build darwin && computeruse

package worker

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Foundation -framework ServiceManagement
#import <Foundation/Foundation.h>
#import <ServiceManagement/ServiceManagement.h>
#include <stdlib.h>
#include <string.h>

// SMAppServiceStatus as an int: 0 not registered, 1 enabled, 2 requires
// approval, 3 not found; -1 before macOS 13.
static int memqlAppServiceStatus(const char *plist) {
    if (@available(macOS 13.0, *)) {
        return (int)[SMAppService agentServiceWithPlistName:[NSString stringWithUTF8String:plist]].status;
    }
    return -1;
}

// 0 on success, otherwise ServiceManagement's error code with *why set to a
// malloc'd sentence.
static long memqlAppServiceChange(const char *plist, int on, char **why) {
    if (@available(macOS 13.0, *)) {
        @autoreleasepool {
            SMAppService *service = [SMAppService agentServiceWithPlistName:[NSString stringWithUTF8String:plist]];
            NSError *error = nil;
            BOOL done = on ? [service registerAndReturnError:&error] : [service unregisterAndReturnError:&error];
            if (done) return 0;
            const char *text = error.localizedDescription.UTF8String;
            *why = strdup(text ? text : "macOS refused the change");
            return error.code ? (long)error.code : -1;
        }
    }
    *why = strdup("macOS 13 or later is required");
    return -1;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// appServicesAvailable is true only for MemQL.app's own executable:
// SMAppService finds the plists in the bundle of the process that asks.
func appServicesAvailable() bool {
	id, _ := currentWorkerBundle()
	return id == "com.znasllc.memql-worker"
}

func appServiceStatus(plist string) int {
	name := C.CString(plist)
	defer C.free(unsafe.Pointer(name))
	return int(C.memqlAppServiceStatus(name))
}

// ServiceManagement's codes for a change that is already true
// (SMErrors.h): registering what is registered, unregistering what is not.
const (
	smErrorJobNotFound       = 6
	smErrorAlreadyRegistered = 12
)

func changeAppService(plist string, on bool) error {
	name := C.CString(plist)
	defer C.free(unsafe.Pointer(name))
	flag := C.int(0)
	if on {
		flag = 1
	}
	var why *C.char
	code := C.memqlAppServiceChange(name, flag, &why)
	if why != nil {
		defer C.free(unsafe.Pointer(why))
	}
	switch {
	case code == 0, on && code == smErrorAlreadyRegistered, !on && code == smErrorJobNotFound:
		return nil
	}
	return fmt.Errorf("%s (ServiceManagement error %d)", C.GoString(why), int(code))
}
