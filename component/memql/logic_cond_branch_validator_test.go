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
		"lower-wrapped":    `cond(args.a == "x", lower(cond(z == "q", args.b == "y", "m")), "n")`,
		"tostring-wrapped": `cond(args.a == "x", toString(cond(z == "q", args.b == "y", "m")), "n")`,
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
			if !strings.Contains(err.Error(), "VALUE position") {
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
	if !strings.Contains(err.Error(), "VALUE position") {
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
			if err == nil || !strings.Contains(err.Error(), "VALUE position") {
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

// TestLogicValuePosition_AdjacentSilentPositions is the #2693 extension:
// two value positions adjacent to the #2655 cond-branch position carried the
// same silent-wrong class. A comparison as a coalesce/concat ARG returned its
// source text on the multi-step string path; a bare/terminal comparison return
// mis-routed as a store query. Both load green before, reject loudly after.
func TestLogicValuePosition_AdjacentSilentPositions(t *testing.T) {
	reject := map[string]string{
		// P1: comparison laundered through a value builtin's args at a
		// top-level value position (not inside a cond branch, which #2655
		// already covered).
		"coalesce-arg comparison": `coalesce(args.b == "y", "n")`,
		"concat-arg comparison":   `concat(args.b == "y", "-suffix")`,
		// P2: the bare/terminal identifier-led comparison return.
		"bare terminal comparison": `args.b == "y"`,
	}
	for name, ret := range reject {
		err := loadCondBranchProbe(t, ret)
		if err == nil {
			t.Errorf("%s: must be rejected at load (#2693); it loaded green -- the silent-value class is open", name)
			continue
		}
		if !strings.Contains(err.Error(), "VALUE position") {
			t.Errorf("%s: rejected for the wrong reason: %v", name, err)
		}
	}
}

// TestLogicValuePosition_ExemptionsHold pins the boundary the #2693 review
// flagged as load-bearing: the widened gate must NOT reject any of the legal
// shapes -- boolean-combinator returns (a comparison operand of && / || is a
// legal boolean-returning logic, dsl/cluster/logic.memql), cond predicates,
// lambda bodies, expression-led comparisons, non-comparison value builtins,
// and real named-query calls.
func TestLogicValuePosition_ExemptionsHold(t *testing.T) {
	legal := map[string]string{
		"boolean AND return":     `z.empty() && args.b == "bff"`,
		"boolean OR return":      `args.b == "x" || args.b == "y"`,
		"cond predicate":         `cond(args.b == "y", "1", "2")`,
		"expression-led loud":    `(coalesce(args.b, "") == "y")`,
		"lambda-body comparison": `args.b.split(",").where(m => cond(m == "a", m == "b", false)).count()`,
		"coalesce non-compare":   `coalesce(args.b, "default")`,
		"concat non-compare":     `concat(args.a, "-", args.b)`,
		"named query call":       `query listThings()`,
		"plain literal":          `"just a string"`,
	}
	for name, ret := range legal {
		if err := loadCondBranchProbe(t, ret); err != nil {
			t.Errorf("%s: wrongly rejected -- a legal shape must still load (#2693): %v", name, err)
		}
	}
}

// TestLogicValuePosition_BooleanOperandKeepsWalking pins that the boolean-
// combinator arms RESET the mode but do NOT stop: a comparison that is a direct
// `&&`/`!` operand is legal, but a genuine cond-BRANCH violation nested under a
// boolean operand is still caught. Deleting the recursion (returning nil at the
// combinator) would silently pass the nested violation.
func TestLogicValuePosition_BooleanOperandKeepsWalking(t *testing.T) {
	if err := loadCondBranchProbe(t, `z.empty() && args.b == "x"`); err != nil {
		t.Errorf("direct && operand comparison must load: %v", err)
	}
	for name, ret := range map[string]string{
		"cond under &&": `z.empty() && cond(args.a == "b", args.c == "d", "e")`,
		"cond under !":  `!(cond(args.a == "b", args.c == "d", "e"))`,
	} {
		if err := loadCondBranchProbe(t, ret); err == nil {
			t.Errorf("%s: a cond-branch violation nested under a boolean operand must still be rejected", name)
		}
	}
}
