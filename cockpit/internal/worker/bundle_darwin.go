//go:build darwin && computeruse

package worker

/*
#cgo LDFLAGS: -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>

static char *copyBundleString(CFStringRef value) {
    if (!value) return NULL;
    CFIndex size = CFStringGetMaximumSizeForEncoding(CFStringGetLength(value), kCFStringEncodingUTF8) + 1;
    char *text = malloc(size);
    if (!CFStringGetCString(value, text, size, kCFStringEncodingUTF8)) { free(text); return NULL; }
    return text;
}
static char *workerBundleID(void) {
    CFBundleRef bundle = CFBundleGetMainBundle();
    return bundle ? copyBundleString(CFBundleGetIdentifier(bundle)) : NULL;
}
static char *workerBundlePath(void) {
    CFBundleRef bundle = CFBundleGetMainBundle();
    if (!bundle || !CFBundleGetIdentifier(bundle)) return NULL;
    CFURLRef url = CFBundleCopyBundleURL(bundle);
    if (!url) return NULL;
    CFStringRef path = CFURLCopyFileSystemPath(url, kCFURLPOSIXPathStyle);
    char *text = copyBundleString(path);
    if (path) CFRelease(path);
    CFRelease(url);
    return text;
}
*/
import "C"
import "unsafe"

func currentWorkerBundle() (string, string) {
	id, path := C.workerBundleID(), C.workerBundlePath()
	defer C.free(unsafe.Pointer(id))
	defer C.free(unsafe.Pointer(path))
	return C.GoString(id), C.GoString(path)
}
