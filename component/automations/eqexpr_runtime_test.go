package automations

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// #2612 (review finding 1): the converter fix opened the load door onto the
// STRING lowering path -- multi-step logic serializes via expressionToString
// and re-parses at runtime, and with no EqExpr case the predicate became
// "<<unsupported expression *ast.EqExpr>>", which EvaluateCondition resolves
// to FALSE: == pinned always-false, != always-true, silently. These pin the
// real path (parse -> compile -> re-parse -> RunLogic) on both operators and
// both branches, in both spellings.
func TestRunLogic_NestedCondCoalesceEquality(t *testing.T) {
	mkSrc := func(op, spelling string) string {
		pred := "coalesce(args.b, \"\") " + op + " \"y\""
		if spelling == "operator" {
			pred = "args.b ?? \"\" " + op + " \"y\""
		}
		return `@description("nested cond equality runtime probe")
logic condEqProbe {
  args {
    a string @required
    b string
  }
  body {
    z := coalesce(args.b, "")
    return cond(args.a == "x", cond(` + pred + `, "1", "2"), "3")
  }
}
`
	}

	type tc struct {
		op, spelling string
		args         map[string]any
		want         string
	}
	cases := []tc{
		{"==", "coalesce", map[string]any{"a": "x", "b": "y"}, "1"},
		{"==", "coalesce", map[string]any{"a": "x", "b": "z"}, "2"},
		{"==", "operator", map[string]any{"a": "x", "b": "y"}, "1"},
		{"==", "operator", map[string]any{"a": "x", "b": "z"}, "2"},
		{"!=", "coalesce", map[string]any{"a": "x", "b": "y"}, "2"},
		{"!=", "coalesce", map[string]any{"a": "x", "b": "z"}, "1"},
		{"!=", "operator", map[string]any{"a": "x", "b": "y"}, "2"},
		{"!=", "operator", map[string]any{"a": "x", "b": "z"}, "1"},
		{"==", "coalesce", map[string]any{"a": "w", "b": "y"}, "3"},
	}
	for _, c := range cases {
		name := c.op + "/" + c.spelling + "/" + c.args["b"].(string)
		t.Run(name, func(t *testing.T) {
			out, _ := runProjectionLogic(t, mkSrc(c.op, c.spelling), "condEqProbe", c.args)
			got, _ := out.(string)
			if got != c.want {
				t.Errorf("args=%v op=%s spelling=%s: got %q, want %q (a silent wrong branch is the finding-1 failure mode)", c.args, c.op, c.spelling, got, c.want)
			}
		})
	}
}


// memql#2655: an identifier-led comparison as a cond BRANCH value previously
// loaded green and the string path returned the expression's own SOURCE TEXT
// as the branch value (`args.b=="y"`) -- a silent wrong answer. That shape is
// now a LOAD rejection (validateLogicCondBranchValues, pinned at the loader
// altitude in component/memql). This test pins the runtime half of the
// contract: the expression-led sibling is UNCHANGED -- it loads and fails
// loudly at RunLogic with the arithmetic-operand error, never silently.
func TestRunLogic_CondBranchValueExpressionLedStaysLoud(t *testing.T) {
	src := `@description("cond branch value runtime probe")
logic condBranchProbe {
  args {
    a string @required
    b string
  }
  body {
    z := coalesce(args.b, "")
    return cond(args.a == "x", (coalesce(args.b, "") == "y"), "n")
  }
}
`
	body := parseLogicBody(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &recordingStepRegistry{}, nil)
	_, err := r.RunLogic(context.Background(), "condBranchProbe", body, map[string]any{"a": "x", "b": "y"})
	if err == nil {
		t.Fatal("expression-led comparison branch value must keep failing loudly at runtime")
	}
	if !strings.Contains(err.Error(), "arithmetic operand") {
		t.Fatalf("expected the existing arithmetic-operand error, got: %v", err)
	}
}
