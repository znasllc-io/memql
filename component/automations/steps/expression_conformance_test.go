package steps

// Expression-evaluator conformance matrix (memql#593).
//
// memQL evaluates DSL value-expressions with TWO engines depending on where
// the expression appears: ARG-TIME (mutation/function step args ->
// MutationExecutor.evaluateValue) and LOGIC-TIME (logic-body RHS / return ->
// the LogicRunner's local-leaf entry automations.EvaluateLocalExpr). Same
// syntax, two implementations -- so a fix to one silently leaves the other
// broken (exactly how the #575/#580 ghost AI survived).
//
// This matrix runs ONE table of (expression, seeded steps) -> expected value
// against BOTH entry points, using REAL *memql.ExecuteResult step results (the
// #575 lesson: fabricated map[string]any{"Bundle":...} shapes hid the bug). A
// case that diverges between the two evaluators fails here, so they can never
// drift apart undetected again.
//
// Two contracts are pinned:
//   - leafCases: the bare-step / skipped / non-step-literal LEAF resolution both
//     evaluators bottom out in (arg-time evaluateValue vs the logic-time leaf
//     Evaluator.EvaluateValue).
//   - localExprCases: the local-expression subset logic bodies actually use --
//     coalesce + step-method calls (.first()/.count()/.empty()) AND a method-THEN-
//     field path inside a coalesce arg (the ghost-AI shape) -- run through the
//     FULL logic-time path (EvaluateLocalExpr) vs arg-time evaluateValue. They
//     all converge.
//
// Story 5 (ADR §2.2 / #2303) retired the legacy capitalized accessor spelling
// (.First()/.Len()/.Empty()/.Last()/.Nodes()); only the lowercase forms resolve
// now. TestStepMethodAccessors_CapitalizedRetired pins that retirement.

import (
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// nodeResult builds a real *memql.ExecuteResult carrying one node (id + a
// flat payload), the shape a real query step yields.
func nodeResult(id string, payload map[string]any) *memqlengine.ExecuteResult {
	pb, err := structpb.NewStruct(payload)
	if err != nil {
		panic(err)
	}
	return &memqlengine.ExecuteResult{
		Bundle: &memqlv1.GraphBundle{
			Nodes: []*memqlv1.MemoryNode{{Id: id, Payload: pb}},
		},
	}
}

// argTime is the ARG-TIME entry: MutationExecutor.evaluateValue (mutation/
// function step args). logicLeaf is the LOGIC-TIME leaf primitive both share.
// logicLocal is the FULL logic-time local-expression path.
func argTime(eval *automations.Evaluator, expr string) (any, error) {
	return (&MutationExecutor{}).evaluateValue(eval, expr)
}

func logicLeaf(eval *automations.Evaluator, expr string) (any, error) {
	return eval.EvaluateValue(expr)
}

func logicLocal(eval *automations.Evaluator, expr string) (any, error) {
	val, handled, err := automations.EvaluateLocalExpr(expr, eval)
	if err != nil {
		return nil, err
	}
	if !handled {
		return nil, fmt.Errorf("EvaluateLocalExpr did not handle %q (it would run as an engine step)", expr)
	}
	return val, nil
}

type conformanceCase struct {
	name  string
	setup func(*automations.Evaluator) // seed steps
	expr  string
	want  any
}

func (c conformanceCase) newSeededEvaluator() *automations.Evaluator {
	eval := automations.NewEvaluator()
	if c.setup != nil {
		c.setup(eval)
	}
	return eval
}

func seedStep(id string, result any, status string) func(*automations.Evaluator) {
	return func(eval *automations.Evaluator) {
		eval.SetStepResult(id, &automations.StepResult{StepId: id, Status: status, Result: result})
	}
}

func seedSteps(setups ...func(*automations.Evaluator)) func(*automations.Evaluator) {
	return func(eval *automations.Evaluator) {
		for _, s := range setups {
			s(eval)
		}
	}
}

// leafCases: the shared LEAF resolution contract (arg-time vs logic-time leaf).
var leafCases = []conformanceCase{
	{
		name: "bare_step_resolves_to_result",
		setup: seedStep("getActiveGA",
			nodeResult("v1:agents:agent:real", map[string]any{"name": "Sofia"}), "success"),
		expr: "getActiveGA",
		want: nodeResult("v1:agents:agent:real", map[string]any{"name": "Sofia"}),
	},
	{
		name:  "skipped_step_resolves_nil",
		setup: seedStep("getFallbackGA", nil, "skipped"),
		expr:  "getFallbackGA",
		want:  nil,
	},
	{
		name: "bare_non_step_stays_literal",
		expr: "writer",
		want: "writer",
	},
}

// localExprCases: the local-expression subset logic bodies use (coalesce +
// step-method), run through the FULL logic-time path vs arg-time.
var localExprCases = []conformanceCase{
	{
		name: "coalesce_first_present_step",
		setup: seedSteps(
			seedStep("active", nodeResult("v1:agents:agent:active", map[string]any{"name": "Sofia"}), "success"),
			seedStep("fallback", nil, "skipped")),
		expr: "coalesce(active, fallback)",
		want: nodeResult("v1:agents:agent:active", nil),
	},
	{
		name: "coalesce_skips_to_second_step",
		setup: seedSteps(
			seedStep("active", nil, "skipped"),
			seedStep("fallback", nodeResult("v1:agents:agent:fallback", nil), "success")),
		expr: "coalesce(active, fallback)",
		want: nodeResult("v1:agents:agent:fallback", nil),
	},
	{
		name:  "coalesce_falls_through_to_literal_default",
		setup: seedStep("missing", nil, "skipped"),
		expr:  `coalesce(missing, "default")`,
		want:  "default",
	},
	{
		name:  "step_method_first",
		setup: seedStep("rows", nodeResult("v1:x:1", map[string]any{"k": "v"}), "success"),
		expr:  "rows.first()",
		want:  map[string]any{"id": "v1:x:1", "payload": map[string]any{"k": "v"}},
	},
	{
		name:  "step_method_count",
		setup: seedStep("rows", nodeResult("v1:x:1", nil), "success"),
		expr:  "rows.count()",
		want:  1,
	},
	{
		name:  "step_method_empty_false",
		setup: seedStep("rows", nodeResult("v1:x:1", nil), "success"),
		expr:  "rows.empty()",
		want:  false,
	},
	{
		// The ghost-AI shape (#575/#580): a method-THEN-field path inside a
		// coalesce arg. Both evaluators now navigate it to the node id rather
		// than ever flowing the raw expression text (memql#593).
		name:  "coalesce_method_then_field",
		setup: seedStep("rows", nodeResult("v1:x:1", map[string]any{"k": "v"}), "success"),
		expr:  `coalesce(rows.first().id, "")`,
		want:  "v1:x:1",
	},
	// Date/duration builtins (#2541): both entry points dispatch through the
	// shared name-keyed evaluator (memql.EvaluateDateBuiltin), so an
	// addDuration/daysBetween/year/... computes the same value as a mutation
	// step ARG and as a logic step VALUE / terminal return.
	{
		name: "date_addDuration_literals",
		expr: `addDuration("2026-01-01T00:00:00Z", "P1D")`,
		want: "2026-01-02T00:00:00Z",
	},
	{
		name: "date_daysBetween_literals",
		expr: `daysBetween("2026-01-01", "2026-01-11")`,
		want: 10,
	},
	{
		name: "date_year_literal",
		expr: `year("2026-07-14T09:30:00Z")`,
		want: 2026,
	},
	{
		name: "date_month_literal",
		expr: `month("2026-07-14T09:30:00Z")`,
		want: 7,
	},
	{
		name: "date_dayOfMonth_literal",
		expr: `dayOfMonth("2026-07-14T09:30:00Z")`,
		want: 14,
	},
	{
		// A bare step reference as a date-builtin operand: the step's raw
		// string result feeds the date parser on both paths.
		name:  "date_year_of_step_result",
		setup: seedStep("startedAt", "2026-07-14T09:30:00Z", "success"),
		expr:  `year(startedAt)`,
		want:  2026,
	},
	{
		name:  "date_addDuration_over_step_result",
		setup: seedStep("startedAt", "2026-03-10", "success"),
		expr:  `addDuration(startedAt, "P1D")`,
		want:  "2026-03-11T00:00:00Z",
	},
}

// logicOnlyExprCases: positions supported ONLY at logic time. Infix binary
// arithmetic (#2542 item 1) is deliberately NOT parsed out of arg-time
// strings -- a `/` or `-` inside an arg-time string is data (URLs, slugs,
// dates), never division, and the explicit add()/sub() builtins already
// cover arg-time arithmetic. TestArithmetic_ArgTimeStaysLiteral pins that
// boundary.
var logicOnlyExprCases = []conformanceCase{
	{
		name: "arith_step_division",
		setup: seedSteps(
			seedStep("revenue", 100, "success"),
			seedStep("orders", 4, "success")),
		expr: `(revenue / orders)`,
		want: 25,
	},
	{
		name: "arith_precedence_grouping",
		setup: seedSteps(
			seedStep("good", 9, "success"),
			seedStep("total", 10, "success")),
		expr: `((good * 100) / total)`,
		want: 90,
	},
	{
		name:  "arith_step_method_operand",
		setup: seedStep("rows", nodeResult("v1:x:1", nil), "success"),
		expr:  `(rows.count() * 2)`,
		want:  2,
	},
	{
		// A date builtin nested as an arithmetic operand (#2541+#2542).
		name: "arith_date_builtin_operand",
		expr: `(daysBetween("2026-07-01", "2026-07-15") / 7)`,
		want: 2,
	},
}

// TestStepMethodAccessors_CapitalizedRetired pins Story 5 (ADR §2.2 / #2303):
// the legacy capitalized accessor spelling no longer resolves through either
// evaluator. The logic-time path errors ("unknown StepResult field: First"),
// and the arg-time path falls through to the raw, unresolved expression text
// (never the resolved value). Either way, the capitalized form is no longer a
// working accessor -- the lowercase localExprCases above are the live surface.
func TestStepMethodAccessors_CapitalizedRetired(t *testing.T) {
	retired := []string{"rows.First()", "rows.Len()", "rows.Empty()", "rows.Last()", "rows.Nodes()"}
	for _, expr := range retired {
		expr := expr
		t.Run(expr, func(t *testing.T) {
			seed := seedStep("rows", nodeResult("v1:x:1", map[string]any{"k": "v"}), "success")

			// Logic-time path: must NOT resolve to a value (errors on the
			// unknown capitalized field).
			if _, err := logicLocal(seededEvaluator(seed), expr); err == nil {
				t.Fatalf("logicLocal(%q) resolved, but the capitalized accessor was retired (#2303)", expr)
			}

			// Arg-time path: falls through to the raw, unresolved literal.
			got, err := argTime(seededEvaluator(seed), expr)
			if err != nil {
				t.Fatalf("argTime(%q): unexpected error: %v", expr, err)
			}
			if got != expr {
				t.Fatalf("argTime(%q) = %#v, want the raw unresolved literal %q (capitalized accessor retired, #2303)", expr, got, expr)
			}
		})
	}
}

func seededEvaluator(setup func(*automations.Evaluator)) *automations.Evaluator {
	eval := automations.NewEvaluator()
	if setup != nil {
		setup(eval)
	}
	return eval
}

func TestExpressionEvaluators_Conformance(t *testing.T) {
	run := func(t *testing.T, cases []conformanceCase, engines []struct {
		name string
		fn   func(*automations.Evaluator, string) (any, error)
	}) {
		for _, c := range cases {
			for _, eng := range engines {
				t.Run(c.name+"/"+eng.name, func(t *testing.T) {
					got, err := eng.fn(c.newSeededEvaluator(), c.expr)
					if err != nil {
						t.Fatalf("%s(%q): unexpected error: %v", eng.name, c.expr, err)
					}
					if !conformanceEqual(got, c.want) {
						t.Fatalf("%s(%q) = %#v, want %#v -- the two evaluators must agree (memql#593)", eng.name, c.expr, got, c.want)
					}
				})
			}
		}
	}

	run(t, leafCases, []struct {
		name string
		fn   func(*automations.Evaluator, string) (any, error)
	}{
		{"argTime", argTime},
		{"logicLeaf", logicLeaf},
	})

	run(t, localExprCases, []struct {
		name string
		fn   func(*automations.Evaluator, string) (any, error)
	}{
		{"argTime", argTime},
		{"logicLocal", logicLocal},
	})

	run(t, logicOnlyExprCases, []struct {
		name string
		fn   func(*automations.Evaluator, string) (any, error)
	}{
		{"logicLocal", logicLocal},
	})
}

// TestArithmetic_ArgTimeStaysLiteral pins the logic-only boundary of infix
// arithmetic (#2542 item 1): the ARG-TIME resolver must NOT reinterpret an
// operator-shaped string as arithmetic -- an arg string containing `/` is
// data. It resolves to the raw literal text, exactly its pre-#2542
// behaviour.
func TestArithmetic_ArgTimeStaysLiteral(t *testing.T) {
	seed := seedSteps(
		seedStep("revenue", 100, "success"),
		seedStep("orders", 4, "success"))
	got, err := argTime(seededEvaluator(seed), "revenue / orders")
	if err != nil {
		t.Fatalf("argTime(revenue / orders): unexpected error: %v", err)
	}
	if got != "revenue / orders" {
		t.Fatalf("argTime(revenue / orders) = %#v, want the raw literal (infix arithmetic is logic-time only, #2542)", got)
	}
}

// conformanceEqual compares results structurally enough for the matrix: two
// *ExecuteResult are "equal" when their first node ids match (the navigable
// identity the downstream payload/.First().id reads care about); maps and
// primitives compare by value.
func conformanceEqual(got, want any) bool {
	gr, gok := got.(*memqlengine.ExecuteResult)
	wr, wok := want.(*memqlengine.ExecuteResult)
	if gok || wok {
		if !(gok && wok) {
			return false
		}
		return firstNodeID(gr) == firstNodeID(wr)
	}
	return fmt.Sprintf("%v", got) == fmt.Sprintf("%v", want)
}

func firstNodeID(r *memqlengine.ExecuteResult) string {
	if r == nil || r.Bundle == nil || len(r.Bundle.Nodes) == 0 {
		return ""
	}
	return r.Bundle.Nodes[0].Id
}
