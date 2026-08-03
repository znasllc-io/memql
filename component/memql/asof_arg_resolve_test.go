package memql

import (
	"strings"
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// asof_arg_resolve_test.go -- memql#2992, the resolution half.
//
// The parser produces a TimestampExpression carrying an ArgPath; this is where
// the caller's value becomes an instant. It happens during argument expansion,
// which is the first point where args are known AND is before
// applyDirectiveWrappers reads Timestamp / UseLatest onto the plan -- so the
// unresolved form never escapes and no downstream consumer needs a new case.

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("fixture timestamp %q: %v", s, err)
	}
	return ts
}

// TestAsOfArg_OmittedArgIsExactlyLatest is the load-bearing one.
//
// It is what lets the six existing `asOf latest` queries adopt
// `asOf args.asOf ?? latest` with NO migration and no behaviour change for any
// caller that passes nothing. If this ever stops holding, adopting the form
// becomes a breaking change and the whole shape of memql#2992's ruling
// collapses.
func TestAsOfArg_OmittedArgIsExactlyLatest(t *testing.T) {
	node := &TimestampExpression{ArgPath: "asOf", FallbackLatest: true}

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"nil args", nil},
		{"empty args", map[string]any{}},
		{"arg present but nil", map[string]any{"asOf": nil}},
		{"arg present but empty string", map[string]any{"asOf": ""}},
		{"arg present but whitespace", map[string]any{"asOf": "   "}},
		{"an unrelated arg", map[string]any{"clusterId": "c1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveAsOfArg(node, tc.args)
			if err != nil {
				t.Fatalf("an omitted arg with `?? latest` must resolve, not error: %v", err)
			}
			if !got.UseLatest || got.Timestamp != nil {
				t.Errorf("omitted arg resolved to {UseLatest:%v Timestamp:%v}, want exactly "+
					"UseLatest -- the whole point of the `?? latest` spelling is that passing "+
					"nothing is byte-identical to the `asOf latest` the query had before "+
					"(memql#2992)", got.UseLatest, got.Timestamp)
			}
			if got.ArgPath != "" || got.FallbackLatest {
				t.Errorf("the resolved node still carries ArgPath=%q FallbackLatest=%v -- an "+
					"unresolved form must never reach the plan, or applyDirectiveWrappers would "+
					"need a case it does not have", got.ArgPath, got.FallbackLatest)
			}
		})
	}
}

// TestAsOfArg_SuppliedInstantResolves is the capability itself.
func TestAsOfArg_SuppliedInstantResolves(t *testing.T) {
	node := &TimestampExpression{ArgPath: "asOf", FallbackLatest: true}
	want := mustParseTime(t, "2026-07-28T12:00:00Z")

	for _, tc := range []struct {
		name string
		val  any
	}{
		{"an RFC3339 string", "2026-07-28T12:00:00Z"},
		{"the same, whitespace-padded", "  2026-07-28T12:00:00Z  "},
		{"an already-typed time.Time", want},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveAsOfArg(node, map[string]any{"asOf": tc.val})
			if err != nil {
				t.Fatalf("a supplied instant must resolve: %v", err)
			}
			if got.UseLatest {
				t.Error("a supplied instant resolved to UseLatest -- the caller's point in time " +
					"was discarded, which is the defect memql#2992 exists to fix")
			}
			if got.Timestamp == nil || !got.Timestamp.Equal(want) {
				t.Errorf("Timestamp = %v, want %v", got.Timestamp, want)
			}
		})
	}
}

// TestAsOfArg_LiteralFormsAreUntouched is the regression floor.
//
// Every query in the tree today uses one of these two, so if they can change
// this feature can break something. They take a path with no arg lookup at all.
func TestAsOfArg_LiteralFormsAreUntouched(t *testing.T) {
	ts := mustParseTime(t, "2026-01-01T00:00:00Z")

	got, err := resolveAsOfArg(&TimestampExpression{UseLatest: true}, map[string]any{"asOf": "2026-07-28T12:00:00Z"})
	if err != nil {
		t.Fatalf("`asOf latest` must resolve: %v", err)
	}
	if !got.UseLatest || got.Timestamp != nil {
		t.Errorf("`asOf latest` changed under an args map that happens to carry an `asOf` key: "+
			"{UseLatest:%v Timestamp:%v}. A literal clause must not read args at all (memql#2992).",
			got.UseLatest, got.Timestamp)
	}

	got, err = resolveAsOfArg(&TimestampExpression{Timestamp: &ts}, map[string]any{"asOf": "2026-07-28T12:00:00Z"})
	if err != nil {
		t.Fatalf("a literal timestamp must resolve: %v", err)
	}
	if got.Timestamp == nil || !got.Timestamp.Equal(ts) || got.UseLatest {
		t.Errorf("a literal timestamp was displaced by a caller arg: %v", got.Timestamp)
	}
}

// TestAsOfArg_BadValuesAreReported keeps a wrong instant from being silently
// swallowed into `latest`.
//
// That is the failure mode worth guarding: a typo'd timestamp resolving to
// "latest" would return a plausible answer for the wrong point in time, which
// is exactly the shape of silent wrongness this program keeps finding.
func TestAsOfArg_BadValuesAreReported(t *testing.T) {
	for _, tc := range []struct {
		name    string
		node    *TimestampExpression
		args    map[string]any
		wantMsg string
	}{
		{
			name:    "a malformed timestamp",
			node:    &TimestampExpression{ArgPath: "asOf", FallbackLatest: true},
			args:    map[string]any{"asOf": "yesterday"},
			wantMsg: "invalid RFC3339",
		},
		{
			name:    "a non-string, non-time value",
			node:    &TimestampExpression{ArgPath: "asOf", FallbackLatest: true},
			args:    map[string]any{"asOf": 1753704000},
			wantMsg: "expected an RFC3339 timestamp string",
		},
		{
			name:    "omitted with NO fallback declared",
			node:    &TimestampExpression{ArgPath: "at"},
			args:    map[string]any{},
			wantMsg: "no `?? latest` fallback declared",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveAsOfArg(tc.node, tc.args)
			if err == nil {
				t.Fatal("must be reported, not silently resolved -- a wrong instant that falls " +
					"back to `latest` returns a plausible answer for the wrong point in time")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error does not mention %q, so an author cannot act on it.\n  got: %v",
					tc.wantMsg, err)
			}
			if !strings.Contains(err.Error(), "asOf args.") {
				t.Errorf("error does not name the clause it came from.\n  got: %v", err)
			}
		})
	}
}

// TestAsOfArg_DottedPath covers a nested arg, which getNestedValue supports and
// the parser accepts.
func TestAsOfArg_DottedPath(t *testing.T) {
	node := &TimestampExpression{ArgPath: "window.start", FallbackLatest: true}
	want := mustParseTime(t, "2026-03-04T05:06:07Z")

	got, err := resolveAsOfArg(node, map[string]any{
		"window": map[string]any{"start": "2026-03-04T05:06:07Z"},
	})
	if err != nil {
		t.Fatalf("a dotted arg path must resolve: %v", err)
	}
	if got.Timestamp == nil || !got.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", got.Timestamp, want)
	}
}

// TestAsOfArg_EndToEndThroughAStructQuery is memql#2992's definition-of-done
// item 2: a caller-supplied instant driven through the real chain rather than
// through resolveAsOfArg directly.
//
// It parses a struct-form query carrying `asOf args.asOf ?? latest` exactly as
// the tree now declares it, runs the same argument expansion a call performs,
// and reads the resolved directive off the result. The seam tests above cannot
// see a break between the rewriter, the parser and expansion; this one can.
func TestAsOfArg_EndToEndThroughAStructQuery(t *testing.T) {
	const src = `use deployment.concepts.{ deployment }

@enabled
@description("memql#2992 end-to-end probe")
query deployment probeDeployments {
  args {
    clusterId  string!
    asOf       datetime
  }
  filter  clusterId == args.clusterId
  asOf    args.asOf ?? latest
}
`
	fn, err := tryParseNewFunctionSyntax("probeDeployments", "query", src, "memql#2992-test", memorynodes.DefaultRegistry())
	if err != nil {
		t.Fatalf("the struct-form query carrying `asOf args.asOf ?? latest` must parse: %v", err)
	}

	resolve := func(t *testing.T, args map[string]any) *TimestampExpression {
		t.Helper()
		v := newFunctionValidatorWithOrigin(nil, nil, 0)
		out, expErr := v.expandExpressionWithArgs(fn.Expr, args)
		if expErr != nil {
			t.Fatalf("expanding with args %v: %v", args, expErr)
		}
		te, ok := out.(*TimestampExpression)
		if !ok {
			t.Fatalf("expansion produced %T, not a *TimestampExpression -- the asOf directive is "+
				"no longer the root and this probe needs updating", out)
		}
		if te.ArgPath != "" {
			t.Errorf("the expanded node still carries ArgPath %q; an unresolved asOf must never "+
				"reach the plan (memql#2992)", te.ArgPath)
		}
		return te
	}

	t.Run("omitted arg behaves as asOf latest", func(t *testing.T) {
		te := resolve(t, map[string]any{"clusterId": "c1"})
		if !te.UseLatest || te.Timestamp != nil {
			t.Errorf("got {UseLatest:%v Timestamp:%v}, want exactly UseLatest. This is what lets "+
				"the existing `asOf latest` queries adopt the form with no migration -- if it "+
				"fails, adopting it is a breaking change (memql#2992).", te.UseLatest, te.Timestamp)
		}
	})

	t.Run("supplied instant pins the read", func(t *testing.T) {
		te := resolve(t, map[string]any{"clusterId": "c1", "asOf": "2026-07-28T12:00:00Z"})
		want := mustParseTime(t, "2026-07-28T12:00:00Z")
		if te.UseLatest {
			t.Error("a supplied instant resolved to UseLatest -- the caller's point in time was " +
				"discarded, which is the defect memql#2992 exists to fix")
		}
		if te.Timestamp == nil || !te.Timestamp.Equal(want) {
			t.Errorf("Timestamp = %v, want %v", te.Timestamp, want)
		}
	})
}

// A raw runtime query string has no declared arguments to resolve against, so
// the caller-arg form must be REFUSED rather than silently dropped
// (memql#2992 landing review).
//
// It parses: runtime strings go through the same parser entry point as a
// declared query. But applyDirectiveWrappers copied only Timestamp and
// UseLatest, so an ArgPath node lost its directive entirely -- the plan came
// out with NO asOf, the caller got a live-tip read having asked for a point in
// time, and nothing reported it. asof_arg_resolve.go's design note says the
// unresolved form never escapes; this was the path where it did.
//
// Asserted against applyDirectiveWrappers directly: it is the seam that drops
// the directive, and driving it needs no initialised engine.
func TestAsOfArg_UnresolvedFormIsRefusedByDirectiveWrappers(t *testing.T) {
	base := &ComparisonExpression{Field: FieldReference{Raw: "concept"}, Operator: "==", Value: "v1:cluster:node"}

	for _, tc := range []struct{ name, argPath string }{
		{"bare arg", "at"},
		{"arg with latest fallback", "asOf"},
		{"nested arg path", "window.start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := &QueryPlan{Root: &TimestampExpression{Target: base, ArgPath: tc.argPath}}
			_, err := applyDirectiveWrappers(plan)
			if err == nil {
				t.Fatalf("an unresolved asOf(args.%s) was accepted. There are no declared "+
					"arguments to resolve it against here, and the wrapper copies only Timestamp "+
					"and UseLatest -- so the directive is DROPPED and the read silently becomes a "+
					"live-tip read. The caller asked for a point in time and got 'now', with no "+
					"error (memql#2992).", tc.argPath)
			}
			if !strings.Contains(err.Error(), "2992") {
				t.Errorf("the refusal should cite the issue so the reason is findable, got: %v", err)
			}
		})
	}

	// The literal forms must still pass through -- that is how #1872's
	// reconstructability proof reads the graph at an instant.
	ts := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		node *TimestampExpression
	}{
		{"explicit instant", &TimestampExpression{Target: base, Timestamp: &ts}},
		{"latest", &TimestampExpression{Target: base, UseLatest: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := applyDirectiveWrappers(&QueryPlan{Root: tc.node}); err != nil {
				t.Errorf("%s must still pass through: %v", tc.name, err)
			}
		})
	}
}
