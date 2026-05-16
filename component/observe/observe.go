// Package observe is the runtime side of the architecture
// framework: a thin instrumentation surface call sites use to emit
// per-invocation records (count, duration, args, result, error)
// that the cockpit's drill-down view consumes alongside the static
// topology model.
//
// Design goals:
//
//   - Stay out of the way when the level is "off" (the default in
//     production). The fast-path is one map lookup, one comparison,
//     and a stack-allocated zero value -- no allocations, no
//     channels, no syscalls.
//
//   - Stay Go-native. Built on context.Context, log/slog, and
//     standard channels; no SDK dependencies. The sink is an
//     interface that a TimescaleDB writer plugs into; the default
//     sink logs structured events via slog so something useful
//     still happens when no writer is registered.
//
//   - Be safe by default. Argument capture redacts anything whose
//     parameter name matches /(?i)pass|token|secret|key|auth/
//     unless the declaring function explicitly listed it as safe
//     via a //memql:observe redact=... source marker (consulted
//     by the static extractor; this package consumes the resulting
//     attrs only at runtime).
//
// Usage at a call site that should report:
//
//	func (h *Handler) Login(ctx context.Context, user, password string) (err error) {
//	    defer observe.Method(ctx, "method:.../auth.(*Handler).Login").
//	        Args(observe.Arg("user", user), observe.Arg("password", password)).
//	        End(&err)
//	    ...
//	}
//
// The fully-qualified name string must match the `id` of the
// corresponding Method node in topology.model.json; the cockpit
// joins on that key when overlaying invocation history onto the
// rendered diagram.
package observe

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"
)

// Level is the runtime verbosity dial. Values correspond exactly to
// the observe_level attribute the static extractor stamps on Method /
// Func nodes from the //memql:observe source marker. Comparison is
// numeric so a runtime check ("am I at least at meta?") is one
// integer compare.
type Level int

const (
	LevelOff     Level = 0
	LevelCount   Level = 1
	LevelMeta    Level = 2
	LevelVerbose Level = 3
)

// ParseLevel turns the textual form (off / count / meta / verbose)
// into a Level. Unknown / empty strings resolve to LevelOff so a
// typo in .env never accidentally turns observability on; the
// caller can detect this case by checking ok.
func ParseLevel(s string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "":
		return LevelOff, s == "" || strings.EqualFold(s, "off")
	case "count":
		return LevelCount, true
	case "meta":
		return LevelMeta, true
	case "verbose":
		return LevelVerbose, true
	}
	return LevelOff, false
}

// String reverses ParseLevel for log and diagnostics output.
func (l Level) String() string {
	switch l {
	case LevelCount:
		return "count"
	case LevelMeta:
		return "meta"
	case LevelVerbose:
		return "verbose"
	default:
		return "off"
	}
}

// EnvLevelVar is the env var the package reads at init() time to
// pick the global default. Per-method levels (set live via the
// codeProfile concept once that lands) override this; until then
// it's the only knob.
const EnvLevelVar = "MEMQL_OBSERVE_LEVEL"

var (
	mu           sync.RWMutex
	defaultLevel = LevelOff
	sink         Sink = slogSink{} // safe default; replaced by Register
)

func init() {
	if l, ok := ParseLevel(os.Getenv(EnvLevelVar)); ok {
		defaultLevel = l
	}
}

// DefaultLevel returns the current process-wide default level.
// Cheap and lock-free in the hot path: read once at start of each
// Method() call, branch on it, and skip the rest of the helper if
// the value is LevelOff.
func DefaultLevel() Level {
	mu.RLock()
	l := defaultLevel
	mu.RUnlock()
	return l
}

// SetDefaultLevel changes the process-wide default. Callers should
// only use this when responding to a live codeProfile update; tests
// may also call it to scope a single test. Returns the previous
// value so the caller can restore it (test cleanup pattern).
func SetDefaultLevel(l Level) Level {
	mu.Lock()
	prev := defaultLevel
	defaultLevel = l
	mu.Unlock()
	return prev
}

// Register installs a Sink that consumes invocation records. The
// default sink logs to slog; the TimescaleDB writer registers a
// real Sink at startup. Passing nil resets to the default.
func Register(s Sink) {
	mu.Lock()
	if s == nil {
		sink = slogSink{}
	} else {
		sink = s
	}
	mu.Unlock()
}

// activeSink returns the currently-registered Sink under a brief
// read lock. The lock scope is intentionally tight so the hot path
// doesn't block when an admin swaps the sink.
func activeSink() Sink {
	mu.RLock()
	s := sink
	mu.RUnlock()
	return s
}

// TraceIDFromContext is the hook the helpers use to stamp
// trace_id / span_id onto each record. Wired by callers that have
// distributed-trace context (OpenTelemetry, the gRPC interceptor,
// etc.) via SetTraceExtractor; default is a no-op so the package
// has no required dependency on a tracing system.
type TraceIDFromContext func(ctx context.Context) (traceID, spanID string)

var (
	traceExtractor TraceIDFromContext = func(context.Context) (string, string) { return "", "" }
	traceMu        sync.RWMutex
)

// SetTraceExtractor installs the trace-correlation hook.
func SetTraceExtractor(f TraceIDFromContext) {
	if f == nil {
		f = func(context.Context) (string, string) { return "", "" }
	}
	traceMu.Lock()
	traceExtractor = f
	traceMu.Unlock()
}

func extractTrace(ctx context.Context) (string, string) {
	traceMu.RLock()
	f := traceExtractor
	traceMu.RUnlock()
	return f(ctx)
}

// nowFn is overridable in tests; production wires time.Now.
var nowFn = time.Now
