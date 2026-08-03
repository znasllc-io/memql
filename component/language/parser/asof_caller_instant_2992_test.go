package parser

import (
	"strings"
	"testing"
)

// asof_caller_instant_2992_test.go -- memql#2992, the parse half.
//
// `asOf` took an RFC3339 literal or the bare word `latest`, both resolved at
// parse time. So a DECLARED query could not offer a point in time to its
// callers: the only route was a hand-built runtime query string with the
// instant inlined, and `component/deploycontrol` calls named queries only --
// which made point-in-time reachable in principle and off-path in practice,
// against memql#1872's own criterion.
//
// Ruled form:
//
//	asOf args.asOf ?? latest
//
// One clause for both callers. Omit the arg and it is exactly `asOf latest`,
// which is what lets the six existing `asOf latest` queries adopt it with no
// behaviour change -- and those six are precisely the ones that could not be
// time-travelled by ANY spelling before, because a declared `asOf` also
// refuses to be wrapped ("multiple asOf() directives are not supported").
//
// The resolution half is in component/memql (asof_arg_resolve.go): this file
// only asserts what the parser produces.

// parseAsOfClause parses a query whose body carries `asOf <clause>` and returns
// the TimestampExpr the parser built.
func parseAsOfClause(t *testing.T, clause string) (*TimestampExpr, error) {
	t.Helper()
	// ParseExpression drives the same parseAsOfFunction the query body reaches
	// (TestAsOfAllowedInStandaloneExpression pins that they share it), without
	// the (value, error) return shape a procedural query body requires -- which
	// keeps these assertions about the asOf clause rather than about the
	// surrounding function form.
	node, err := ParseExpression("asOf(concept==v1:cluster:node, " + clause + ")")
	if err != nil {
		return nil, err
	}
	te, ok := node.(*TimestampExpr)
	if !ok {
		t.Fatalf("clause %q parsed to %T, not *TimestampExpr -- the parse shape changed and this "+
			"helper needs updating, not the assertions below", clause, node)
	}
	return te, nil
}

func TestAsOf_CallerInstantForms(t *testing.T) {
	for _, tc := range []struct {
		name       string
		clause     string
		wantArg    string
		wantFB     bool
		wantLatest bool
		wantTS     bool
	}{
		{
			name: "bare latest is unchanged", clause: "latest",
			wantLatest: true,
		},
		{
			name: "RFC3339 literal is unchanged", clause: `"2026-07-28T12:00:00Z"`,
			wantTS: true,
		},
		{
			name: "caller arg", clause: "args.at",
			wantArg: "at",
		},
		{
			name: "caller arg with the latest fallback", clause: "args.asOf ?? latest",
			wantArg: "asOf", wantFB: true,
		},
		{
			name: "a dotted arg path", clause: "args.window.start ?? latest",
			wantArg: "window.start", wantFB: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te, err := parseAsOfClause(t, tc.clause)
			if err != nil {
				t.Fatalf("`asOf %s` must parse: %v", tc.clause, err)
			}
			if te.ArgPath != tc.wantArg {
				t.Errorf("ArgPath = %q, want %q", te.ArgPath, tc.wantArg)
			}
			if te.FallbackLatest != tc.wantFB {
				t.Errorf("FallbackLatest = %v, want %v", te.FallbackLatest, tc.wantFB)
			}
			if te.UseLatest != tc.wantLatest {
				t.Errorf("UseLatest = %v, want %v", te.UseLatest, tc.wantLatest)
			}
			if (te.Timestamp != nil) != tc.wantTS {
				t.Errorf("Timestamp set = %v, want %v", te.Timestamp != nil, tc.wantTS)
			}
			// The two literal forms must never acquire an ArgPath -- that is
			// what keeps every existing query on a path this change cannot
			// alter.
			if (tc.wantLatest || tc.wantTS) && te.ArgPath != "" {
				t.Errorf("a literal asOf form gained ArgPath %q; the literal forms must stay "+
					"byte-identical to their pre-memql#2992 parse", te.ArgPath)
			}
		})
	}
}

// TestAsOf_RejectedClauses keeps the widening narrow.
func TestAsOf_RejectedClauses(t *testing.T) {
	for _, tc := range []struct{ name, clause, wantMsg string }{
		{"a bare identifier is still not a timestamp", "bogus", "RFC3339"},
		{"args with no name", "args", "args.<name>"},
		{"a non-latest ?? fallback", `args.at ?? "2026-01-01T00:00:00Z"`, "only supported `??` fallback"},
		{"an empty timestamp string", `""`, "must not be empty"},
		{"a malformed timestamp", `"not-a-time"`, "invalid RFC3339"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseAsOfClause(t, tc.clause)
			if err == nil {
				t.Fatalf("`asOf %s` must be refused at parse time", tc.clause)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error does not mention %q, so an author cannot act on it.\n  got: %v",
					tc.wantMsg, err)
			}
		})
	}
}

// TestAsOf_CallerInstantStaysQueryOnly is the guard that matters most.
//
// memql#2305 / the core-builtins ADR §2.3 make `asOf` query-only: it is
// rejected in logic, automation and mutation bodies, because a temporal
// dependency is declared THROUGH the query a body imports. Adding a new
// accepted spelling must not open that door, and it would be an easy thing to
// miss -- the new branch sits after the query-only check, not before it.
func TestAsOf_CallerInstantStaysQueryOnly(t *testing.T) {
	for _, body := range []string{"Logic", "Automation"} {
		t.Run(body, func(t *testing.T) {
			src := "func (" + body + ") xReadsAsOf(ctx any) (any, error) {\n" +
				"  return asOf(concept==v1:cluster:node, args.at ?? latest)\n" +
				"}"
			_, err := ParseFile(src)
			if err == nil {
				t.Fatalf("the caller-instant asOf form parsed in a %s body -- memql#2992 must not "+
					"widen the query-only rule (memql#2305, core-builtins ADR 2.3)", body)
			}
			if !strings.Contains(err.Error(), "query-only") {
				t.Errorf("expected the query-only error, got: %v", err)
			}
		})
	}
}
