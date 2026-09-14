package memql

import (
	"math"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// expr_cost_test.go pins EstimateCost, the static half of the M tier's cost
// bound (D11; memql#5366). The loader refuses an M expression whose estimate
// exceeds tiers.MaxStaticCost, so what the estimate counts is a contract: one
// per node, and a collection lambda's body once per element of a collection
// the loader cannot size.

func nodeCount(n ast.ExpressionNode) int {
	count := 0
	ast.WalkV1(n, func(ast.ExpressionNode) bool {
		count++
		return true
	})
	return count
}

// A flat expression -- no lambda passed to a collection method -- costs
// exactly its node count, whatever node kinds it is made of.
func TestEstimateCostOfAFlatExpressionIsItsNodeCount(t *testing.T) {
	for _, n := range []ast.ExpressionNode{
		xid("now"),
		xbin("&&", xbin("==", xpath("row", "status"), xpath("args", "status")), xcall("isActiveRecord", xid("row"))),
		xparen(xbin("||", xbin("==", xpath("args", "x"), xnil()), xbin("==", xpath("row", "f"), xpath("args", "x")))),
		xtern(xnot(xpath("args", "p")), xneg(xlit(int64(1))), xbin("??", xpath("args", "a"), xlit("b"))),
		xmap("a", xlist(xlit(int64(1)), xopt(xid("row"), "lineage")), "b", xbin("+", xlit("x"), xlit("y"))),
		xmeth(xpath("args", "xs"), "count"),
		xmeth(xpath("args", "xs"), "take", xlit(int64(3))),
		&ast.CallExpr{Kind: "query", Name: "activeUsers", Named: []ast.NamedArg{{Name: "status", Value: xlit("active")}}},
		// A lambda that is not a collection-method argument (a relationship
		// traversal's, which runs in SQL) is counted as the nodes it is.
		xcall("childOf", xlam("c", xbin("==", xpath("c", "active"), xlit(true)))),
	} {
		if got, want := EstimateCost(n), nodeCount(n); got != want {
			t.Errorf("EstimateCost(%s) = %d, want its node count %d", ast.FormatExpr(n), got, want)
		}
	}
	if got := EstimateCost(nil); got != 0 {
		t.Errorf("EstimateCost(nil) = %d, want 0", got)
	}
}

// One collection lambda multiplies its body by the collection-size estimate;
// the method node and its receiver are counted once.
func TestEstimateCostOfOneCollectionLambdaMultipliesByTheSizeEstimate(t *testing.T) {
	recv := xpath("args", "xs")
	body := xbin(">", xid("x"), xlit(int64(1)))
	n := xmeth(recv, "where", xlam("x", body))
	want := 1 + EstimateCost(recv) + tiers.DefaultCollectionSizeEstimate*EstimateCost(body)
	if got := EstimateCost(n); got != want {
		t.Fatalf("EstimateCost(%s) = %d, want 1 + receiver + %d * body = %d", ast.FormatExpr(n), got, tiers.DefaultCollectionSizeEstimate, want)
	}
	if want != 3003 {
		t.Fatalf("the worked example is 1 + 2 + 1000 * 3 = 3003, got %d", want)
	}
	if want > tiers.MaxStaticCost {
		t.Fatalf("one scan over an unsized list must load: %d > MaxStaticCost %d", want, tiers.MaxStaticCost)
	}

	// A method's non-lambda arguments are counted once, beside the lambda.
	seed := xlit(int64(0))
	sum := xbin("+", xid("acc"), xid("x"))
	reduce := xmeth(recv, "reduce", seed, &ast.LambdaExpr{Params: []string{"acc", "x"}, Body: sum})
	wantReduce := 1 + EstimateCost(recv) + EstimateCost(seed) + tiers.DefaultCollectionSizeEstimate*EstimateCost(sum)
	if got := EstimateCost(reduce); got != wantReduce {
		t.Fatalf("EstimateCost(%s) = %d, want %d", ast.FormatExpr(reduce), got, wantReduce)
	}

	// A method chained on the scan pays for the scan once.
	chained := xmeth(n, "count")
	if got := EstimateCost(chained); got != 1+want {
		t.Fatalf("EstimateCost(%s) = %d, want %d", ast.FormatExpr(chained), got, 1+want)
	}
}

// Two nested collection lambdas multiply by the size estimate twice -- a
// million evaluations for a three-node body -- which is what the load-time
// refusal exists to catch.
func TestEstimateCostOfNestedCollectionLambdasMultiplies(t *testing.T) {
	innerBody := xbin("==", xid("y"), xid("x"))
	inner := xmeth(xpath("args", "ys"), "any", xlam("y", innerBody))
	outer := xmeth(xpath("args", "xs"), "where", xlam("x", inner))

	N := tiers.DefaultCollectionSizeEstimate
	wantInner := 1 + 2 + N*EstimateCost(innerBody)
	want := 1 + 2 + N*wantInner
	if got := EstimateCost(outer); got != want {
		t.Fatalf("EstimateCost(%s) = %d, want %d", ast.FormatExpr(outer), got, want)
	}
	if want < N*N {
		t.Fatalf("two nested scans must cost at least %d (1e6), got %d", N*N, want)
	}
	if want <= tiers.MaxStaticCost {
		t.Fatalf("two nested scans over unsized lists must exceed MaxStaticCost %d, got %d", tiers.MaxStaticCost, want)
	}
}

// The estimate saturates at math.MaxInt32 rather than overflowing: four
// nested scans are 1e12 before the body, and a wrapped number would read as a
// cheap expression.
func TestEstimateCostSaturates(t *testing.T) {
	var n ast.ExpressionNode = xbin("==", xid("v"), xlit(int64(1)))
	for i := 0; i < 6; i++ {
		n = xmeth(xpath("args", "xs"), "any", xlam("v", n))
		got := EstimateCost(n)
		if got <= 0 || got > math.MaxInt32 {
			t.Fatalf("depth %d: EstimateCost = %d, want a positive value capped at MaxInt32", i+1, got)
		}
		if i >= 3 && got != math.MaxInt32 {
			t.Fatalf("depth %d: want the saturated MaxInt32, got %d", i+1, got)
		}
	}
	// Wide as well as deep: a long conjunction of saturated terms stays capped.
	wide := n
	for i := 0; i < 10; i++ {
		wide = xbin("&&", wide, n)
	}
	if got := EstimateCost(wide); got != math.MaxInt32 {
		t.Fatalf("a conjunction of saturated terms: want MaxInt32, got %d", got)
	}
}
