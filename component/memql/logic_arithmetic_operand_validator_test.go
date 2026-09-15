package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// Tests for the unparenthesized-comparison arithmetic trap (#2542).
//
// Before edition 2026, `a - b > 0` parsed as `a - (b > 0)` (the trailing bare
// identifier folded the comparison into the subtraction's right operand), so
// the arithmetic operand was a boolean -- always an author mistake -- and the
// loader refused it for both the terminal-return and the intermediate-step
// positions. Edition 2026 has one precedence table (rule 11c): arithmetic
// binds tighter than comparison, so the same text is `(a - b) > 0`. These tests
// pin that the unparenthesized form loads in both positions and means what it
// reads as, and that the parenthesized / plain-arithmetic forms stay clean.

func arithTrapRegistry() memoryNodes.Registry {
	return newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:lint:marker": {Name: "v1:lint:marker"},
	})
}

// A terminal `return a - b > 0` loads, and answers `(a - b) > 0`.
func TestLogicArithmeticOperand_UnparenthesizedComparisonReadsByPrecedence_Return(t *testing.T) {
	src := `@description("returns an unparenthesized arithmetic-then-comparison")
logic ratioGate {
  args {
    a int @required
    b int @required
  }
  return args.a - args.b > 0
}`
	fn, err := tryParseNewFunctionSyntax("ratioGate", "logic", src, "test.memql", arithTrapRegistry())
	require.NoError(t, err)
	ret := statementReturnExpr(t, fn)
	for _, tc := range []struct {
		a, b int64
		want bool
	}{{5, 3, true}, {3, 5, false}, {3, 3, false}} {
		got, err := EvalExpr(context.Background(), ret, MapScope{"args": map[string]any{"a": tc.a, "b": tc.b}}, EvalOptions{})
		require.NoError(t, err)
		require.Equalf(t, tc.want, got, "a=%d b=%d: `args.a - args.b > 0` is `(args.a - args.b) > 0`", tc.a, tc.b)
	}
}

// The same text in an INTERMEDIATE `:=` step loads too: the step's right-hand
// side is the one grammar every position reads.
func TestLogicArithmeticOperand_UnparenthesizedComparisonReadsByPrecedence_IntermediateStep(t *testing.T) {
	src := `@description("computes an unparenthesized comparison into an intermediate step")
logic ratioStepGate {
  args {
    a int @required
    b int @required
  }
  flag := args.a - args.b > 0
  return flag
}`
	fn, err := tryParseNewFunctionSyntax("ratioStepGate", "logic", src, "test.memql", arithTrapRegistry())
	require.NoError(t, err)
	require.NotNil(t, fn.LogicBody, "the body loads as statements")
}

// The PARENTHESIZED form is the working idiom (an expression-led
// BinaryComparisonExpr over an arithmetic left operand) and loads cleanly.
func TestLogicArithmeticOperand_AcceptsParenthesizedComparison(t *testing.T) {
	src := `@description("parenthesized arithmetic then comparison -- the working idiom")
logic ratioGateOK {
  args {
    a int @required
    b int @required
  }
  return (args.a - args.b) > 0
}`
	_, err := tryParseNewFunctionSyntax("ratioGateOK", "logic", src, "test.memql", arithTrapRegistry())
	require.NoError(t, err)
}

// Plain arithmetic with no comparison operand (the #2542 item-1 ratio) is
// unaffected -- the validator only fires on a comparison operand.
func TestLogicArithmeticOperand_AcceptsPlainArithmetic(t *testing.T) {
	src := `@description("plain arithmetic ratio return")
logic ratioValue {
  args {
    a int @required
    b int @required
  }
  return args.a / args.b
}`
	_, err := tryParseNewFunctionSyntax("ratioValue", "logic", src, "test.memql", arithTrapRegistry())
	require.NoError(t, err)
}
