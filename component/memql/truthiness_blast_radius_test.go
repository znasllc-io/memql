package memql

import (
	"testing"
)

// truthiness_blast_radius_test.go -- memql#2963.
//
// The ruling that made the strings "false" and "0" falsy did NOT only change
// cond. It changed every construct that asks whether a value is true, because
// there is deliberately one rule for all of them: the collection lambdas
// (.filter / .where / .any() / .all() / .count()) and the logical operators
// && / || / !. The mutation-template conditionals were the third surface; a
// mutation value has no truthiness since the flip (D8), and
// TestMutationValuesV1BooleanIsNotTruthy pins that it refuses these inputs.
//
// This file exists because that was measured and found untested. Reverting
// IsTruthy's string arm to the old permissive spelling produced EXACTLY ONE
// failure across the whole component/memql suite -- TestCond_TruthinessIsPinned-
// Independently -- while every construct below silently flipped semantics. A
// language change advertised as global with coverage on one construct is a
// change whose blast radius nobody has pinned.
//
// So each test here drives a DIFFERENT construct and asserts the two
// historically-permissive inputs are falsy through it. Together with
// TestCond_TruthinessIsPinnedIndependently they cover the surfaces the docs
// name in docs/public/language/functions.md.

// truthinessBlastCases is the pair that moved. Deliberately narrow: the full
// input set is pinned once, on cond, in cond_argref_test.go. What this file
// adds is BREADTH of construct, not depth of input.
var truthinessBlastCases = []struct {
	name string
	in   any
}{
	{`the string "false"`, "false"},
	{`the string "0"`, "0"},
}

func lit(v any) ExpressionNode { return &LiteralValueNode{Value: v} }

// && short-circuits on a falsy left operand, so a permissive rule makes this
// return true.
func TestTruthinessBlastRadius_LogicalAnd(t *testing.T) {
	for _, tc := range truthinessBlastCases {
		t.Run(tc.name, func(t *testing.T) {
			node := &LogicalExpression{Op: LogicalAnd, Left: lit(tc.in), Right: lit(true)}
			got, err := evalCollLogical(node, map[string]any{}, nil)
			if err != nil {
				t.Fatalf("evalCollLogical(&&, %#v): %v", tc.in, err)
			}
			if got != false {
				t.Errorf("%#v && true = %#v, want false.\n\n"+
					"&& reads its operands through the one truthiness rule (memql#2963). "+
					"A permissive rule makes this true, which is the fail-open direction the "+
					"ruling exists to close.", tc.in, got)
			}
		})
	}
}

// || short-circuits on a truthy left operand, so a permissive rule makes this
// true without ever evaluating the right.
func TestTruthinessBlastRadius_LogicalOr(t *testing.T) {
	for _, tc := range truthinessBlastCases {
		t.Run(tc.name, func(t *testing.T) {
			node := &LogicalExpression{Op: LogicalOr, Left: lit(tc.in), Right: lit(false)}
			got, err := evalCollLogical(node, map[string]any{}, nil)
			if err != nil {
				t.Fatalf("evalCollLogical(||, %#v): %v", tc.in, err)
			}
			if got != false {
				t.Errorf("%#v || false = %#v, want false.\n\n"+
					"|| reads its operands through the one truthiness rule (memql#2963).", tc.in, got)
			}
		})
	}
}

// .any() and .all() over a one-element collection, driven through
// evalCollectionMethod -- the real method dispatcher -- with a lambda whose
// body returns the element itself. Both funnel the lambda's RESULT through the
// truthiness rule, so with one element they answer exactly what the rule says
// about it.
func TestTruthinessBlastRadius_CollectionAnyAll(t *testing.T) {
	for _, tc := range truthinessBlastCases {
		for _, method := range []string{"any", "all"} {
			t.Run(method+" "+tc.name, func(t *testing.T) {
				node := &CollectionMethodExpression{
					Receiver: lit([]any{tc.in}),
					Method:   method,
					Args: []ExpressionNode{&LambdaExpression{
						Params: []string{"m"},
						Body:   &SpecReferenceExpression{Name: "m"},
					}},
				}
				got, err := evalCollectionMethod(node, map[string]any{}, map[string]any{})
				if err != nil {
					t.Fatalf("evalCollectionMethod(.%s(), %#v): %v", method, tc.in, err)
				}
				if got != false {
					t.Errorf("[%#v].%s(m => m) = %#v, want false.\n\n"+
						"A collection lambda's result is read through the one truthiness rule "+
						"(memql#2963). A permissive rule makes this true, and .any() is the "+
						"fail-open direction: a gate written `rows.any(r => r.blocked)` handed "+
						"the string \"false\" would report a block that is not there.",
						tc.in, method, got)
				}
			})
		}
	}
}

// The unhandled-type arm, pinned because it is the one place the rule fails
// OPEN and the docs now say so out loud. Not an endorsement -- a record, so
// that changing it is a deliberate act with a test to update.
func TestTruthinessUnhandledTypesAreTruthy(t *testing.T) {
	type someStruct struct{ A int }
	for _, tc := range []struct {
		name string
		in   any
	}{
		{"a struct", someStruct{}},
		{"an int32 zero", int32(0)},
		{"a float32 zero", float32(0)},
		{"an empty typed slice", []string{}},
		{"an empty typed map", map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !IsTruthy(tc.in) {
				t.Errorf("IsTruthy(%#v) = false, want true.\n\n"+
					"Values the rule does not name fall to the catch-all and are TRUE. That is "+
					"documented in docs/public/language/functions.md; if this arm changes, the "+
					"doc changes with it.", tc.in)
			}
		})
	}
}

// The exact-match property of the string arm, likewise documented and likewise
// worth a test rather than a comment: only "false" and "0" are falsy.
func TestTruthinessStringComparisonIsExact(t *testing.T) {
	for _, in := range []string{"FALSE", "False", " false", "false ", "0.0", "00"} {
		if !IsTruthy(in) {
			t.Errorf("IsTruthy(%q) = false, want true -- only the exact strings "+
				"\"false\" and \"0\" are falsy, which is why the docs tell callers to "+
				"normalise a stringified boolean before it reaches a condition (memql#2963).", in)
		}
	}
	for _, in := range []string{"false", "0"} {
		if IsTruthy(in) {
			t.Errorf("IsTruthy(%q) = true, want false -- the memql#2963 ruling", in)
		}
	}
}
