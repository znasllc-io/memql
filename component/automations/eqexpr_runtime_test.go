package automations

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// #2612 (review finding 1): with no EqExpr case the string lowering path
// turned a nested equality predicate into "<<unsupported expression
// *ast.EqExpr>>", which the string evaluator resolved to FALSE: == pinned
// always-false, != always-true, silently. These pin the real path (the
// loader's compile -> RunLogicBody) on both operators and both branches.
func TestRunLogic_NestedCondCoalesceEquality(t *testing.T) {
	mkSrc := func(op string) string {
		pred := "(args.b ?? \"\") " + op + " \"y\""
		return `@description("nested ternary equality runtime probe")
logic condEqProbe {
  args {
    a string @required
    b string
  }
  z := args.b ?? ""
  return args.a == "x" ? (` + pred + ` ? "1" : "2") : "3"
}
`
	}

	type tc struct {
		op   string
		args map[string]any
		want string
	}
	cases := []tc{
		{"==", map[string]any{"a": "x", "b": "y"}, "1"},
		{"==", map[string]any{"a": "x", "b": "z"}, "2"},
		{"!=", map[string]any{"a": "x", "b": "y"}, "2"},
		{"!=", map[string]any{"a": "x", "b": "z"}, "1"},
		{"==", map[string]any{"a": "w", "b": "y"}, "3"},
	}
	for _, c := range cases {
		name := c.op + "/" + c.args["a"].(string) + "/" + c.args["b"].(string)
		t.Run(name, func(t *testing.T) {
			out, _ := runProjectionLogic(t, mkSrc(c.op), "condEqProbe", c.args)
			got, _ := out.(string)
			if got != c.want {
				t.Errorf("args=%v op=%s: got %q, want %q (a silent wrong branch is the finding-1 failure mode)", c.args, c.op, got, c.want)
			}
		})
	}
}

// memql#2655: an identifier-led comparison as a cond BRANCH value used to load
// green, and the string path returned the expression's own SOURCE TEXT as the
// branch value (`args.b=="y"`) -- a silent wrong answer. In edition 2026 a
// comparison is an expression like any other: as a branch value it is its
// boolean, with `??` binding tighter than `==` (D9), so
// `args.b ?? "" == "y"` is `(args.b ?? "") == "y"`.
func TestRunLogic_CondBranchValueComparisonIsItsBoolean(t *testing.T) {
	src := `@description("cond branch value runtime probe")
logic condBranchProbe {
  args {
    a string @required
    b string
  }
  z := args.b ?? ""
  return args.a == "x" ? (args.b ?? "" == "y") : "n"
}
`
	for b, want := range map[string]any{"y": true, "z": false} {
		_, steps := compiledLogic(t, src)
		r := NewLogicRunner(&memql.MemQLEngine{}, &recordingStepRegistry{}, nil)
		got, err := r.RunLogicBody(context.Background(), "condBranchProbe", steps, map[string]any{"a": "x", "b": b})
		if err != nil {
			t.Fatalf("b=%q: %v", b, err)
		}
		if got != want {
			t.Errorf("b=%q: branch value = %#v, want %#v (the comparison's boolean, never its source text)", b, got, want)
		}
	}
}
