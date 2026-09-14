package parser

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
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
	root, err := p.Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	file, ok := root.(*File)
	if !ok {
		t.Fatalf("expected *File, got %T", root)
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
// chain -- in edition 2026 the v1 method call itself, whose canonical source
// is what was written, for the runtime evaluator to run in process.
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
	chain, ok := cfg.Query.(*ast.CallExpr)
	if !ok || chain.Receiver == nil || chain.Name != "where" {
		t.Fatalf("active step query expr = %T %v, want the method call .where(...)", cfg.Query, cfg.Query)
	}
	if got := ast.FormatExpr(chain); got != `args.members.where(m => m.active)` {
		t.Errorf("the chain reads %q, want %q", got, `args.members.where(m => m.active)`)
	}
}

// TestParser_NonCallStepRHS_IsAQueryStep pins what edition 2026 made of the
// #2317 rescue. The legacy grammar converted only a genuine method chain to a
// query step and refused every other non-call RHS; edition 2026's step RHS is
// any expression -- a call without a receiver is a function step, anything
// else a query step the runtime evaluates in process -- so a bare literal
// (`x := 5`) is a query step carrying the literal, and a call is still a
// function step.
func TestParser_NonCallStepRHS_IsAQueryStep(t *testing.T) {
	src := `@description("literal step RHS")
logic logicLiteral {
  args {
    n integer @required
  }
  body {
    x := 5
    y := double(n: args.n)
    return x + y
  }
}`
	steps := parseLogicStepDefs(t, src)
	byID := map[string]StepDef{}
	for _, s := range steps {
		byID[s.ID] = s
	}
	x, ok := byID["x"]
	if !ok || x.Type != StepTypeQuery {
		t.Fatalf("x step = %+v, want a query step", x)
	}
	if lit, ok := x.Config.(*QueryStepConfig).Query.(*ast.LiteralExpr); !ok || ast.FormatExpr(lit) != "5" {
		t.Errorf("x step query = %T %v, want the literal 5", x.Config.(*QueryStepConfig).Query, x.Config.(*QueryStepConfig).Query)
	}
	y, ok := byID["y"]
	if !ok || y.Type != StepTypeFunction {
		t.Fatalf("y step = %+v, want a function step", y)
	}
	if cfg := y.Config.(*FunctionStepConfig); cfg.Name != "double" {
		t.Errorf("y step calls %q, want double", cfg.Name)
	}
}
