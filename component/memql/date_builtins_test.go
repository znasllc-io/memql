package memql

import (
	"strings"
	"testing"

	parser "github.com/znasllc-io/memql/component/language/parser"
)

// date_builtins_test.go covers the #2541 shared date/duration builtin
// dispatch (EvaluateDateBuiltin) and its wiring into the in-memory expression
// evaluator (evalCollScalar) that backs logic terminal returns at the
// engine's plan root and collection-lambda comparison values.

func TestEvaluateDateBuiltin_Values(t *testing.T) {
	cases := []struct {
		name string
		fn   string
		args []any
		want any
	}{
		{"addDuration_day", "addDuration", []any{"2026-01-01T00:00:00Z", "P1D"}, "2026-01-02T00:00:00Z"},
		{"addDuration_date_only", "addDuration", []any{"2026-03-10", "P1D"}, "2026-03-11T00:00:00Z"},
		{"daysBetween", "daysBetween", []any{"2026-01-01", "2026-01-11"}, 10},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvaluateDateBuiltin(tc.fn, tc.args)
			if err != nil {
				t.Fatalf("EvaluateDateBuiltin(%s, %v): %v", tc.fn, tc.args, err)
			}
			if got != tc.want {
				t.Errorf("EvaluateDateBuiltin(%s, %v) = %#v, want %#v", tc.fn, tc.args, got, tc.want)
			}
		})
	}
}

func TestEvaluateDateBuiltin_Errors(t *testing.T) {
	if _, err := EvaluateDateBuiltin("addDuration", []any{"not-a-date", "P1D"}); err == nil || !strings.Contains(err.Error(), "invalid timestamp") {
		t.Errorf("addDuration(not-a-date) err = %v, want invalid-timestamp", err)
	}
	if _, err := EvaluateDateBuiltin("daysBetween", []any{"2026-01-01"}); err == nil || !strings.Contains(err.Error(), "expects 2 args") {
		t.Errorf("daysBetween arity err = %v, want expects-2-args", err)
	}
	if _, err := EvaluateDateBuiltin("daysBetween", []any{nil, "2026-01-01"}); err == nil || !strings.Contains(err.Error(), "resolved to nil") {
		t.Errorf("daysBetween(nil) err = %v, want resolved-to-nil", err)
	}
	if _, err := EvaluateDateBuiltin("notADateFn", []any{"x"}); err == nil {
		t.Errorf("unknown builtin accepted; want error")
	}
	// #2707: the seven retired calendar builtins are gone from the dispatch.
	for _, retired := range []string{"subtractTimestamps", "year", "quarter", "month", "dayOfMonth", "isAnniversary", "isFirstDayOfQuarter"} {
		if _, err := EvaluateDateBuiltin(retired, []any{"2026-07-14"}); err == nil || !strings.Contains(err.Error(), "not a date/duration builtin") {
			t.Errorf("%s must be rejected by the dispatch (retired, #2707), err = %v", retired, err)
		}
	}
}

func TestIsDateBuiltin(t *testing.T) {
	for _, name := range []string{"addDuration", "daysBetween"} {
		if !IsDateBuiltin(name) {
			t.Errorf("IsDateBuiltin(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"coalesce", "concat", "cond", "timestamp", "",
		// Retired under 2026.08 (#2620 ruling / #2707):
		"subtractTimestamps", "year", "quarter", "month",
		"dayOfMonth", "isAnniversary", "isFirstDayOfQuarter",
	} {
		if IsDateBuiltin(name) {
			t.Errorf("IsDateBuiltin(%q) = true, want false", name)
		}
	}
}

// The engine-side in-memory evaluator (evalCollScalar) dispatches a
// date-builtin FunctionCallExpression -- the shape the logic converter emits
// -- including as an arithmetic operand (`daysBetween(a, b) / 7`), which is
// exactly what a single-return logic resolves to at plan root.
func TestEvalCollScalar_DateBuiltinDispatch(t *testing.T) {
	eval := func(src string) (any, error) {
		t.Helper()
		pexpr, err := parser.ParseExpression(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		engineExpr, err := NewASTConverter(WithCollectionMethods()).ConvertExpression(pexpr)
		if err != nil {
			t.Fatalf("convert %q: %v", src, err)
		}
		return evalCollScalar(engineExpr, nil, nil)
	}

	got, err := eval(`daysBetween("2026-01-01", "2026-01-15") / 7`)
	if err != nil {
		t.Fatalf("daysBetween / 7: %v", err)
	}
	if got != int64(2) {
		t.Errorf("daysBetween / 7 = %#v, want int64(2)", got)
	}

	// A non-date function call keeps the unsupported error.
	if _, err := eval(`daysBetween("2026-01-01", "not-a-date")`); err == nil {
		t.Errorf("invalid operand accepted; want a clean error")
	}
}
