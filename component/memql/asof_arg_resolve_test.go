package memql

import (
	"strings"
	"testing"
	"time"
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
