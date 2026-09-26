package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/znasllc-io/memql/integrations/shopify"
)

// Connect Shopify's callback reaches the Shopify code through a hook the
// identity bootstrap wires (design 12.7). A hook left nil answers the callback
// 404 past the install landing, which reads as "Shopify sent me nowhere".

func TestTheShopifyConnectHookIsTheConnector(t *testing.T) {
	conn := shopify.NewConnector(nil, nil, nil, nil)
	if got := shopifyConnectHook(shopify.NewIntegration(conn)); got != conn {
		t.Fatalf("hook = %v, want the Shopify connector", got)
	}
	// No integration is NO hook -- an untyped nil, so the server's nil check
	// holds. A typed nil in the interface would read as wired.
	if got := shopifyConnectHook(nil); got != nil {
		t.Fatalf("hook for no integration = %#v, want nil", got)
	}
}

func TestTheIdentityBootstrapWiresTheShopifyConnectHook(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "integrations_identity.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	wired := false
	ast.Inspect(file, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		sel, ok := assign.Lhs[0].(*ast.SelectorExpr)
		call, isCall := assign.Rhs[0].(*ast.CallExpr)
		if ok && sel.Sel.Name == "ShopifyConnect" && isCall {
			if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "shopifyConnectHook" {
				wired = true
			}
		}
		return true
	})
	if !wired {
		t.Fatal("integrations_identity.go does not set the identity server's ShopifyConnect from shopifyConnectHook")
	}
}
