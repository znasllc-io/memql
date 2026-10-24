"""Exercise the actual native bridge against a fake service, never launchd."""
import pathlib
import re
import subprocess
import tempfile


SERVICE = r'''
#import <Foundation/Foundation.h>
#import <dispatch/dispatch.h>
#include <assert.h>

static int completedCalls = 0;
static int synchronousCalls = 0;
static int completionCode = 0;
static int64_t completionDelay = 20 * NSEC_PER_MSEC;
static dispatch_semaphore_t callbackFinished;
@interface SMAppService : NSObject
@property(nonatomic) int status;
+ (instancetype)agentServiceWithPlistName:(NSString *)name;
- (BOOL)registerAndReturnError:(NSError **)error;
- (BOOL)unregisterAndReturnError:(NSError **)error;
- (void)unregisterWithCompletionHandler:(void (^)(NSError *))handler;
@end
@implementation SMAppService
+ (instancetype)agentServiceWithPlistName:(NSString *)name { return [self new]; }
- (BOOL)registerAndReturnError:(NSError **)error { return YES; }
- (BOOL)unregisterAndReturnError:(NSError **)error { synchronousCalls++; return YES; }
- (void)unregisterWithCompletionHandler:(void (^)(NSError *))handler {
    int result = completionCode;
    dispatch_after(dispatch_time(DISPATCH_TIME_NOW, completionDelay),
                   dispatch_get_global_queue(QOS_CLASS_DEFAULT, 0), ^{
        completedCalls++;
        handler(result ? [NSError errorWithDomain:@"fixture" code:result userInfo:nil] : nil);
        dispatch_semaphore_signal(callbackFinished);
    });
}
@end
'''

CASES = r'''
int main(void) {
    @autoreleasepool {
        callbackFinished = dispatch_semaphore_create(0);
        char *why = NULL;
        assert(memqlAppServiceChange("fixture.plist", 0, &why) == 0);
        assert(dispatch_semaphore_wait(callbackFinished, dispatch_time(DISPATCH_TIME_NOW, NSEC_PER_SEC)) == 0);
        assert(completedCalls == 1 && synchronousCalls == 0);
        assert(why == NULL);
        completionCode = 77;
        assert(memqlAppServiceChange("fixture.plist", 0, &why) == 77);
        assert(dispatch_semaphore_wait(callbackFinished, dispatch_time(DISPATCH_TIME_NOW, NSEC_PER_SEC)) == 0);
        assert(completedCalls == 2 && synchronousCalls == 0);
        assert(why != NULL);
        free(why); why = NULL;
        assert(memqlAppServiceChange("fixture.plist", 1, &why) == 0);
        assert(completedCalls == 2 && why == NULL);
        // The callback can arrive after the bounded native call returns. ARC
        // must retain its semaphore/error storage; it never captures &why.
        completionDelay = 10 * NSEC_PER_SEC + 100 * NSEC_PER_MSEC;
        assert(memqlAppServiceChange("fixture.plist", 0, &why) == -1);
        assert(why && strstr(why, "not finished stopping"));
        free(why); why = NULL;
        assert(dispatch_semaphore_wait(callbackFinished, dispatch_time(DISPATCH_TIME_NOW, NSEC_PER_SEC)) == 0);
        assert(completedCalls == 3 && synchronousCalls == 0);
    }
    return 0;
}
'''


def main():
    cockpit = pathlib.Path(__file__).resolve().parents[2]
    source = (cockpit / 'internal/worker/app_service_darwin.go').read_text()
    bridge = source.split('/*', 1)[1].split('*/', 1)[0]
    bridge = re.sub(r'^#cgo.*$', '', bridge, flags=re.MULTILINE)
    bridge = bridge.replace('#import <ServiceManagement/ServiceManagement.h>', SERVICE)
    with tempfile.TemporaryDirectory(prefix='memql-service-native-') as temporary:
        root = pathlib.Path(temporary)
        native = root / 'service.m'
        executable = root / 'service-test'
        native.write_text(bridge + CASES)
        subprocess.run(['xcrun', 'clang', '-x', 'objective-c', '-fobjc-arc', '-fblocks',
                        '-framework', 'Foundation', str(native), '-o', str(executable)], check=True)
        subprocess.run([str(executable)], check=True, timeout=15)
    print('PASS: native unregister completion, error propagation, bounded timeout and late callback lifetime; service APIs stubbed.')


if __name__ == '__main__':
    main()
