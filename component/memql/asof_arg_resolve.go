package memql

import (
	"fmt"
	"strings"
	"time"
)

// Caller-chosen point-in-time reads (memql#2992).
//
// `asOf` used to take an RFC3339 literal or the bare word `latest`, both
// resolved at parse time. So a DECLARED query could not offer a point in time
// to its callers: the only way to read the graph as it was at an instant the
// caller picks was to hand-build a runtime query string with the timestamp
// inlined. `component/deploycontrol` calls named queries only, which made
// point-in-time reachable in principle and off-path in practice -- against
// memql#1872's own stated criterion that a deployment timeline be
// "reconstructable asOf any time".
//
// The authored form is
//
//	asOf args.asOf ?? latest
//
// One clause covering both callers. Omit the arg and the behaviour is
// byte-identical to `asOf latest`, which is what lets the six existing
// `asOf latest` queries adopt it with no migration -- and those six are
// precisely the ones that could not be time-travelled by ANY spelling before,
// because a declared `asOf` also refuses to be wrapped ("multiple asOf()
// directives are not supported").
//
// # Why resolution lives at argument expansion
//
// It is the first point where args are known, and it is BEFORE
// applyDirectiveWrappers reads Timestamp / UseLatest onto the plan. Resolving
// here means the unresolved form never escapes: the SQL builder, the shadow
// analyzer, the collection-scan lint and every other consumer keep seeing the
// two literal shapes they already handled. The alternative -- threading an
// unresolved node all the way to the plan -- would have added a case to each
// of them, every one a place to forget.

// resolveAsOfArg turns a caller-chosen `asOf` into one of the two literal
// forms. A node with no ArgPath is returned unchanged, so every existing query
// takes a path that cannot behave differently.
//
// Target is deliberately NOT copied here; the caller assigns the expanded one.
func resolveAsOfArg(node *TimestampExpression, args map[string]any) (*TimestampExpression, error) {
	if node == nil {
		return nil, nil
	}
	if node.ArgPath == "" {
		// Literal `asOf "<ts>"` or `asOf latest` -- untouched, by construction.
		return &TimestampExpression{
			Timestamp: node.Timestamp,
			UseLatest: node.UseLatest,
		}, nil
	}

	raw, present := getNestedValue(args, node.ArgPath)
	if !present || raw == nil || isBlankAsOfArg(raw) {
		if node.FallbackLatest {
			// The whole point of the `?? latest` spelling: an omitted arg is
			// exactly `asOf latest`, so a caller that passes nothing gets the
			// behaviour the query had before it offered the choice.
			return &TimestampExpression{UseLatest: true}, nil
		}
		// UNREACHABLE FROM PARSED SOURCE as of memql#3028, and kept anyway.
		//
		// The parser is the only producer of a node with ArgPath set, and it
		// now refuses `asOf args.X` without the fallback, so it cannot emit
		// ArgPath != "" with FallbackLatest == false. Everything that could
		// reach here -- durable rehydration, raw query strings, sense -- goes
		// back through that same parser. asof_arg_resolve_test.go pins this
		// branch by hand-building the AST, which is the only way to produce it.
		//
		// Kept as defence in depth rather than deleted: it is the last guard
		// before an unresolved instant reaches temporal resolution, and it
		// costs one comparison. Do not read its existence as evidence that the
		// bare form is a shape the language emits -- it is not, and the message
		// below deliberately names the required spelling rather than offering
		// it as one option of two.
		return nil, fmt.Errorf(
			"asOf args.%s: no value supplied and no `?? latest` fallback declared -- this node "+
				"cannot come from parsed source (the fallback is required, memql#3028), so it was "+
				"built directly; write `asOf args.%s ?? latest` so an omitted value means the "+
				"latest version (memql#2992)",
			node.ArgPath, node.ArgPath)
	}

	ts, err := parseAsOfArgValue(raw)
	if err != nil {
		return nil, fmt.Errorf("asOf args.%s: %w", node.ArgPath, err)
	}
	return &TimestampExpression{Timestamp: &ts}, nil
}

// isBlankAsOfArg reports whether a supplied value should be treated as absent.
//
// This is load-bearing for the entire SDK path, not a convenience. The
// generated client builder emits the arg UNCONDITIONALLY:
//
//	b.WriteString("asOf: ")
//	b.WriteString(fmt.Sprintf("%q", args.AsOf))
//
// so every caller who leaves the field unset sends `asOf: ""`. Without this,
// each of them would get "invalid RFC3339 timestamp" instead of the latest
// version -- which would make adopting the form a breaking change for exactly
// the callers it was supposed to leave untouched.
//
// A non-string is never blank: it is either parseable below or a reportable
// error.
func isBlankAsOfArg(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// parseAsOfArgValue applies the same RFC3339 rule the literal string form
// applies at parse time, so the two spellings cannot drift into accepting
// different instants.
func parseAsOfArgValue(v any) (time.Time, error) {
	switch t := v.(type) {
	case time.Time:
		return t, nil
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(t))
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid RFC3339 timestamp %q", t)
		}
		return parsed, nil
	default:
		return time.Time{}, fmt.Errorf("expected an RFC3339 timestamp string, got %T", v)
	}
}
