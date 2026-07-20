package memql

// logic_cond_branch_validator_test.go -- memql#2655: an identifier-led
// comparison as a cond BRANCH value must fail LOUDLY at load. The docs
// (authoring-rules) declare the shape invalid, but it previously loaded green
// and the multi-step string path returned the expression's own SOURCE TEXT
// (`args.b=="y"`) as the branch value -- a silent wrong answer -- while the
// engine path mis-routed it as a store query. The expression-led sibling
// (parenthesized, BinaryComparisonExpr) keeps its existing behavior: it loads
// and fails loudly at runtime.

import (
	"strings"
	"testing"
)

func loadCondBranchProbe(t *testing.T, ret string) error {
	t.Helper()
	src := strings.Join([]string{
		"@description(\"cond branch value probe\")",
		"logic condBranchValProbe {",
		"  args {",
		"    a string @required",
		"    b string",
		"  }",
		"  body {",
		"    z := coalesce(args.b, \"\")",
		"    return " + ret,
		"  }",
		"}",
	}, "\n")
	_, err := tryParseNewFunctionSyntax("condBranchValProbe", "logic", src, "common.logic.memql", dotAccessLoadRegistry())
	return err
}

func TestLogicCondBranchValue_IdentifierLedComparisonRejectedAtLoad(t *testing.T) {
	for name, ret := range map[string]string{
		"then-branch":  `cond(args.a == "x", args.b == "y", "n")`,
		"else-branch":  `cond(args.a == "x", "y", args.b == "n")`,
		"nested-then":  `cond(args.a == "x", cond(z == "q", args.b == "y", "m"), "n")`,
		"coalesce-arg": `coalesce(cond(args.a == "x", args.b == "y", "n"), "")`,
	} {
		t.Run(name, func(t *testing.T) {
			err := loadCondBranchProbe(t, ret)
			if err == nil {
				t.Fatal("identifier-led comparison as a cond BRANCH value must be rejected at load (previously returned its own source text silently)")
			}
			if !strings.Contains(err.Error(), "BRANCH value") {
				t.Errorf("rejection should name the branch-value rule, got: %v", err)
			}
		})
	}
}

func TestLogicCondBranchValue_ValidShapesStillLoad(t *testing.T) {
	for name, ret := range map[string]string{
		// The PREDICATE position keeps accepting comparisons.
		"comparison-predicate": `cond(args.a == "x", "y", "n")`,
		// A nested cond as a branch VALUE is fine.
		"nested-cond-branch": `cond(args.a == "x", cond(z == "q", "1", "2"), "n")`,
		// The expression-led sibling keeps loading (its loudness is the
		// existing runtime arithmetic-operand error -- behavior unchanged).
		"expression-led-branch": `cond(args.a == "x", (coalesce(args.b, "") == "y"), "n")`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := loadCondBranchProbe(t, ret); err != nil {
				t.Fatalf("%s must keep loading: %v", name, err)
			}
		})
	}
}
