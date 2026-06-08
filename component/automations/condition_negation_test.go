package automations

import (
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// Regression for memql#1096: the automation/logic condition evaluator must
// honour a leading `!` (NOT) operator. Before the fix `evaluateAtomicCondition`
// fell through to EvaluateValue + isTruthy without ever negating, so `!true`
// read truthy and `! $steps.x.Empty` fired even on an empty result. These
// tests pin the now-correct negation semantics plus its composition with
// `&&` / `||` / parens, and assert `!=` comparisons stay unaffected.

func negStepResult(empty bool) *StepResult {
	var nodes []*memqlv1.MemoryNode
	if !empty {
		payload, err := structpb.NewStruct(map[string]any{"name": "x"})
		if err != nil {
			panic(err)
		}
		nodes = append(nodes, &memqlv1.MemoryNode{Id: "v1:agents:agent:x", Payload: payload})
	}
	return &StepResult{
		StepId: "x",
		Status: "success",
		Result: &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}},
	}
}

func TestEvaluateCondition_BangLiterals(t *testing.T) {
	cases := []struct {
		cond string
		want bool
	}{
		{"!true", false},
		{"!false", true},
		{"!!true", true},
		{"!!false", false},
		{"! true", false}, // whitespace after `!` allowed
	}
	for _, tc := range cases {
		e := NewEvaluator()
		got, err := e.EvaluateCondition(tc.cond)
		if err != nil {
			t.Fatalf("EvaluateCondition(%q): %v", tc.cond, err)
		}
		if got != tc.want {
			t.Errorf("EvaluateCondition(%q) = %v, want %v", tc.cond, got, tc.want)
		}
	}
}

// `! $steps.x.Empty` is the method-truthiness form the compiler emits for
// `!x.Empty()`. On an empty step result Empty=true so `!Empty` must be FALSE;
// on a non-empty result Empty=false so `!Empty` must be TRUE.
func TestEvaluateCondition_BangStepEmpty(t *testing.T) {
	cases := []struct {
		name  string
		empty bool
		want  bool
	}{
		{"empty result -> !Empty is false", true, false},
		{"non-empty result -> !Empty is true", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEvaluator()
			e.SetStepResult("x", negStepResult(tc.empty))
			got, err := e.EvaluateCondition("! $steps.x.Empty")
			if err != nil {
				t.Fatalf("EvaluateCondition: %v", err)
			}
			if got != tc.want {
				t.Errorf("`! $steps.x.Empty` (empty=%v) = %v, want %v", tc.empty, got, tc.want)
			}
		})
	}
}

// `!$steps.x` (no whitespace) is also accepted as the NOT-prefix form.
func TestEvaluateCondition_BangNoSpace(t *testing.T) {
	e := NewEvaluator()
	e.SetStepResult("x", negStepResult(true)) // empty -> Empty truthy-ish
	got, err := e.EvaluateCondition("!$steps.x.Empty")
	if err != nil {
		t.Fatalf("EvaluateCondition: %v", err)
	}
	if got != false {
		t.Errorf("`!$steps.x.Empty` on empty = %v, want false", got)
	}
}

// Precedence + composition: `!` binds tighter than comparisons, which bind
// tighter than `&&`, which binds tighter than `||`.
func TestEvaluateCondition_BangComposition(t *testing.T) {
	cases := []struct {
		name string
		cond string
		a    any
		b    any
		want bool
	}{
		// !a && b  == (!a) && b
		{"!a && b: a=false,b=true -> true", "!$a && $b", false, true, true},
		{"!a && b: a=true,b=true -> false", "!$a && $b", true, true, false},
		// !a || b  == (!a) || b
		{"!a || b: a=true,b=false -> false", "!$a || $b", true, false, false},
		{"!a || b: a=true,b=true -> true", "!$a || $b", true, true, true},
		{"!a || b: a=false,b=false -> true", "!$a || $b", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEvaluator()
			e.SetCustom("a", tc.a)
			e.SetCustom("b", tc.b)
			got, err := e.EvaluateCondition(tc.cond)
			if err != nil {
				t.Fatalf("EvaluateCondition(%q): %v", tc.cond, err)
			}
			if got != tc.want {
				t.Errorf("EvaluateCondition(%q) = %v, want %v", tc.cond, got, tc.want)
			}
		})
	}
}

// `!(a == 1)` negates a parenthesised comparison; `!(a || b)` negates a
// parenthesised OR group.
func TestEvaluateCondition_BangParenGroup(t *testing.T) {
	// !(a == 1)
	e := NewEvaluator()
	e.SetCustom("a", 1)
	got, err := e.EvaluateCondition("!($a == 1)")
	if err != nil {
		t.Fatalf("EvaluateCondition: %v", err)
	}
	if got != false {
		t.Errorf("!($a == 1) with a=1 = %v, want false", got)
	}

	e = NewEvaluator()
	e.SetCustom("a", 2)
	got, err = e.EvaluateCondition("!($a == 1)")
	if err != nil {
		t.Fatalf("EvaluateCondition: %v", err)
	}
	if got != true {
		t.Errorf("!($a == 1) with a=2 = %v, want true", got)
	}

	// !(a || b): negates the whole OR group.
	e = NewEvaluator()
	e.SetCustom("a", false)
	e.SetCustom("b", false)
	got, err = e.EvaluateCondition("!($a || $b)")
	if err != nil {
		t.Fatalf("EvaluateCondition: %v", err)
	}
	if got != true {
		t.Errorf("!(false || false) = %v, want true", got)
	}

	e = NewEvaluator()
	e.SetCustom("a", true)
	e.SetCustom("b", false)
	got, err = e.EvaluateCondition("!($a || $b)")
	if err != nil {
		t.Fatalf("EvaluateCondition: %v", err)
	}
	if got != false {
		t.Errorf("!(true || false) = %v, want false", got)
	}
}

// Regression: a leading-operand `!` that is part of a `!=` comparison must
// NOT be treated as a unary NOT -- `x != 1` stays a comparison.
func TestEvaluateCondition_NotEqualUnaffected(t *testing.T) {
	cases := []struct {
		x    any
		want bool
	}{
		{2, true},  // 2 != 1
		{1, false}, // 1 != 1
	}
	for _, tc := range cases {
		e := NewEvaluator()
		e.SetCustom("x", tc.x)
		got, err := e.EvaluateCondition("$x != 1")
		if err != nil {
			t.Fatalf("EvaluateCondition($x=%v): %v", tc.x, err)
		}
		if got != tc.want {
			t.Errorf("`$x != 1` with x=%v = %v, want %v", tc.x, got, tc.want)
		}
	}
}
