package memql

import (
	"strings"
	"testing"

	parser "github.com/znasllc-io/memql/component/language/parser"
)

// TestCollectionScopeRejectedInSpecOrFilter locks the ADR §2.2 rule: the
// collection-method surface is rejected by the default converter (the one
// specs and query filters use). Only the logic-body converter
// (WithCollectionMethods) admits it.
func TestCollectionScopeRejectedInSpecOrFilter(t *testing.T) {
	pexpr, err := parser.ParseExpression(`args.members.where(m => m.active).count()`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Default converter (specs / query filters) must reject.
	if _, err := NewASTConverter().ConvertExpression(pexpr); err == nil {
		t.Fatalf("default converter accepted collection method; want scope rejection")
	} else if !strings.Contains(err.Error(), "not allowed in specs or query filters") {
		t.Fatalf("unexpected error %q; want scope rejection message", err.Error())
	}

	// Logic converter admits it.
	if _, err := NewASTConverter(WithCollectionMethods()).ConvertExpression(pexpr); err != nil {
		t.Fatalf("logic converter rejected collection method: %v", err)
	}
}

// TestLambdaPurityRejectsMutation locks the ADR §2.2 purity rule: a
// mutation/action call inside a collection lambda is a load error.
func TestLambdaPurityRejectsMutation(t *testing.T) {
	functions := map[string]*Function{
		"mutationTouch": {Name: "mutationTouch", FunctionKind: "mutation"},
		"logicBad": {
			Name:         "logicBad",
			FunctionKind: "logic",
			Expr: &CollectionMethodExpression{
				Receiver: &ArgRefExpression{Path: "members"},
				Method:   "where",
				Args: []ExpressionNode{
					&LambdaExpression{
						Params: []string{"m"},
						Body: &FunctionCallExpression{
							Name: "mutationTouch",
							Args: map[string]any{},
						},
					},
				},
			},
		},
	}

	err := enforceLambdaPurity(functions)
	if err == nil {
		t.Fatalf("expected purity error for mutation call inside lambda")
	}
	if !strings.Contains(err.Error(), "must be pure") {
		t.Fatalf("unexpected error %q; want purity message", err.Error())
	}

	// A pure lambda body passes.
	clean := map[string]*Function{
		"logicOk": {
			Name:         "logicOk",
			FunctionKind: "logic",
			Expr: &CollectionMethodExpression{
				Receiver: &ArgRefExpression{Path: "members"},
				Method:   "count",
				Args:     nil,
			},
		},
	}
	if err := enforceLambdaPurity(clean); err != nil {
		t.Fatalf("pure logic flagged as impure: %v", err)
	}
}
