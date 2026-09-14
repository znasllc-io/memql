package memql

// logic_body_v1.go -- a logic body parsed in the edition-2026 grammar
// (FunctionDef.ExpressionsV1), bridged onto the two runners logic has today
// (epic memql#5363, memql#5367).
//
// # Which runner
//
// The loader decides, as it does for a legacy body, and by the same rule
// wherever the two grammars can share one:
//
//   - a body with statements before its `return` runs on the LogicRunner
//     (fn.LogicSteps). The runner's v1 half already exists: it compiles the
//     body with the v1 generator and evaluates every statement over the run
//     (component/automations, expressions_v1.go). The loader stores the body.
//   - a body that is ONE `return` of a construct call -- `return query
//     expiredActiveDelegations(asOf: args.asOf)` -- runs through fn.Expr, the
//     engine IR a top-level call expands. The call becomes the
//     FunctionCallExpression the legacy conversion produces, so it
//     dispatches exactly as it did and the caller reads the construct's own
//     result (`decide.nodes()`). Each argument is a literal value, or for an
//     expression a PlanConstExpression -- the IR leaf that carries a v1 AST
//     -- which argument expansion evaluates with EvalExpr over the call's
//     arguments and the ambient envelope (evaluateLogicArgumentV1), the one
//     point both are known.
//   - a body that is one `return` of an EXPRESSION -- `return
//     args.gate.passed ?? false` -- runs on the LogicRunner too, whose v1 half
//     evaluates it with EvalExpr over the scope a logic body reads (args,
//     actor, now, config, event). Its fn.Expr is the expression as that same
//     leaf, which no dispatch evaluates -- the LogicSteps hoist runs first --
//     and which is there for the walkers that read fn.Expr.
//
// The legacy grammar sends that last shape through fn.Expr as well, where the
// engine folds it at the plan root; the two produce the same result, a flat
// output carrying the value, and the equivalence corpus
// (component/automations/steps, logic_v1_corpus_test.go) holds them to it.
//
// # What load refuses
//
// What neither runner can run: a construct call anywhere but a statement's
// whole right-hand side or the whole return (EvalExpr has no construct
// dispatch inside a logic body), a return that calls an automation, an
// action or a capability, and a body the v1 generator does not compile --
// which would otherwise fail on every call instead of once, at boot.
//
// # Lifetime
//
// A bridge. The statement runner that replaces fn.LogicSteps and RunLogic
// replaces this file with them.

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/compiler"
	"github.com/znasllc-io/memql/component/language/functions"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// loadLogicBodyV1 is the logic branch of tryParseNewFunctionSyntax for a v1
// body: it checks the body and returns fn.Expr, and whether the body runs on
// the LogicRunner (fn.LogicSteps).
func loadLogicBodyV1(auto *languageParser.AutomationDef, ret ast.ExpressionNode) (ExpressionNode, bool, error) {
	for i := range auto.Steps {
		if err := checkLogicStepV1(&auto.Steps[i]); err != nil {
			return nil, false, err
		}
	}
	// The runner compiles the body on every call; compiling it once here is
	// what turns a body it cannot compile into a load error.
	file := &languageParser.File{Definitions: []languageParser.Node{&languageParser.FunctionDef{
		Name: auto.Name,
		Type: languageParser.FunctionTypeAutomation,
		Body: auto,
	}}}
	if _, err := compiler.NewDefault().CompileFile(file); err != nil {
		return nil, false, err
	}
	expr, pure, err := convertLogicReturnV1(ret)
	if err != nil {
		return nil, false, err
	}
	return expr, pure || nonReturnStepCount(auto.Steps) > 0, nil
}

// convertLogicReturnV1 converts a v1 body's return expression into fn.Expr.
// pure is true when it is an expression rather than a construct call.
func convertLogicReturnV1(ret ast.ExpressionNode) (ExpressionNode, bool, error) {
	call, err := logicReturnCallV1(ret)
	if err != nil {
		return nil, false, err
	}
	if call == nil {
		return &PlanConstExpression{Expr: ret}, true, nil
	}
	args := make(map[string]any, len(call.Args)+len(call.Named))
	for i, a := range call.Args {
		args[fmt.Sprint(i)] = logicArgumentV1(a)
	}
	for _, na := range call.Named {
		args[na.Name] = logicArgumentV1(na.Value)
	}
	return &FunctionCallExpression{Name: call.Name, Args: args}, false, nil
}

// logicReturnCallV1 is the construct call a return makes, or nil when the
// return is an expression. A call names its kind (`return query x(...)`); a
// call without one is a construct call too when it names no catalog
// function, as the legacy conversion read it -- and refused, since the
// runner reads the same text as an expression: the two runners would
// disagree about it.
func logicReturnCallV1(ret ast.ExpressionNode) (*ast.CallExpr, error) {
	call, ok := ast.Unparen(ret).(*ast.CallExpr)
	if !ok || call.Receiver != nil {
		return nil, nil
	}
	switch call.Kind {
	case "query", "mutation", "logic", "builtin":
		return call, nil
	case "":
		if _, catalog := functions.Lookup(call.Name); catalog {
			return nil, nil
		}
		return nil, fmt.Errorf("`return %s` calls %s, which is not a catalog function: a construct call names its kind, as in `return query %s(...)` or `return builtin %s(...)`",
			ast.FormatExpr(call), call.Name, call.Name, call.Name)
	default:
		return nil, fmt.Errorf("`return %s`: a logic body returns a value, and %s %s is not called for one -- run it as a statement, as in `x := %s %s(...)`",
			ast.FormatExpr(call), call.Kind, call.Name, call.Kind, call.Name)
	}
}

// logicArgumentV1 is one argument of a return call: a literal is its value,
// as the legacy conversion stored a constant argument; anything else is the
// expression, which argument expansion evaluates.
func logicArgumentV1(n ast.ExpressionNode) any {
	if v, ok := literalValueV1(n); ok {
		return v
	}
	return &PlanConstExpression{Expr: n}
}

// literalValueV1 is the value of an expression that is a literal -- a
// scalar, nil, or a list or map of literals -- and ok=false for anything that
// has to be evaluated.
func literalValueV1(n ast.ExpressionNode) (any, bool) {
	switch e := ast.Unparen(n).(type) {
	case *ast.LiteralExpr:
		switch e.Value.(type) {
		case string, bool, float64, int, int64:
			return e.Value, true
		}
		return nil, false
	case *ast.NilExpr:
		return nil, true
	case *ast.ListExpr:
		out := make([]any, len(e.Elems))
		for i, el := range e.Elems {
			v, ok := literalValueV1(el)
			if !ok {
				return nil, false
			}
			out[i] = v
		}
		return out, true
	case *ast.MapExpr:
		out := make(map[string]any, len(e.Entries))
		for _, en := range e.Entries {
			v, ok := literalValueV1(en.Value)
			if !ok {
				return nil, false
			}
			out[en.Key] = v
		}
		return out, true
	}
	return nil, false
}

// evaluateLogicArgumentV1 evaluates one expression argument of a v1 return
// call during argument expansion, over what the body reads: the call's
// arguments, the ambient envelope (actor, config, now) and the event the
// LogicRunner binds (the call's `event` argument, or an empty envelope).
// present is false when the argument evaluated to absent: it is not passed,
// as an absent argument of a statement's call is not (v1ConstructCallText).
func evaluateLogicArgumentV1(pc *PlanConstExpression, args, ambient map[string]any) (any, bool, error) {
	bindings := planConstantBindings(args, ambient)
	if bindings["args"] == nil {
		bindings["args"] = map[string]any{}
	}
	bindings["event"] = logicEventBindingV1(args)
	v, err := EvalExpr(context.Background(), pc.Expr, MapScope(bindings), EvalOptions{CanonicalID: canonicalIDForPlanConstant})
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", formatPlanConstant(pc), err)
	}
	if IsAbsent(v) {
		return nil, false, nil
	}
	return v, true, nil
}

// logicEventBindingV1 is the LogicRunner's `event` binding
// (logicEventBinding in component/automations): the call's `event`
// argument, or a well-formed empty envelope.
func logicEventBindingV1(args map[string]any) any {
	if ev, ok := args["event"]; ok && ev != nil {
		return ev
	}
	return map[string]any{"topic": "", "kind": "", "payload": map[string]any{}}
}

// checkLogicStepV1 refuses a construct call a step carries anywhere but at
// its statement: the whole right-hand side (a function step, or a query step
// whose query is the call) and, inside that call, nowhere.
func checkLogicStepV1(step *languageParser.StepDef) error {
	where := fmt.Sprintf("statement %q", step.ID)
	if step.ID == "_return" {
		where = "the return"
	}
	check := func(n ast.ExpressionNode) error { return refuseNestedConstructCall(n, where) }
	if err := check(step.ConditionExpr); err != nil {
		return err
	}
	switch cfg := step.Config.(type) {
	case *languageParser.QueryStepConfig:
		n, _ := cfg.Query.(ast.ExpressionNode)
		if call, ok := ast.Unparen(n).(*ast.CallExpr); ok && call.Receiver == nil && ast.KindOf(call) == ast.KindConstructCall {
			return checkCallArgumentsV1(call.Args, call.Named, check)
		}
		return check(n)
	case *languageParser.FunctionStepConfig:
		return checkValuesV1(cfg.Args, check)
	case *languageParser.ActionStepConfig:
		return checkValuesV1(cfg.Args, check)
	case *languageParser.MutationStepConfig:
		if cfg.Mutation == nil {
			return nil
		}
		for _, v := range []any{cfg.Mutation.PayloadExpr, cfg.Mutation.IDTemplate, cfg.Mutation.CreatedAtTemplate, cfg.Mutation.ParentTemplate, cfg.Mutation.AliasOfTemplate} {
			if n, ok := v.(ast.ExpressionNode); ok {
				if err := check(n); err != nil {
					return err
				}
			}
		}
	case *languageParser.ForEachStepConfig:
		if err := check(cfg.SourceExpr); err != nil {
			return err
		}
		if err := check(cfg.FilterExpr); err != nil {
			return err
		}
		for i := range cfg.Do {
			if err := checkLogicStepV1(&cfg.Do[i]); err != nil {
				return err
			}
		}
	case *languageParser.ParallelStepConfig:
		for i := range cfg.Branches {
			if err := checkLogicStepV1(&cfg.Branches[i]); err != nil {
				return err
			}
		}
	case *languageParser.SwitchStepConfig:
		if err := check(cfg.ExpressionExpr); err != nil {
			return err
		}
		cases := make([]*languageParser.SwitchCase, 0, len(cfg.Cases)+1)
		for _, sc := range cfg.Cases {
			cases = append(cases, sc)
		}
		cases = append(cases, cfg.Default)
		for _, sc := range cases {
			if sc == nil {
				continue
			}
			for i := range sc.Steps {
				if err := checkLogicStepV1(&sc.Steps[i]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// checkCallArgumentsV1 applies check to every argument of a call.
func checkCallArgumentsV1(positional []ast.ExpressionNode, named []ast.NamedArg, check func(ast.ExpressionNode) error) error {
	for _, a := range positional {
		if err := check(a); err != nil {
			return err
		}
	}
	for _, na := range named {
		if err := check(na.Value); err != nil {
			return err
		}
	}
	return nil
}

// checkValuesV1 applies check to every v1 node in a step's value map.
func checkValuesV1(values map[string]any, check func(ast.ExpressionNode) error) error {
	for _, v := range values {
		if n, ok := v.(ast.ExpressionNode); ok {
			if err := check(n); err != nil {
				return err
			}
		}
	}
	return nil
}

// refuseNestedConstructCall refuses the first construct call inside n.
func refuseNestedConstructCall(n ast.ExpressionNode, where string) error {
	if n == nil {
		return nil
	}
	var found *ast.CallExpr
	ast.WalkV1(n, func(x ast.ExpressionNode) bool {
		if found != nil {
			return false
		}
		if call, ok := x.(*ast.CallExpr); ok && ast.KindOf(call) == ast.KindConstructCall {
			found = call
			return false
		}
		return true
	})
	if found == nil {
		return nil
	}
	return fmt.Errorf("%s calls `%s` inside an expression: a construct call is a statement of its own, so bind it first -- `rows := %s %s(...)` -- and read `rows`",
		where, ast.FormatExpr(found), found.Kind, found.Name)
}
