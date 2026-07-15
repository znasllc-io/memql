package memql

import (
	"testing"

	"github.com/stretchr/testify/require"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// Tests for the unparenthesized-comparison arithmetic trap validator (#2542).
//
// `a - b > 0` parses as `a - (b > 0)` (the trailing bare identifier folds the
// comparison into the subtraction's right operand), so the arithmetic operand is
// a boolean -- always an author mistake. memqllint accepted it (parse + Init both
// pass) and the runtime failed with an opaque non-numeric-operand error. These
// tests pin the LOAD-time rejection (which the lint/boot-parity pass surfaces)
// for both the terminal-return and the intermediate-step positions, and pin the
// parenthesized / plain-arithmetic forms as clean.

func arithTrapRegistry() memoryNodes.Registry {
	return newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:lint:marker": {Name: "v1:lint:marker"},
	})
}

// FAIL-before / PASS-after: a terminal `return a - b > 0` is rejected at load
// with the parenthesise-the-arithmetic guidance.
func TestLogicArithmeticOperand_RejectsUnparenthesizedComparison_Return(t *testing.T) {
	src := `@description("returns an unparenthesized arithmetic-then-comparison")
logic ratioGate {
  args {
    a int @required
    b int @required
  }
  body {
    return args.a - args.b > 0
  }
}`
	_, err := tryParseNewFunctionSyntax("ratioGate", "logic", src, "test.memql", arithTrapRegistry())
	require.Error(t, err)
	require.Contains(t, err.Error(), "comparison")
	require.Contains(t, err.Error(), "(a - b) > 0")
	require.Contains(t, err.Error(), "2542")
}

// The same trap in an INTERMEDIATE `:=` step (which the loader stashes
// un-converted) is caught by the AutomationDef walk so it too fails at load.
func TestLogicArithmeticOperand_RejectsUnparenthesizedComparison_IntermediateStep(t *testing.T) {
	src := `@description("computes an unparenthesized comparison into an intermediate step")
logic ratioStepGate {
  args {
    a int @required
    b int @required
  }
  body {
    flag := args.a - args.b > 0
    return flag
  }
}`
	_, err := tryParseNewFunctionSyntax("ratioStepGate", "logic", src, "test.memql", arithTrapRegistry())
	require.Error(t, err)
	require.Contains(t, err.Error(), "ratioStepGate")
	require.Contains(t, err.Error(), "comparison")
	require.Contains(t, err.Error(), "2542")
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
  body {
    return (args.a - args.b) > 0
  }
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
  body {
    return args.a / args.b
  }
}`
	_, err := tryParseNewFunctionSyntax("ratioValue", "logic", src, "test.memql", arithTrapRegistry())
	require.NoError(t, err)
}
