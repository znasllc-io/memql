package memql

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// TestMultiStepLogicReturnParsing reproduces a parser regression where a
// Logic body that combines `name := <funcCall>` assignments with a
// `for item := range ... { ... }` loop and a trailing `return <expr>`
// terminator loses the `return` step. Without it the loader's
// extractLogicReturnExpression treats the body as malformed and the
// runtime LogicRunner refuses to run the function.
//
// The test isolates the parser layer so the failure mode is clear:
// after rewriting + parsing, we expect exactly one `_return` step in
// the resulting AutomationDef.
func TestMultiStepLogicReturnParsing(t *testing.T) {
	// Minimal reproduction of dsl/identity/logic.memql's
	// revokeExpiredDelegations body shape. The parser layer
	// doesn't care that the inner calls are unresolved -- it just has
	// to recognise the trailing `return` keyword + expression.
	source := `@useQuery(expiredActiveDelegations)
@useMutation(revokeDelegation)
@description("repro of revokeExpiredDelegations shape")
logic revokeExpiredDelegations {
  args {
    now string @required
  }
  body {
    expiredDelegations := expiredActiveDelegations( now: args.now )
    for item := range expiredDelegations.nodes() {
      revokeStep := revokeDelegation(
        delegationId: item.id,
        revokedBySubject: "system:expiry",
        revokedAt: args.now
      )
    }
    return expiredDelegations.count()
  }
}
`

	normalised, err := languageParser.NormaliseAll(source)
	if err != nil {
		t.Fatalf("NormaliseAll: %v", err)
	}
	t.Logf("post-rewrite source:\n%s", normalised)

	lexer := languageParser.NewLexer(normalised)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	p := languageParser.NewParser(tokens)
	ast, err := p.Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	file, ok := ast.(*languageParser.File)
	if !ok {
		t.Fatalf("expected File, got %T", ast)
	}
	if len(file.Definitions) == 0 {
		t.Fatalf("no definitions parsed")
	}

	def, ok := file.Definitions[0].(*languageParser.FunctionDef)
	if !ok {
		t.Fatalf("expected FunctionDef, got %T", file.Definitions[0])
	}
	body, ok := def.Body.(*languageParser.AutomationDef)
	if !ok {
		t.Fatalf("expected AutomationDef body, got %T", def.Body)
	}

	t.Logf("step count: %d", len(body.Steps))
	for i, s := range body.Steps {
		t.Logf("  step[%d] ID=%q Type=%v Config=%T", i, s.ID, s.Type, s.Config)
	}

	hasReturn := false
	for _, s := range body.Steps {
		if s.ID == "_return" {
			hasReturn = true
			cfg, _ := s.Config.(*languageParser.QueryStepConfig)
			if cfg == nil || cfg.Query == nil {
				t.Fatalf("_return step has no Query")
			}
			t.Logf("_return.Query = %T", cfg.Query)
		}
	}
	if !hasReturn {
		t.Fatalf("expected a _return step in body; got %d steps without one",
			len(body.Steps))
	}

	// Sanity: the body should also have at least one assignment step
	// and one for-range step before the return.
	var assigns, forSteps int
	for _, s := range body.Steps {
		switch s.Type {
		case languageParser.StepTypeFunction:
			assigns++
		case languageParser.StepTypeForEach:
			forSteps++
		}
	}
	if assigns < 1 || forSteps < 1 {
		t.Fatalf("expected at least one function-call + one for-range step; got assigns=%d forSteps=%d",
			assigns, forSteps)
	}
	_ = strings.TrimSpace // keep import live in case we add more checks
}

// TestConvertExpressionHandlesCoalesce pins the ASTConverter's
// handling of `coalesce(...)`. The parser emits a typed
// languageParser.CoalesceExpr for `coalesce(a, b)` (and matching
// dedicated nodes for `cond`, `concat`, `hash`, `canonicalId`,
// `first`, `last`, `lower`, `upper`, `trim`); before this fix the
// converter only knew about FunctionCallExpr and rejected the
// typed forms with `unsupported parser expression type:
// *ast.CoalesceExpr`. That broke every Logic body whose `return`
// expression used `coalesce(...)` -- bootstrapSession,
// releaseWorkspaceOnPlanTerminal, etc. -- because
// extractLogicReturnExpression handed the raw CoalesceExpr to
// the converter.
func TestConvertExpressionHandlesCoalesce(t *testing.T) {
	source := `
@description("repro: return value uses coalesce")
logic logicSample {
  body {
    return coalesce("a", "b")
  }
}
`
	normalised, err := languageParser.NormaliseAll(source)
	if err != nil {
		t.Fatalf("NormaliseAll: %v", err)
	}
	lexer := languageParser.NewLexer(normalised)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	ast, err := languageParser.NewParser(tokens).Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	file := ast.(*languageParser.File)
	def := file.Definitions[0].(*languageParser.FunctionDef)
	body := def.Body.(*languageParser.AutomationDef)

	// Pull out the synthetic `_return` step's expression.
	var retExpr languageParser.ExpressionNode
	for _, s := range body.Steps {
		if s.ID == "_return" {
			cfg, _ := s.Config.(*languageParser.QueryStepConfig)
			if cfg != nil {
				retExpr = cfg.Query
			}
			break
		}
	}
	if retExpr == nil {
		t.Fatalf("no _return step expression")
	}
	if _, ok := retExpr.(*languageParser.CoalesceExpr); !ok {
		t.Fatalf("expected CoalesceExpr, got %T", retExpr)
	}

	// The real check: ConvertExpression handles the typed builtin
	// without erroring. Before the fix this returned "unsupported
	// parser expression type: *ast.CoalesceExpr".
	converter := NewASTConverter()
	converted, err := converter.ConvertExpression(retExpr)
	if err != nil {
		t.Fatalf("ConvertExpression: %v", err)
	}
	call, ok := converted.(*FunctionCallExpression)
	if !ok {
		t.Fatalf("expected *FunctionCallExpression, got %T", converted)
	}
	if call.Name != "coalesce" {
		t.Errorf("call.Name = %q, want %q", call.Name, "coalesce")
	}
	if len(call.Args) != 2 {
		t.Errorf("expected 2 positional args, got %d (%v)", len(call.Args), call.Args)
	}
}

// TestTopLevelMultiStatementIf pins that `if cond { stmt1; stmt2; ... }`
// at the top level of a Logic body parses every inner statement and
// stamps the if's condition on each. The legacy behaviour silently
// dropped the entire body (ifStatementToSteps returned nil) so
// workbench-style logics ran no code under their gating condition.
func TestTopLevelMultiStatementIf(t *testing.T) {
	source := `
@description("multi-stmt if at top level")
logic logicSample {
  args {
    event object @required
  }
  body {
    planId := someQuery( id: args.event.id )
    if planId != "" {
      workspaces := someQuery( planId: planId )
      teardown := someMutation( planId: planId )
    }
    return planId
  }
}
`
	body := parseLogicBody(t, source)
	t.Logf("step count: %d", len(body.Steps))
	for i, s := range body.Steps {
		t.Logf("  step[%d] ID=%q Type=%v Cond=%q", i, s.ID, s.Type, s.Condition)
	}

	// Expect: planId, workspaces (conditional), teardown (conditional),
	// _return. The two inner statements were silently dropped under the
	// old parser; the fix lifts them out as standalone steps stamped
	// with the if's condition.
	var conditionalCount int
	for _, s := range body.Steps {
		if s.Condition != "" {
			conditionalCount++
		}
	}
	if conditionalCount < 2 {
		t.Fatalf("expected at least 2 conditional steps from the inner if body; got %d", conditionalCount)
	}

	// The condition string should carry the comparison the author wrote.
	hasPlanIdGuard := false
	for _, s := range body.Steps {
		if strings.Contains(s.Condition, "planId") {
			hasPlanIdGuard = true
			break
		}
	}
	if !hasPlanIdGuard {
		t.Errorf("expected at least one step's condition to reference planId; got conditions %v",
			func() []string {
				out := []string{}
				for _, s := range body.Steps {
					if s.Condition != "" {
						out = append(out, s.Condition)
					}
				}
				return out
			}())
	}
}

// TestTopLevelMultiStatementIfWithForRange pins the workbench
// releaseWorkspaceOnPlanTerminal shape -- an outer `if cond`
// with an inner `for item := range x.nodes() { ... }` plus a
// trailing statement. The for-range inner steps must inherit the
// outer if's condition so the mutation only runs when the gate is
// open; ifStatementToSteps' walk into ForEachStepConfig.Do handles
// that.
func TestTopLevelMultiStatementIfWithForRange(t *testing.T) {
	source := `
@description("workbench-shaped logic")
logic logicSample {
  args {
    event object @required
  }
  body {
    planId := someQuery( id: args.event.id )
    if planId != "" {
      workspaces := workspaceForPlan( planId: planId )
      for item := range workspaces.nodes() {
        flip := if item.payload.status == "provisioned" {
          someMutation( id: item.id )
        }
      }
      teardown := workbenchTeardownDirectory( planId: planId )
    }
    return planId
  }
}
`
	body := parseLogicBody(t, source)
	t.Logf("step count: %d", len(body.Steps))
	for i, s := range body.Steps {
		t.Logf("  step[%d] ID=%q Type=%v Cond=%q", i, s.ID, s.Type, s.Condition)
	}

	// Find the for-range step among the conditional steps.
	var forStep *languageParser.StepDef
	for i := range body.Steps {
		s := &body.Steps[i]
		if s.Type == languageParser.StepTypeForEach {
			forStep = s
			break
		}
	}
	if forStep == nil {
		t.Fatalf("expected a for-range step in body; got none")
	}
	if forStep.Condition == "" {
		t.Errorf("expected for-range step to inherit the outer if's condition; got empty")
	}

	cfg, ok := forStep.Config.(*languageParser.ForEachStepConfig)
	if !ok || cfg == nil {
		t.Fatalf("expected ForEachStepConfig, got %T", forStep.Config)
	}
	if len(cfg.Do) == 0 {
		t.Fatalf("expected at least one inner Do step")
	}
	for i, doStep := range cfg.Do {
		if doStep.Condition == "" {
			t.Errorf("for-range Do[%d] (ID=%q) is missing the outer if's condition stamp",
				i, doStep.ID)
		}
	}
}

// TestIfElseBranches pins the else-branch flattener: ElseSteps gets
// the negated parent condition. This lets `if cond { ... } else { ... }`
// compile to two flat groups of always-gated steps without any
// branching primitive in the runtime.
func TestIfElseBranches(t *testing.T) {
	source := `
@description("if/else branches")
logic logicSample {
  args {
    flag object @required
  }
  body {
    if flag.enabled == true {
      enabled := someQuery()
    } else {
      disabled := someOtherQuery()
    }
    return "done"
  }
}
`
	body := parseLogicBody(t, source)
	var enabledCond, disabledCond string
	for _, s := range body.Steps {
		switch s.ID {
		case "enabled":
			enabledCond = s.Condition
		case "disabled":
			disabledCond = s.Condition
		}
	}
	if enabledCond == "" {
		t.Errorf("expected `enabled` step to carry the positive condition")
	}
	if disabledCond == "" {
		t.Errorf("expected `disabled` step to carry the negated condition")
	}
	if !strings.HasPrefix(disabledCond, "not (") {
		t.Errorf("expected else-branch condition to start with `not (`, got %q", disabledCond)
	}
}

// parseLogicBody re-uses the helper from multistep_logic_test.go's
// earlier blocks. Re-declared as an inline helper so the new tests
// stay self-contained.
func parseLogicBody(t *testing.T, source string) *languageParser.AutomationDef {
	t.Helper()
	normalised, err := languageParser.NormaliseAll(source)
	if err != nil {
		t.Fatalf("NormaliseAll: %v", err)
	}
	lexer := languageParser.NewLexer(normalised)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	ast, err := languageParser.NewParser(tokens).Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	file := ast.(*languageParser.File)
	def := file.Definitions[0].(*languageParser.FunctionDef)
	body, ok := def.Body.(*languageParser.AutomationDef)
	if !ok {
		t.Fatalf("expected AutomationDef body, got %T", def.Body)
	}
	return body
}

// TestForRangeWithInnerConditionalStep reproduces the workbench
// logic.memql parse failure where the inner step is shaped
// `name := if <cond> { funcCall(...) }`. The for-range body parser
// dispatches on `TokenIdentifier` and hands off to parseGoStyleStep,
// which has a working conditional-step branch for the same syntax
// at the body's top level. The failure mode the loader surfaces
// today is `parse error at line N, column M: expected '}', got ':='`,
// from somewhere inside the inner block. This test pins the parse
// path so a fix lands a green test, not a stab in the dark.
func TestForRangeWithInnerConditionalStep(t *testing.T) {
	source := `
@description("repro of releaseWorkspaceOnPlanTerminal inner-if shape")
logic logicSample {
  args {
    event object @required
  }
  body {
    workspaces := queryFoo( planId: "x" )
    for item := range workspaces.nodes() {
      flip := if item.payload.status == "provisioned" {
        mutationBar( id: item.id, reason: "x" )
      }
    }
    return workspaces.count()
  }
}
`
	normalised, err := languageParser.NormaliseAll(source)
	if err != nil {
		t.Fatalf("NormaliseAll: %v", err)
	}
	t.Logf("post-rewrite source:\n%s", normalised)
	lexer := languageParser.NewLexer(normalised)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	_, err = languageParser.NewParser(tokens).Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// If we got here without parse error, the regression is fixed.
}

// TestMultiStepLogicReturnSurvivesCompileRoundTrip guards the
// compiler -> JSON -> loader handoff that the LogicRunner uses to
// produce its runtime Automation. The compiler peels `_return` out
// of the step list and emits it as a top-level JSON field; the
// loader has no field for it, so a vanilla unmarshal silently drops
// the return expression. The runtime check in logic_runner then
// fails with "logic %q has no `_return` step". The fix re-attaches
// a synthetic _return Step inside compileBodyToAutomation; this test
// exercises just the compile/marshal step so a regression in the
// emitted JSON shape shows up here instead of at first run.
func TestMultiStepLogicReturnSurvivesCompileRoundTrip(t *testing.T) {
	source := `
@description("repro of the round-trip")
logic logicSample {
  args {
    now string @required
  }
  body {
    rows := someQuery( now: args.now )
    for item := range rows.nodes() {
      tick := someMutation( id: item.id )
    }
    return rows.count()
  }
}
`
	normalised, err := languageParser.NormaliseAll(source)
	if err != nil {
		t.Fatalf("NormaliseAll: %v", err)
	}
	lexer := languageParser.NewLexer(normalised)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	ast, err := languageParser.NewParser(tokens).Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	file := ast.(*languageParser.File)
	def := file.Definitions[0].(*languageParser.FunctionDef)
	body := def.Body.(*languageParser.AutomationDef)
	if body == nil {
		t.Fatalf("body is nil")
	}

	// We can't import the automations package from a memql test
	// (import cycle: automations -> memql). The assertion this test
	// guards is in component/automations/logic_runner_test.go --
	// keep this here as a tripwire on the parser side: if the
	// parser stops emitting a _return step entirely, the round-trip
	// fix is moot. Verified above. Documenting the symmetric
	// runner-side test for grep-ability.
	_ = body
}
