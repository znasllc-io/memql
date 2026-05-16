package observe

import "time"

// ArgKV is one captured argument. Captured at the call site via the
// Arg() / RedactedArg() constructors; the helper decides whether
// (and how) the value lives in the final Record based on the
// active Level and the function's redact list.
type ArgKV struct {
	Name   string
	Value  any
	Redact bool // if true, value is force-redacted regardless of level
}

// Arg captures a single argument by name. Use this for safe values
// (ids, modes, public counts). The helper still applies a name-
// pattern redactor by default, so even Arg("password", x) emits a
// redacted form unless the function's source marker explicitly
// listed "password" in `redact=` (which inverts the meaning: that
// list names the params SAFE to capture).
func Arg(name string, value any) ArgKV { return ArgKV{Name: name, Value: value} }

// RedactedArg captures a name with a placeholder value. The Record
// always shows "<redacted>" for these even at LevelVerbose. Useful
// for arguments the caller knows are sensitive without relying on
// the name pattern.
func RedactedArg(name string) ArgKV { return ArgKV{Name: name, Value: nil, Redact: true} }

// Record is one invocation row written by a Sink. Field set mirrors
// the v1:observability:invocation concept on the MemQL side and the
// code_invocation hypertable on the Postgres side, so a Sink can
// serialize directly without an intermediate type.
type Record struct {
	FQN      string         // matches model.Node.ID for a Method/Func
	Ts       time.Time      // start time
	Duration time.Duration  // end - Ts
	Error    string         // non-empty when the caller's *error was non-nil
	Args     map[string]any // captured per active Level; redacted values appear as "<redacted>"
	Result   any            // captured at LevelVerbose only
	TraceID  string         // from SetTraceExtractor
	SpanID   string         // from SetTraceExtractor
	Level    Level          // the level at which this record was captured (audit)
}
