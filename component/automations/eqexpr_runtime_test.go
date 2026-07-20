package automations

import "testing"

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
