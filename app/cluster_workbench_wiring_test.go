package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestTheClusterPhaseWiresWorkbenchForwardingOnTheBff pins WHERE the cluster
// phase calls wireWorkbenchForwarding, because the bug it guards against is
// invisible to every test that calls the function directly.
//
// wireWorkbenchForwarding has carried a NodeTypeBFF case since epic memql#4900:
// the bff serves packageDeploy, which builds, so it needs the ForwardRouter
// that finds a workbench peer. The case was right and the call was wrong --
// a.cluster() invoked the function only inside the worker-side else branch of
// `if nodeIdentity.Type == node.NodeTypeBFF`, so on the bff it never ran. The
// workbench integration then had no router, forwarder() answered nil, and
// with MEMQL_WORKBENCH_REMOTE=1 every package build refused with
// "no healthy workbench peer is reachable" while two workbench pods sat
// registered and healthy in that same bff's peer table. The unit tests in
// cluster_workbench_dialer_test.go call wireWorkbenchForwarding by hand, which
// is exactly why they stayed green.
//
// a.cluster() has no harness (it wants an engine, a gRPC server and a mesh),
// so this test reads the source: the call must not sit inside the else branch
// of the bff/worker split. Anywhere the bff also reaches is acceptable.
func TestTheClusterPhaseWiresWorkbenchForwardingOnTheBff(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cluster.go", nil, 0)
	if err != nil {
		t.Fatalf("parse cluster.go: %v", err)
	}

	var clusterFn *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "cluster" && fn.Recv != nil {
			clusterFn = fn
			break
		}
	}
	if clusterFn == nil {
		t.Fatal("cluster.go declares no (a *App) cluster() method any more; move this test to wherever the mesh wiring went")
	}

	// The bff/worker split: `if nodeIdentity.Type == node.NodeTypeBFF { ... } else { ... }`.
	var split *ast.IfStmt
	ast.Inspect(clusterFn.Body, func(n ast.Node) bool {
		if split != nil {
			return false
		}
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok || ifStmt.Else == nil {
			return true
		}
		bin, ok := ifStmt.Cond.(*ast.BinaryExpr)
		if !ok || bin.Op != token.EQL {
			return true
		}
		if exprText(bin.X) == "nodeIdentity.Type" && exprText(bin.Y) == "node.NodeTypeBFF" {
			split = ifStmt
			return false
		}
		return true
	})
	if split == nil {
		t.Fatal("cluster() no longer splits on nodeIdentity.Type == node.NodeTypeBFF; re-derive what this test should pin")
	}

	calls := 0
	insideWorkerElse := 0
	ast.Inspect(clusterFn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "wireWorkbenchForwarding" {
			return true
		}
		calls++
		if split.Else.Pos() <= call.Pos() && call.End() <= split.Else.End() {
			insideWorkerElse++
		}
		return true
	})

	if calls == 0 {
		t.Fatal("cluster() never calls wireWorkbenchForwarding, so no node type gets a workbench router")
	}
	if calls == insideWorkerElse {
		t.Fatalf("every wireWorkbenchForwarding call (%d) sits inside the worker-side else branch of the bff split at %s, so the bff never wires its workbench router and every remote package build refuses",
			calls, fset.Position(split.Pos()))
	}
}

func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	default:
		return ""
	}
}
