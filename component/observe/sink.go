package observe

import (
	"context"
	"log/slog"
)

// Sink consumes finalized Records. The package ships a default
// slog-backed sink so importing observe with no further wiring
// still produces useful output (a debug log line per call).
// Production deployments register a buffered TimescaleDB writer
// via Register(); the helper itself is sink-agnostic.
//
// Implementations MUST be non-blocking; the helper calls Write
// synchronously from the instrumented function's deferred path
// and a slow sink directly tax the application's request latency.
// Buffered sinks should drop on full rather than block, so the
// instrumentation never becomes a head-of-line waiter.
type Sink interface {
	Write(ctx context.Context, r Record)
}

// slogSink is the always-available default. Writes a structured
// log line at Debug level so it stays out of normal operator
// output but is available with one env-var flip.
type slogSink struct{}

func (slogSink) Write(_ context.Context, r Record) {
	attrs := []any{
		"fqn", r.FQN,
		"level", r.Level.String(),
		"duration_ms", r.Duration.Milliseconds(),
		"ts", r.Ts,
	}
	if r.Error != "" {
		attrs = append(attrs, "error", r.Error)
	}
	if r.TraceID != "" {
		attrs = append(attrs, "trace_id", r.TraceID, "span_id", r.SpanID)
	}
	if r.Args != nil {
		attrs = append(attrs, "args", r.Args)
	}
	if r.Result != nil {
		attrs = append(attrs, "result", r.Result)
	}
	slog.Debug("observe.invocation", attrs...)
}
