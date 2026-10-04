package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestTheClusterPhaseRegistersThePipelinesExecutorAfterTheWorkbenchRouter
// pins WHERE the pipelines executor is registered (epic memql#5478), for the
// reason TestTheClusterPhaseWiresWorkbenchForwardingOnTheBff pins its call:
// the bug is invisible to a test that calls the function directly.
//
// The executor forwards cluster steps over the workbench ForwardRouter, which
// wireWorkbenchForwarding builds -- the router the node stream delivers
// replies to. Registered before it, the executor would find no router and
// answer every cluster step pipeline_runner_unavailable on a node with two
// healthy workbench replicas. And the call sits in cluster()'s own body, not
// inside the mesh block: an agent with no mesh still registers an executor
// that names what it lacks, rather than leaving the driver to say no runner
// is registered at all.
func TestTheClusterPhaseRegistersThePipelinesExecutorAfterTheWorkbenchRouter(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cluster.go", nil, 0)
	if err != nil {
		t.Fatalf("parse cluster.go: %v", err)
	}
	var clusterFn *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "cluster" && fn.Recv != nil {
			clusterFn = fn
		}
	}
	if clusterFn == nil {
		t.Fatal("cluster.go declares no (a *App) cluster() method any more; move this test to wherever the mesh wiring went")
	}

	callsOf := func(name string) []*ast.CallExpr {
		var out []*ast.CallExpr
		ast.Inspect(clusterFn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
					out = append(out, call)
				}
			}
			return true
		})
		return out
	}
	executor := callsOf("wirePipelinesExecutor")
	router := callsOf("wireWorkbenchForwarding")
	if len(executor) != 1 {
		t.Fatalf("cluster() calls wirePipelinesExecutor %d time(s), want once", len(executor))
	}
	for _, r := range router {
		if r.Pos() > executor[0].Pos() {
			t.Fatalf("wirePipelinesExecutor (%s) runs before wireWorkbenchForwarding (%s): the executor would find no router",
				fset.Position(executor[0].Pos()), fset.Position(r.Pos()))
		}
	}
	topLevel := false
	for _, stmt := range clusterFn.Body.List {
		if es, ok := stmt.(*ast.ExprStmt); ok && es.X == executor[0] {
			topLevel = true
		}
	}
	if !topLevel {
		t.Fatalf("wirePipelinesExecutor (%s) is called inside a block of cluster(); an agent that skips the block would "+
			"register no executor", fset.Position(executor[0].Pos()))
	}
}
