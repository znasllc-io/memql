package automations

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// logic_step_arithmetic_test.go pins #2542 GAP 2: infix arithmetic as an
// INTERMEDIATE step RHS (`doubled := base * 2`). Wave 1 (#2556) covered terminal
// returns only; this extends the logic-body statement grammar so an arithmetic
// expression is a valid step RHS, compiling through the same serialize/re-parse
// machinery (the compiler's ArithmeticExpr serializer + the LogicRunner's
// tryEvaluateArithmeticLocally). Operand parity with terminal-return arithmetic
// is by construction: same node, same serializer, same runtime evaluator.

// TestLogicRunner_RunLogic_IntermediateArithmeticStepRHS pins the end-to-end
// path: an intermediate `doubled := base * 2` step binds the product and a
// later return reads it, with no engine step dispatched (arithmetic resolves
// locally).
func TestLogicRunner_RunLogic_IntermediateArithmeticStepRHS(t *testing.T) {
	src := `@enabled
@description("intermediate arithmetic step RHS (#2542 GAP 2)")
logic doubler {
  args {
    n int @required
  }
  body {
    base := coalesce(args.n, 0)
    doubled := base * 2
    return doubled
  }
}
`
	body := parseLogicBody(t, src)
	registry := &recordingStepRegistry{}
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)

	out, err := r.RunLogic(context.Background(), "doubler", body, map[string]any{"n": 21})
	if err != nil {
		t.Fatalf("RunLogic (#2542 GAP 2 arithmetic step RHS must evaluate): %v", err)
	}
	if !numericEquals(out, 42) {
		t.Errorf("RunLogic return = %#v (%T), want 42 (21 * 2)", out, out)
	}
	if len(registry.dispatched) != 0 {
		t.Errorf("dispatched steps = %v, want none (coalesce + arithmetic resolve locally)", registry.dispatched)
	}
}

// TestLogicRunner_RunLogic_ArithmeticStepRHS_ArgsOperands pins that an
// arithmetic step RHS whose operands are caller args (`net := args.gross -
// args.fee`) resolves them -- the serializer's convertArgReferences rewrite
// applied to query steps is what makes the `$args.X` operands re-parse and
// resolve, exactly as a terminal-return arithmetic already does.
func TestLogicRunner_RunLogic_ArithmeticStepRHS_ArgsOperands(t *testing.T) {
	src := `@enabled
@description("args operands in an arithmetic step RHS (#2542 GAP 2)")
logic netAmount {
  args {
    gross int @required
    fee int @required
  }
  body {
    net := args.gross - args.fee
    return net
  }
}
`
	body := parseLogicBody(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &recordingStepRegistry{}, nil)

	out, err := r.RunLogic(context.Background(), "netAmount", body, map[string]any{"gross": 100, "fee": 30})
	if err != nil {
		t.Fatalf("RunLogic (args-operand arithmetic step): %v", err)
	}
	if !numericEquals(out, 70) {
		t.Errorf("RunLogic return = %#v (%T), want 70 (100 - 30)", out, out)
	}
}

// TestLogicRunner_RunLogic_ArithmeticStepRHS_DateBuiltinOperand pins operand
// PARITY with terminal returns: a date builtin nested in an arithmetic step RHS
// (`weeks := daysBetween(args.a, args.b) / 7`) evaluates through the same
// operand walker. This RHS leads with a call, so it is rescued inside the
// bareCall branch rather than the leading-operand speculative branch.
func TestLogicRunner_RunLogic_ArithmeticStepRHS_DateBuiltinOperand(t *testing.T) {
	src := `@enabled
@description("date builtin inside an arithmetic step RHS (#2542 GAP 2)")
logic weeksBetween {
  args {
    a string @required
    b string @required
  }
  body {
    weeks := daysBetween(args.a, args.b) / 7
    return weeks
  }
}
`
	body := parseLogicBody(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &recordingStepRegistry{}, nil)

	out, err := r.RunLogic(context.Background(), "weeksBetween", body, map[string]any{
		"a": "2026-07-01",
		"b": "2026-07-15",
	})
	if err != nil {
		t.Fatalf("RunLogic (date-builtin arithmetic step): %v", err)
	}
	if !numericEquals(out, 2) {
		t.Errorf("RunLogic return = %#v (%T), want 2 (14 days / 7)", out, out)
	}
}

// TestLogicRunner_RunLogic_ArithmeticStepRHS_DivisionByZero pins that a
// division-by-zero in a step RHS surfaces the same clean error a terminal
// return does, never a panic or a silent nil bind.
func TestLogicRunner_RunLogic_ArithmeticStepRHS_DivisionByZero(t *testing.T) {
	src := `@enabled
@description("division by zero in an arithmetic step RHS (#2542 GAP 2)")
logic ratioStep {
  args {
    a int @required
    b int @required
  }
  body {
    x := coalesce(args.a, 0)
    y := coalesce(args.b, 0)
    q := x / y
    return q
  }
}
`
	body := parseLogicBody(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &recordingStepRegistry{}, nil)

	_, err := r.RunLogic(context.Background(), "ratioStep", body, map[string]any{"a": 10, "b": 0})
	if err == nil {
		t.Fatalf("RunLogic succeeded on x / 0 step RHS; want a division-by-zero error")
	}
	if !strings.Contains(err.Error(), "division by zero") {
		t.Errorf("error = %q, want it to name division by zero", err.Error())
	}
}

// TestLogicRunner_CompileRoundTrip_ArithmeticStepRHS pins that an intermediate
// arithmetic step compiles to the parenthesized operator form (never the
// <<unsupported>> marker), with args operands rewritten to the $args form the
// runtime re-parses -- the same serialized shape a terminal return carries.
func TestLogicRunner_CompileRoundTrip_ArithmeticStepRHS(t *testing.T) {
	cases := []struct {
		name      string
		stepLine  string
		stepID    string
		wantQuery string
	}{
		{"step_operand", "doubled := base * 2", "doubled", "(base * 2)"},
		{"args_operands", "net := args.gross - args.fee", "net", "($args.gross - $args.fee)"},
		{"date_in_arithmetic", "weeks := daysBetween(args.gross, args.fee) / 7", "weeks", "(daysBetween($args.gross, $args.fee) / 7)"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			src := `@enabled
@description("step-RHS round-trip probe")
logic probe {
  args {
    gross string @required
    fee string @required
  }
  body {
    base := coalesce(args.gross, "")
    ` + tc.stepLine + `
    return base
  }
}
`
			body := parseLogicBody(t, src)
			r := NewLogicRunner(nil, nil, nil)
			auto, err := r.compileBodyToAutomation("probe", body)
			if err != nil {
				t.Fatalf("compileBodyToAutomation: %v", err)
			}
			var step *Step
			for _, s := range auto.Steps {
				if s != nil && s.ID == tc.stepID {
					step = s
				}
			}
			if step == nil || step.Query == nil {
				t.Fatalf("no %q query step in compiled automation", tc.stepID)
			}
			got := strings.TrimSpace(step.Query.Query)
			if strings.Contains(got, "<<unsupported") {
				t.Fatalf("step %q = %q still carries the unsupported-expression marker", tc.stepID, got)
			}
			if got != tc.wantQuery {
				t.Errorf("step %q query = %q, want %q", tc.stepID, got, tc.wantQuery)
			}
		})
	}
}
