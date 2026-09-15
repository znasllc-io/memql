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

	// A logic's and an automation's calls are not walked: neither kind can
	// violate the rule (violatesCQS), and a construct call in one is a
	// statement the compiler's scope check holds.
	switch n := node.(type) {
	case *parser.QueryStmt:
		visitExpression(n.Expression, known, calls)
	case *parser.MutationStmt:
		// Mutation payload is parsed into templates at runtime; direct nested function
		// expression calls inside templates are not represented in AST today.
	case parser.ExpressionNode:
		visitExpression(n, known, calls)
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
	case *parser.RefineExpr:
		// The refine clause (memql#5366) wraps the paginated query, whose
		// calls are what CQS judges; the lambda is a v1 AST whose calls are
		// catalog functions and predicates, and the engine refuses a
		// construct call in it at load.
		visitExpression(e.Target, known, calls)
	case *parser.SelectExpr:
		visitExpression(e.Target, known, calls)
	case *parser.DepthExpr:
		visitExpression(e.Target, known, calls)
	case *parser.CountExpr:
		visitExpression(e.Target, known, calls)
	case *parser.RelationshipExpr:
		visitExpression(e.Target, known, calls)
	case *parser.ShapeExpr:
		visitExpression(e.Target, known, calls)
	}
}
