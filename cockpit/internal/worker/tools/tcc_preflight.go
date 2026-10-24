package tools

// ScreenCapturePreflightHook is a per-call preflight check for the
// macOS Screen Recording permission. Returns true when the calling
// process is approved (or has inherited approval from its parent),
// false otherwise.
//
// The hook is set at init time by the worker package's darwin && computeruse
// build (internal/worker/tcc_hook_darwin.go) and
// remains nil on every other platform / build configuration. Callers
// must treat nil as "the platform doesn't gate this; proceed."
//
// Why a package-level var rather than a constructor param: the
// dispatcher is wired generically (no platform-specific types in its
// signature), and the computer-use permissions are an operating-system
// detail rather than something the dispatcher should be aware of.
// The hook lets the worker package inject the check at startup
// without taking on a cgo dependency on the tools package itself.
var ScreenCapturePreflightHook func() bool
