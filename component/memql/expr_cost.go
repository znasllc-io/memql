package memql

import (
	"math"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// expr_cost.go -- the static half of the M tier's cost bound (D11;
// memql#5366).
//
// An in-process expression is bounded twice: at run time by EvalExpr's step
// budget, and at load by this estimate, which the loader compares with
// tiers.MaxStaticCost. The estimate counts what the budget counts -- node
// evaluations -- so an expression the loader admits is, on the estimate's
// assumptions, one the budget lets finish:
//
//   - every node costs one;
//   - a collection method's lambda body runs once per element, and the
//     loader cannot know how many elements an argument or a step result will
//     hold, so it assumes tiers.DefaultCollectionSizeEstimate. A method call
//     with a lambda therefore costs 1 + its receiver + its other arguments +
//     DefaultCollectionSizeEstimate x the lambda's body, and a lambda nested
//     in another multiplies again: two nested scans of an unsized list are a
//     million evaluations of their body;
//   - a ternary is charged both branches, an upper bound for whichever runs;
//   - the lambda node itself costs nothing when it is a method argument, as
//     EvalExpr never evaluates it (it evaluates the body); anywhere else --
//     a relationship traversal's argument, which runs in SQL -- it is counted
//     as the nodes it is.
//
// The estimate saturates at math.MaxInt32 rather than overflowing, so a
// deeply nested scan reads as the most expensive expression there is, never
// as a wrapped, cheap-looking one.

// EstimateCost returns the static cost estimate of an edition-2026
// expression: 1 per node, a collection method's lambda body counted
// tiers.DefaultCollectionSizeEstimate times, nested scans multiplying,
// saturating at math.MaxInt32. A nil expression costs 0.
func EstimateCost(n ast.ExpressionNode) int {
	return exprCost(n)
}

func exprCost(n ast.ExpressionNode) int {
	switch e := n.(type) {
	case nil:
		return 0
	case *ast.IdentExpr, *ast.LiteralExpr, *ast.NilExpr:
		return 1
	case *ast.MemberExpr:
		if e == nil {
			return 0
		}
		return exprCostAdd(1, exprCost(e.Object))
	case *ast.UnaryExpr:
		if e == nil {
			return 0
		}
		return exprCostAdd(1, exprCost(e.Operand))
	case *ast.BinaryExpr:
		if e == nil {
			return 0
		}
		return exprCostAdd(1, exprCostAdd(exprCost(e.Left), exprCost(e.Right)))
	case *ast.TernaryExpr:
		if e == nil {
			return 0
		}
		return exprCostAdd(1, exprCostAdd(exprCost(e.Condition), exprCostAdd(exprCost(e.Then), exprCost(e.Else))))
	case *ast.ParenExpr:
		if e == nil {
			return 0
		}
		return exprCostAdd(1, exprCost(e.Inner))
	case *ast.ListExpr:
		if e == nil {
			return 0
		}
		cost := 1
		for _, el := range e.Elems {
			cost = exprCostAdd(cost, exprCost(el))
		}
		return cost
	case *ast.MapExpr:
		if e == nil {
			return 0
		}
		cost := 1
		for _, en := range e.Entries {
			cost = exprCostAdd(cost, exprCost(en.Value))
		}
		return cost
	case *ast.LambdaExpr:
		if e == nil {
			return 0
		}
		return exprCostAdd(1, exprCost(e.Body))
	case *ast.CallExpr:
		if e == nil {
			return 0
		}
		cost := 1
		if e.Receiver != nil {
			cost = exprCostAdd(cost, exprCost(e.Receiver))
		}
		for _, a := range e.Args {
			if lam, ok := ast.Unparen(a).(*ast.LambdaExpr); ok && lam != nil && e.Receiver != nil {
				// A collection method's lambda: its body per element of a
				// collection the loader cannot size.
				cost = exprCostAdd(cost, exprCostMul(tiers.DefaultCollectionSizeEstimate, exprCost(lam.Body)))
				continue
			}
			cost = exprCostAdd(cost, exprCost(a))
		}
		for _, a := range e.Named {
			cost = exprCostAdd(cost, exprCost(a.Value))
		}
		return cost
	}
	// A node outside the edition-2026 set: one node, and nothing below it
	// that this walk can see.
	return 1
}

// exprCostAdd adds two costs, saturating at math.MaxInt32. Costs are never
// negative.
func exprCostAdd(a, b int) int {
	if a > math.MaxInt32-b {
		return math.MaxInt32
	}
	return a + b
}

// exprCostMul multiplies two costs, saturating at math.MaxInt32.
func exprCostMul(a, b int) int {
	if a == 0 || b == 0 {
		return 0
	}
	if a > math.MaxInt32/b {
		return math.MaxInt32
	}
	return a * b
}
