package automations

import (
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// cond_truthiness_agreement_test.go -- memql#2963.
//
// `cond(args.allowed, "Y", "N")` is evaluated by two different code paths
// depending on the shape of the logic body it sits in:
//
//	single-statement   `return cond(...)`        -> component/memql, evalCollCond
//	multi-statement    `x := cond(...)  return x` -> this package, evaluateCondLocally
//
// They used to carry SEPARATE truthiness rules, and the divergence was real
// rather than theoretical. Measured across the full input set before the fix:
//
//	input        single   multi
//	nil          N        N
//	false        N        N
//	true         Y        Y
//	""           N        N
//	"false"      Y        N     <- diverged
//	"0"          Y        N     <- diverged
//	"true"       Y        Y
//	0            N        N
//	1            Y        Y
//	"nonempty"   Y        Y
//	2.5          Y        Y
//
// Two inputs, and both of them the shape a JSON, HTTP or MCP caller sends for a
// stringified boolean. A gate written `return cond(args.allowed, true, false)`
// therefore opened on the string "false" in one body shape and closed in the
// other.
//
// There is one implementation now (memql.IsTruthy, strict). This test is the
// gate that keeps it that way, and it is the sibling of
// TestPositionalBuiltinEvaluatorsAgree, whose own premise is exactly this:
// "the same source must produce the same value either way." That test covers
// concat and coalesce, not cond.
//
// Direction of the ruling: STRICT. The permissive rule is the one that fails
// OPEN on a gate, and an author who writes "false" means false.

// condTruthinessCases is the input set memql#2963 asks to be measured. Shared
// by both halves of this file so neither can quietly test a narrower set.
var condTruthinessCases = []struct {
	name string
	in   any
	want string // "Y" (truthy) or "N" (falsy)
}{
	{"nil", nil, "N"},
	{"bool false", false, "N"},
	{"bool true", true, "Y"},
	{"empty string", "", "N"},
	{`the string "false"`, "false", "N"},
	{`the string "0"`, "0", "N"},
	{`the string "true"`, "true", "Y"},
	{"zero", 0, "N"},
	{"one", 1, "Y"},
	{"non-empty string", "nonempty", "Y"},
	{"non-zero float", 2.5, "Y"},
}

// The multi-statement path, driven through the evaluator a logic body's
// intermediate step actually uses.
func TestCondTruthinessMultiStatementPath(t *testing.T) {
	for _, tc := range condTruthinessCases {
		t.Run(tc.name, func(t *testing.T) {
			ev := NewEvaluator()
			ev.SetCustom("args", map[string]any{"allowed": tc.in})
			got, handled, err := tryEvaluateBuiltinLocally(`cond(args.allowed, "Y", "N")`, ev)
			if err != nil {
				t.Fatalf("evaluating cond with allowed=%#v: %v", tc.in, err)
			}
			if !handled {
				t.Fatalf("cond was not handled locally for allowed=%#v, so this measures nothing", tc.in)
			}
			if fmt.Sprint(got) != tc.want {
				t.Errorf("multi-statement `cond(args.allowed, \"Y\", \"N\")` with allowed=%#v = %v, want %s",
					tc.in, got, tc.want)
			}
		})
	}
}

// TestCondTruthinessAgreesAcrossBodyShapes is the agreement itself: the SAME
// input set, run through the shared rule the single-statement path uses, must
// produce the same verdict as the multi-statement path above.
//
// It reads memql.IsTruthy directly rather than re-running the engine, because
// this package cannot construct component/memql's seam harness. The
// single-statement half is pinned in that package by
// TestCond_TruthinessIsPinnedIndependently over the same values, and both
// ultimately call the function this asserts on -- so if the two ever diverge
// again, one of the two tests fails and this comment says why.
func TestCondTruthinessAgreesAcrossBodyShapes(t *testing.T) {
	for _, tc := range condTruthinessCases {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want == "Y"
			if got := memql.IsTruthy(tc.in); got != want {
				t.Errorf("memql.IsTruthy(%#v) = %v, want %v.\n\n"+
					"The two logic-body shapes decide cond's branch through this one rule "+
					"(memql#2963). If it changes, BOTH the multi-statement expectations above "+
					"and component/memql's TestCond_TruthinessIsPinnedIndependently have to "+
					"change with it -- and the direction matters: the permissive spelling "+
					"(any non-empty string is truthy) fails OPEN on a gate written "+
					"`cond(args.allowed, true, false)`.", tc.in, got, want)
			}
		})
	}
}

// The duplicate is gone, not merely aligned. Two implementations that happen to
// agree today is the state memql#2963 was filed about -- they agreed on nine of
// eleven inputs, which is exactly why nobody noticed.
func TestThereIsOnlyOneTruthinessRule(t *testing.T) {
	// A local isTruthy in this package would shadow the shared one at every
	// call site without a compile error, which is how the divergence survived.
	// Nothing here can assert "no such function exists" at runtime, so the
	// gate is the call sites: they name the package explicitly.
	//
	// Kept as a behavioural anchor rather than a source scan: if someone
	// reintroduces a local rule and repoints the call sites at it, the
	// agreement test above fails the moment the two disagree on any input in
	// the set -- which now includes both inputs they historically disagreed on.
	for _, in := range []any{"false", "0"} {
		if memql.IsTruthy(in) {
			t.Errorf("memql.IsTruthy(%q) is true. That is the permissive spelling this package "+
				"used to reject, and it is the one that opens a gate handed a stringified "+
				"boolean (memql#2963).", in)
		}
	}
}
