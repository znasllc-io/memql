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
		{"subtractTimestamps", "subtractTimestamps", []any{"2026-01-01T02:00:00Z", "2026-01-01T00:00:00Z"}, "PT2H0M0S"},
		{"year", "year", []any{"2026-07-14T09:00:00Z"}, 2026},
		{"quarter", "quarter", []any{"2026-07-14"}, 3},
		{"month", "month", []any{"2026-07-14"}, 7},
		{"dayOfMonth", "dayOfMonth", []any{"2026-07-14"}, 14},
		{"isAnniversary", "isAnniversary", []any{"2024-07-14", "2026-07-14"}, true},
		{"isFirstDayOfQuarter", "isFirstDayOfQuarter", []any{"2026-07-01"}, true},
		{"isFirstDayOfQuarter_false", "isFirstDayOfQuarter", []any{"2026-07-02"}, false},
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
	if _, err := EvaluateDateBuiltin("year", []any{"not-a-date"}); err == nil || !strings.Contains(err.Error(), "invalid timestamp") {
		t.Errorf("year(not-a-date) err = %v, want invalid-timestamp", err)
	}
	if _, err := EvaluateDateBuiltin("daysBetween", []any{"2026-01-01"}); err == nil || !strings.Contains(err.Error(), "expects 2 args") {
		t.Errorf("daysBetween arity err = %v, want expects-2-args", err)
	}
	if _, err := EvaluateDateBuiltin("year", []any{nil}); err == nil || !strings.Contains(err.Error(), "resolved to nil") {
		t.Errorf("year(nil) err = %v, want resolved-to-nil", err)
	}
	if _, err := EvaluateDateBuiltin("notADateFn", []any{"x"}); err == nil {
		t.Errorf("unknown builtin accepted; want error")
	}
}

func TestIsDateBuiltin(t *testing.T) {
	for _, name := range []string{
		"addDuration", "daysBetween", "subtractTimestamps", "year",
		"quarter", "month", "dayOfMonth", "isAnniversary", "isFirstDayOfQuarter",
	} {
		if !IsDateBuiltin(name) {
			t.Errorf("IsDateBuiltin(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"coalesce", "concat", "cond", "timestamp", ""} {
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

	got, err = eval(`year("2026-07-14")`)
	if err != nil {
		t.Fatalf("year: %v", err)
	}
	if got != 2026 {
		t.Errorf("year = %#v, want 2026", got)
	}

	// A non-date function call keeps the unsupported error.
	if _, err := eval(`daysBetween("2026-01-01", "not-a-date")`); err == nil {
		t.Errorf("invalid operand accepted; want a clean error")
	}
}
