package observe

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Invocation is the in-flight handle returned by Method() / Func().
// Construct once at the top of an instrumented function, optionally
// attach args / result, and finalize with End(&err) (typically via
// defer). The struct is small and stack-allocated; no allocations on
// the off-path.
type Invocation struct {
	ctx     context.Context
	fqn     string
	level   Level
	started time.Time
	args    map[string]any // built lazily; nil when level < meta or no Args() call
	result  any
}

// Method returns an Invocation handle for a method call. fqn must
// match the corresponding model.Node.ID
// (e.g. "method:github.com/.../auth.(*Handler).Login").
//
// Level resolution, in precedence:
//
//  1. Per-FQN override installed via SetProfile (driven by the
//     v1:observability:codeProfile CDC subscriber).
//  2. Process-wide DefaultLevel (set from MEMQL_OBSERVE_LEVEL at
//     start; mutable via SetDefaultLevel).
//
// When the resolved level is LevelOff, the returned handle is the
// zero value of Invocation and every subsequent method is a no-op;
// the helper never touches the heap on the off-path.
func Method(ctx context.Context, fqn string) Invocation {
	l, ok := lookupProfile(fqn)
	if !ok {
		l = DefaultLevel()
	}
	if l == LevelOff {
		return Invocation{}
	}
	return Invocation{
		ctx:     ctx,
		fqn:     fqn,
		level:   l,
		started: nowFn(),
	}
}

// Func is an alias for Method that reads better at the call site
// for non-receiver functions. It records the same record kind and
// fqn format ("func:..."); the cockpit renders both under the same
// drill-down node.
func Func(ctx context.Context, fqn string) Invocation { return Method(ctx, fqn) }

// Args attaches the captured arguments to the in-flight invocation.
// At LevelCount, the call is a fast no-op (the slice never gets
// walked). At LevelMeta, only the names + types + sizes are kept --
// no values. At LevelVerbose, the values are kept unless the name
// matches the default redact pattern OR the corresponding ArgKV
// carries Redact: true.
//
// The redact-by-default pattern catches obvious-by-name secrets
// even on functions that haven't yet been annotated with a
// //memql:observe redact=... marker; opt-out is by listing the
// param name in the marker (the extractor stamps the *safe* list
// onto the Method's `redact_args` attr, which the runtime sink
// inverts at write time -- see sink.go).
func (i Invocation) Args(kvs ...ArgKV) Invocation {
	if i.level < LevelMeta {
		return i
	}
	out := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		switch {
		case kv.Redact || matchesSecretName(kv.Name):
			out[kv.Name] = redactedPlaceholder
		case i.level == LevelMeta:
			out[kv.Name] = metaShape(kv.Value)
		default: // LevelVerbose
			out[kv.Name] = kv.Value
		}
	}
	i.args = out
	return i
}

// Result attaches the return value for capture at LevelVerbose.
// Below LevelVerbose, the call is a no-op so callers can wrap it
// unconditionally.
func (i Invocation) Result(v any) Invocation {
	if i.level < LevelVerbose {
		return i
	}
	i.result = v
	return i
}

// End finalizes the invocation and emits a Record to the active
// Sink. errp may be nil (function returns no error); when non-nil
// and *errp != nil, the error text lands on Record.Error.
//
// Pass-by-pointer to *error lets the canonical
//
//	defer observe.Method(ctx, "...").End(&err)
//
// shape work without callers having to capture the error in two
// places. errp is read at End time, so deferred error wrapping
// just before return is captured correctly.
func (i Invocation) End(errp *error) {
	if i.level == LevelOff || i.fqn == "" {
		return
	}
	end := nowFn()
	rec := Record{
		FQN:      i.fqn,
		Ts:       i.started,
		Duration: end.Sub(i.started),
		Args:     i.args,
		Result:   i.result,
		Level:    i.level,
	}
	if errp != nil && *errp != nil {
		rec.Error = (*errp).Error()
	}
	if tid, sid := extractTrace(i.ctx); tid != "" {
		rec.TraceID = tid
		rec.SpanID = sid
	}
	activeSink().Write(i.ctx, rec)
}

const redactedPlaceholder = "<redacted>"

// secretNamePattern matches argument names that look like secrets
// by convention. Conservative on purpose: false positives (a non-
// sensitive var named "key") are recoverable via the source-marker
// redact= list; false negatives (a secret named "x") are not.
var secretNamePattern = regexp.MustCompile(`(?i)(pass|token|secret|key|auth|credential)`)

func matchesSecretName(name string) bool {
	if name == "" {
		return false
	}
	return secretNamePattern.MatchString(name)
}

// metaShape returns a string description of a value without its
// contents -- e.g. "string(len=42)", "[]int(len=10)",
// "*foo.Bar(non-nil)". Used at LevelMeta so the trace still tells
// you *something* about the args without leaking values.
func metaShape(v any) string {
	if v == nil {
		return "<nil>"
	}
	switch x := v.(type) {
	case string:
		return fmt.Sprintf("string(len=%d)", len(x))
	case []byte:
		return fmt.Sprintf("[]byte(len=%d)", len(x))
	case int, int32, int64, uint, uint32, uint64, float32, float64, bool:
		return fmt.Sprintf("%T", v)
	}
	t := fmt.Sprintf("%T", v)
	// Length-bearing types report their cardinality without contents.
	type lenner interface{ Len() int }
	if l, ok := v.(lenner); ok {
		return fmt.Sprintf("%s(len=%d)", t, l.Len())
	}
	if strings.HasPrefix(t, "*") {
		return t + "(non-nil)"
	}
	return t
}
