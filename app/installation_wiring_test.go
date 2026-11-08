package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// A direct constructor test cannot expose this failure: the production
// transport phase precedes the router, even when every dependency is healthy.
func TestTheClusterPhaseWiresInstallationAfterWorkbench(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cluster.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.BlockStmt
	for _, declaration := range file.Decls {
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Name.Name == "cluster" && fn.Recv != nil {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("cluster startup is missing")
	}
	var router, receiver []*ast.CallExpr
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if name, ok := call.Fun.(*ast.SelectorExpr); ok {
				switch name.Sel.Name {
				case "wireWorkbenchForwarding":
					router = append(router, call)
				case "wireInstallationSelection":
					receiver = append(receiver, call)
				}
			}
		}
		return true
	})
	if len(router) != 1 || len(receiver) != 1 || router[0].Pos() >= receiver[0].Pos() {
		t.Fatal("installation must bind once, after the authenticated Workbench router")
	}
	topLevel := false
	for _, statement := range body.List {
		if expression, ok := statement.(*ast.ExprStmt); ok && expression.X == receiver[0] {
			topLevel = true
		}
	}
	if !topLevel {
		t.Fatal("installation wiring must be reachable on BFF and agent nodes outside the mesh branches")
	}
	for _, source := range []string{"transport_bff.go", "transport_agent.go", "integrations_release.go"} {
		parsed, err := parser.ParseFile(fset, source, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range parsed.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Name.Name == "wireInstallationSelection" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if name, ok := call.Fun.(*ast.SelectorExpr); ok && (name.Sel.Name == "installationPreparer" || name.Sel.Name == "wireInstallationSelection") {
						t.Errorf("%s attempts installer construction before the cluster phase", source)
					}
				}
				return true
			})
		}
	}
}
