package pipelinesteps

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestTheInternalOriginStampNeverEscapesItsCall is the precondition the
// package's entry in call_origin_conformance_test.go rests on (epic
// memql#5478): internal origin is stamped in ONE place -- inline, as the
// context argument of the fleet's one Dispatch call -- so the pipeline purpose
// it admits can be claimed by no other path of this package, and the marked
// context dies at that call. go/parser ignores build constraints, so the
// agent-tagged fleet.go is read by an untagged test.
func TestTheInternalOriginStampNeverEscapesItsCall(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	type site struct {
		file   string
		inline bool
	}
	var sites []site
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		authName := ""
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "github.com/znasllc-io/memql/component/auth" {
				authName = "auth"
				if imp.Name != nil {
					authName = imp.Name.Name
				}
			}
		}
		if authName == "" {
			continue
		}
		isStamp := func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ContextWithInternalOrigin" {
				return false
			}
			pkg, ok := sel.X.(*ast.Ident)
			return ok && pkg.Name == authName
		}
		// Every reference to the stamp, and whether it is the inline argument
		// of a Dispatch call.
		inlineCalls := map[ast.Node]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fun, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || fun.Sel.Name != "Dispatch" {
				return true
			}
			for _, arg := range call.Args {
				if stamp, ok := arg.(*ast.CallExpr); ok && isStamp(stamp.Fun) {
					inlineCalls[stamp.Fun] = true
				}
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			if isStamp(n) {
				sites = append(sites, site{file: name, inline: inlineCalls[n]})
			}
			return true
		})
	}
	if len(sites) != 1 {
		t.Fatalf("internal origin is stamped at %d site(s) %+v, want exactly one: the fleet's Dispatch call", len(sites), sites)
	}
	if sites[0].file != "fleet.go" || !sites[0].inline {
		t.Fatalf("the stamp is at %+v; it must be the inline context argument of fleet.go's Dispatch call, so the "+
			"marked context dies at that call", sites[0])
	}
}
