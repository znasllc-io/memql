package parser

import (
	"testing"
)

// parseLogicStepDefs is a local helper: it normalises + parses a logic source
// string and returns the parsed *AutomationDef step list. It mirrors the
// production load path (NormaliseAll -> NewParser -> SetSource -> Parse) so a
// collection-chain step RHS gets its source span captured (#2317).
func parseLogicStepDefs(t *testing.T, src string) []StepDef {
	t.Helper()
	normalised, err := NormaliseAll(src)
	if err != nil {
		t.Fatalf("NormaliseAll: %v", err)
	}
	lexer := NewLexer(normalised)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	p := NewParser(tokens)
	p.SetSource(normalised)
	ast, err := p.Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("expected *File, got %T", ast)
	}
	for _, def := range file.Definitions {
		fd, ok := def.(*FunctionDef)
		if !ok || fd.Type != FunctionTypeLogic {
			continue
		}
		body, ok := fd.Body.(*AutomationDef)
		if !ok {
			t.Fatalf("expected *AutomationDef body, got %T", fd.Body)
		}
		return body.Steps
	}
	t.Fatalf("no logic function found")
	return nil
}

// TestParser_CollectionChainStepRHS_EmitsQueryStep pins the #2317 parser fix:
// a multi-statement logic step whose RHS is a collection-method / lambda chain
// (`active := args.members.where(m => m.active)`) is no longer rejected with
// "step RHS must be a function call or builtin; got *ast.MethodCallExpr".
// Instead it parses into a StepTypeQuery step whose QueryStepConfig carries the
// chain's VERBATIM source on the MethodCallExpr.Raw field, which the compiler
// emits as-is so the runtime collection evaluator re-parses what was written.
func TestParser_CollectionChainStepRHS_EmitsQueryStep(t *testing.T) {
	src := `@description("chain step probe")
logic logicProbe {
  args {
    members []object @required
  }
  body {
    active := args.members.where(m => m.active)
    return active.count()
  }
}`
	steps := parseLogicStepDefs(t, src)

	var active *StepDef
	for i := range steps {
		if steps[i].ID == "active" {
			active = &steps[i]
			break
		}
	}
	if active == nil {
		t.Fatalf("expected an `active` step; got %d steps", len(steps))
	}
	if active.Type != StepTypeQuery {
		t.Fatalf("active step type = %v, want %v (a collection chain RHS must emit a query step)", active.Type, StepTypeQuery)
	}
	cfg, ok := active.Config.(*QueryStepConfig)
	if !ok {
		t.Fatalf("active step config = %T, want *QueryStepConfig", active.Config)
	}
	chain, ok := cfg.Query.(*MethodCallExpr)
	if !ok {
		t.Fatalf("active step query expr = %T, want *MethodCallExpr", cfg.Query)
	}
	if chain.Raw != `args.members.where(m => m.active)` {
		t.Errorf("captured chain source = %q, want %q", chain.Raw, `args.members.where(m => m.active)`)
	}
}

// TestParser_NonChainStepRHS_StillErrors pins that the #2317 rescue is narrow:
// only a genuine *MethodCallExpr RHS is converted to a query step. A bare
// literal RHS (`x := 5`) is neither a function call nor a chain, so it keeps
// erroring exactly as before (the chain rescue lives inside the `ident(...)`
// call branch, which a literal never enters).
func TestParser_NonChainStepRHS_StillErrors(t *testing.T) {
	src := `@description("invalid literal step RHS")
logic logicBad {
  args {
    n integer @required
  }
  body {
    x := 5
    return x
  }
}`
	normalised, err := NormaliseAll(src)
	if err != nil {
		t.Fatalf("NormaliseAll: %v", err)
	}
	lexer := NewLexer(normalised)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	p := NewParser(tokens)
	p.SetSource(normalised)
	if _, err := p.Parse(); err == nil {
		t.Fatalf("expected a parse error for `x := 5` (a non-call, non-chain literal step RHS), got nil")
	}
}
