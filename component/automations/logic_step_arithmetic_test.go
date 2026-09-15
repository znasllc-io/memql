package automations

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// logic_step_arithmetic_test.go pins #2542 GAP 2: infix arithmetic as an
// INTERMEDIATE statement's value (`doubled := base * 2`), not only a terminal
// return's. Operand parity with terminal-return arithmetic is by
// construction: both are expression statements, evaluated by EvalExpr.

// TestLogicRunner_IntermediateArithmeticStatement pins the end-to-end path: an
// intermediate `doubled := base * 2` statement binds the product and a later
// return reads it, with no call dispatched (arithmetic resolves locally).
func TestLogicRunner_IntermediateArithmeticStatement(t *testing.T) {
	src := `
@description("intermediate arithmetic statement (#2542 GAP 2)")
logic doubler {
  args {
    n int @required
  }
  base := args.n ?? 0
  doubled := base * 2
  return doubled
}
`
	_, bodySteps := compiledLogic(t, src)
	registry := &recordingStepRegistry{}
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)

	out, err := r.RunLogicBody(context.Background(), "doubler", bodySteps, map[string]any{"n": 21})
	if err != nil {
		t.Fatalf("RunLogicBody (#2542 GAP 2 arithmetic statement must evaluate): %v", err)
	}
	if !numericEquals(out, 42) {
		t.Errorf("return = %#v (%T), want 42 (21 * 2)", out, out)
	}
	if len(registry.dispatched) != 0 {
		t.Errorf("dispatched steps = %v, want none (coalesce + arithmetic resolve locally)", registry.dispatched)
	}
}

// TestLogicRunner_ArithmeticStatement_ArgsOperands pins that an arithmetic
// statement whose operands are caller args (`net := args.gross - args.fee`)
// resolves them, exactly as a terminal-return arithmetic does.
func TestLogicRunner_ArithmeticStatement_ArgsOperands(t *testing.T) {
	src := `
@description("args operands in an arithmetic statement (#2542 GAP 2)")
logic netAmount {
  args {
    gross int @required
    fee int @required
  }
  net := args.gross - args.fee
  return net
}
`
	_, bodySteps := compiledLogic(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &recordingStepRegistry{}, nil)

	out, err := r.RunLogicBody(context.Background(), "netAmount", bodySteps, map[string]any{"gross": 100, "fee": 30})
	if err != nil {
		t.Fatalf("RunLogicBody (args-operand arithmetic statement): %v", err)
	}
	if !numericEquals(out, 70) {
		t.Errorf("return = %#v (%T), want 70 (100 - 30)", out, out)
	}
}

// TestLogicRunner_ArithmeticStatement_DateBuiltinOperand pins operand PARITY
// with terminal returns: a date builtin nested in an arithmetic statement
// (`weeks := daysBetween(args.a, args.b) / 7`) evaluates as it does there.
func TestLogicRunner_ArithmeticStatement_DateBuiltinOperand(t *testing.T) {
	src := `
@description("date builtin inside an arithmetic statement (#2542 GAP 2)")
logic weeksBetween {
  args {
    a string @required
    b string @required
  }
  weeks := daysBetween(args.a, args.b) / 7
  return weeks
}
`
	_, bodySteps := compiledLogic(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &recordingStepRegistry{}, nil)

	out, err := r.RunLogicBody(context.Background(), "weeksBetween", bodySteps, map[string]any{
		"a": "2026-07-01",
		"b": "2026-07-15",
	})
	if err != nil {
		t.Fatalf("RunLogicBody (date-builtin arithmetic statement): %v", err)
	}
	if !numericEquals(out, 2) {
		t.Errorf("return = %#v (%T), want 2 (14 days / 7)", out, out)
	}
}

// TestLogicRunner_ArithmeticStatement_DivisionByZero pins that a
// division-by-zero in a statement surfaces the same clean error a terminal
// return does, never a panic or a silent nil bind.
func TestLogicRunner_ArithmeticStatement_DivisionByZero(t *testing.T) {
	src := `
@description("division by zero in an arithmetic statement (#2542 GAP 2)")
logic ratioStep {
  args {
    a int @required
    b int @required
  }
  x := args.a ?? 0
  y := args.b ?? 0
  q := x / y
  return q
}
`
	_, bodySteps := compiledLogic(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &recordingStepRegistry{}, nil)

	_, err := r.RunLogicBody(context.Background(), "ratioStep", bodySteps, map[string]any{"a": 10, "b": 0})
	if err == nil {
		t.Fatalf("RunLogicBody succeeded on an x / 0 statement; want a division-by-zero error")
	}
	if !strings.Contains(err.Error(), "division_by_zero") {
		t.Errorf("error = %q, want the division_by_zero refusal", err.Error())
	}
}

// TestLogicRunner_CompiledArithmeticStatementIsCanonicalSource pins that an
// intermediate arithmetic statement compiles to canonical v1 source (never an
// <<unsupported>> marker), its args operands carried as written -- the same
// shape a terminal return carries.
func TestLogicRunner_CompiledArithmeticStatementIsCanonicalSource(t *testing.T) {
	cases := []struct {
		name     string
		stmt     string
		binds    string
		wantExpr string
	}{
		{"statement_operand", "doubled := base * 2", "doubled", "base * 2"},
		{"args_operands", "net := args.gross - args.fee", "net", "args.gross - args.fee"},
		{"date_in_arithmetic", "weeks := daysBetween(args.gross, args.fee) / 7", "weeks", "daysBetween(args.gross, args.fee) / 7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, steps := compiledLogic(t, `
@description("statement round-trip probe")
logic probe {
  args {
    gross string @required
    fee string @required
  }
  base := args.gross ?? ""
  `+tc.stmt+`
  return base
}`)
			var got string
			for _, s := range steps {
				if s["binds"] == tc.binds && s["type"] == "expression" {
					got, _ = s["expression"].(string)
				}
			}
			if got == "" {
				t.Fatalf("no expression statement binding %q in the compiled body: %v", tc.binds, steps)
			}
			if strings.Contains(got, "<<unsupported") {
				t.Fatalf("%s = %q still carries the unsupported-expression marker", tc.binds, got)
			}
			if got != tc.wantExpr {
				t.Errorf("%s = %q, want %q", tc.binds, got, tc.wantExpr)
			}
		})
	}
}
