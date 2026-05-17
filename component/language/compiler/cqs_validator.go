package compiler

import (
	"fmt"

	"github.com/znasllc-io/memql/component/language/parser"
)

type CQSViolation struct {
	Message string
	Caller  string
	Callee  string
}

func (e *CQSViolation) Error() string { return e.Message }

type FunctionCallGraph struct {
	Calls map[string][]string
	Types map[string]parser.FunctionType
}

func BuildCallGraph(functions []*parser.FunctionDef) *FunctionCallGraph {
	graph := &FunctionCallGraph{
		Calls: map[string][]string{},
		Types: map[string]parser.FunctionType{},
	}
	if len(functions) == 0 {
		return graph
	}

	nameSet := make(map[string]struct{}, len(functions))
	for _, fn := range functions {
		if fn == nil || fn.Name == "" {
			continue
		}
		nameSet[fn.Name] = struct{}{}
		graph.Types[fn.Name] = fn.Type
	}

	for _, fn := range functions {
		if fn == nil || fn.Name == "" {
			continue
		}
		seen := map[string]struct{}{}
		visitFunctionBody(fn.Body, nameSet, seen)
		for callee := range seen {
			graph.Calls[fn.Name] = append(graph.Calls[fn.Name], callee)
		}
	}

	return graph
}

func ValidateCQS(functions []*parser.FunctionDef) error {
	graph := BuildCallGraph(functions)
	for caller, callees := range graph.Calls {
		callerType, ok := graph.Types[caller]
		if !ok {
			continue
		}
		for _, callee := range callees {
			calleeType, ok := graph.Types[callee]
			if !ok {
				continue
			}
			if violatesCQS(callerType, calleeType) {
				return &CQSViolation{
					Message: fmt.Sprintf("CQS violation: %s %q calls %s %q", callerType, caller, calleeType, callee),
					Caller:  caller,
					Callee:  callee,
				}
			}
		}
	}
	return nil
}

func violatesCQS(caller, callee parser.FunctionType) bool {
	switch caller {
	case parser.FunctionTypeQuery:
		return callee == parser.FunctionTypeMutation
	case parser.FunctionTypeMutation:
		return callee == parser.FunctionTypeMutation
	case parser.FunctionTypeSpec:
		return callee == parser.FunctionTypeMutation
	default:
		return false
	}
}

func visitFunctionBody(node parser.Node, known map[string]struct{}, calls map[string]struct{}) {
	if node == nil {
		return
	}

	switch n := node.(type) {
	case *parser.AutomationDef:
		for _, step := range n.Steps {
			visitStep(&step, known, calls)
		}
		if n.OnComplete != nil {
			visitStep(n.OnComplete, known, calls)
		}
		if n.OnError != nil {
			visitStep(n.OnError, known, calls)
		}
	case *parser.QueryStmt:
		visitExpression(n.Expression, known, calls)
	case *parser.MutationStmt:
		// Mutation payload is parsed into templates at runtime; direct nested function
		// expression calls inside templates are not represented in AST today.
	case parser.ExpressionNode:
		visitExpression(n, known, calls)
	}
}

func visitStep(step *parser.StepDef, known map[string]struct{}, calls map[string]struct{}) {
	if step == nil {
		return
	}
	if cfg, ok := step.Config.(*parser.FunctionStepConfig); ok && cfg.Name != "" {
		if _, exists := known[cfg.Name]; exists {
			calls[cfg.Name] = struct{}{}
		}
	}

	switch cfg := step.Config.(type) {
	case *parser.QueryStepConfig:
		visitExpression(cfg.Query, known, calls)
	case *parser.ForEachStepConfig:
		for _, nested := range cfg.Do {
			visitStep(&nested, known, calls)
		}
	case *parser.ParallelStepConfig:
		for _, nested := range cfg.Branches {
			visitStep(&nested, known, calls)
		}
	case *parser.SwitchStepConfig:
		for _, sc := range cfg.Cases {
			for _, nested := range sc.Steps {
				visitStep(&nested, known, calls)
			}
		}
		if cfg.Default != nil {
			for _, nested := range cfg.Default.Steps {
				visitStep(&nested, known, calls)
			}
		}
	}
}

func visitExpression(expr parser.ExpressionNode, known map[string]struct{}, calls map[string]struct{}) {
	if expr == nil {
		return
	}

	switch e := expr.(type) {
	case *parser.FunctionCallExpr:
		if _, exists := known[e.Name]; exists {
			calls[e.Name] = struct{}{}
		}
		for _, arg := range e.Args {
			if nested, ok := arg.(parser.ExpressionNode); ok {
				visitExpression(nested, known, calls)
			}
		}
	case *parser.LogicalExpr:
		visitExpression(e.Left, known, calls)
		visitExpression(e.Right, known, calls)
	case *parser.SortExpr:
		visitExpression(e.Target, known, calls)
	case *parser.PaginateExpr:
		visitExpression(e.Target, known, calls)
	case *parser.SelectExpr:
		visitExpression(e.Target, known, calls)
	case *parser.DepthExpr:
		visitExpression(e.Target, known, calls)
	case *parser.RelationshipExpr:
		visitExpression(e.Target, known, calls)
	case *parser.ShapeExpr:
		visitExpression(e.Target, known, calls)
	}
}
