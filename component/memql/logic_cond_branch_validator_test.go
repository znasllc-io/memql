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
		// Wrapper builtins must not launder a nested violation.
		"lower-wrapped":     `cond(args.a == "x", lower(cond(z == "q", args.b == "y", "m")), "n")`,
		"tostring-wrapped":  `cond(args.a == "x", toString(cond(z == "q", args.b == "y", "m")), "n")`,
		// A nested cond inside the PREDICATE still carries branch positions.
		"nested-predicate": `cond(cond(z == "q", true, args.b == "y"), "1", "2")`,
		// A projection object literal carrying the cond is still a value walk.
		"object-literal": `{v: cond(args.a == "x", args.b == "y", "n")}`,
		// A comparison INSIDE a wrapper as the branch value is the same
		// silent source-text return one wrapper deep -- the walk carries
		// branch context through containers, not just direct children.
		"lower-arg-comparison":    `cond(args.a == "x", lower(args.b == "y"), "n")`,
		"coalesce-arg-comparison": `cond(args.a == "x", coalesce(args.b == "y", ""), "n")`,
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
		// Lambda bodies are a DIFFERENT, correct evaluation context: the
		// in-memory collection evaluator handles a Field-led comparison as
		// a cond branch value fine (docs matrix: "yes (lambda body)"), so
		// the walk must stop at the lambda boundary.
		"lambda-body-cond-branch": `args.b.split(",").where(m => cond(m == "a", m == "b", false)).count()`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := loadCondBranchProbe(t, ret); err != nil {
				t.Fatalf("%s must keep loading: %v", name, err)
			}
		})
	}
}

// The bind-then-return idiom is the SAME multi-step string path as the
// terminal return -- the gate must not be one refactor away from bypassed.
func TestLogicCondBranchValue_AssignmentStepRHSRejected(t *testing.T) {
	src := strings.Join([]string{
		"@description(\"cond branch value assignment probe\")",
		"logic condBranchAssignProbe {",
		"  args {",
		"    a string @required",
		"    b string",
		"  }",
		"  body {",
		"    w := cond(args.a == \"x\", args.b == \"y\", \"n\")",
		"    return w",
		"  }",
		"}",
	}, "\n")
	_, err := tryParseNewFunctionSyntax("condBranchAssignProbe", "logic", src, "common.logic.memql", dotAccessLoadRegistry())
	if err == nil {
		t.Fatal("cond with an identifier-led comparison branch as a step RHS must be rejected at load (same silent source-text return as the terminal form)")
	}
	if !strings.Contains(err.Error(), "BRANCH value") {
		t.Errorf("rejection should name the branch-value rule, got: %v", err)
	}
}

// The FunctionStepConfig feed is the defense for assignment shapes the
// flattened cond-step arm cannot see (a cond nested under ANOTHER wrapper,
// e.g. coalesce-wrapped -- the flattened arm covers cond-rooted trees,
// including nested conds) -- pin it so deleting the arm fails loudly.
func TestLogicCondBranchValue_NestedAssignmentShapesRejected(t *testing.T) {
	for name, rhs := range map[string]string{
		"coalesce-wrapped": `coalesce(cond(args.a == "x", args.b == "y", "n"), "")`,
		"nested-cond":      `cond(args.a == "x", cond(z == "q", args.b == "y", "m"), "n")`,
	} {
		t.Run(name, func(t *testing.T) {
			src := strings.Join([]string{
				"@description(\"nested assignment probe\")",
				"logic condBranchNestedAssignProbe {",
				"  args {",
				"    a string @required",
				"    b string",
				"  }",
				"  body {",
				"    z := coalesce(args.b, \"\")",
				"    w := " + rhs,
				"    return w",
				"  }",
				"}",
			}, "\n")
			_, err := tryParseNewFunctionSyntax("condBranchNestedAssignProbe", "logic", src, "common.logic.memql", dotAccessLoadRegistry())
			if err == nil || !strings.Contains(err.Error(), "BRANCH value") {
				t.Fatalf("nested/wrapped cond assignment must be rejected via the step-args feed, got: %v", err)
			}
		})
	}
}

// The step-args feed also carries the #2542 arithmetic gate into function-step
// args: an always-wrong `a - b > 0` trap inside a lambda passed through a cond
// predicate arg is now a load rejection instead of an opaque runtime failure.
func TestLogicStepArgs_ArithmeticTrapRejected(t *testing.T) {
	src := strings.Join([]string{
		"@description(\"arith trap in step args probe\")",
		"logic arithStepArgsProbe {",
		"  args {",
		"    b string",
		"  }",
		"  body {",
		"    w := cond(args.b.split(\",\").where(m => m.len - m.cap > 0).any(), \"y\", \"n\")",
		"    return w",
		"  }",
		"}",
	}, "\n")
	_, err := tryParseNewFunctionSyntax("arithStepArgsProbe", "logic", src, "common.logic.memql", dotAccessLoadRegistry())
	if err == nil || !strings.Contains(err.Error(), "arithmetic operand is a comparison") {
		t.Fatalf("the #2542 trap inside function-step args must be rejected at load, got: %v", err)
	}
}
