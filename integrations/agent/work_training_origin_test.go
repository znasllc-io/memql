package agent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Internal origin opens server-only constructs. Keep all stamps inline on the
// three fixed native Training operations; a returned/reassigned context could instead grant
// that origin to later authored code or model-selected tools.
func TestTrainingInternalOriginStaysOnItsFixedNativeOperations(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	stamps, inline := 0, 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		source, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, entry.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		alias := "auth"
		for _, imp := range source.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if path == "github.com/znasllc-io/memql/component/auth" && imp.Name != nil {
				alias = imp.Name.Name
			}
		}
		isStamp := func(expr ast.Expr) bool {
			if name, ok := expr.(*ast.Ident); ok {
				return alias == "." && name.Name == "ContextWithInternalOrigin"
			}
			sel, ok := expr.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ContextWithInternalOrigin" {
				return false
			}
			pkg, ok := sel.X.(*ast.Ident)
			return ok && pkg.Name == alias
		}
		ast.Inspect(source, func(node ast.Node) bool {
			if expr, ok := node.(ast.Expr); ok && isStamp(expr) {
				stamps++
				if entry.Name() != "work_training.go" {
					t.Errorf("%s stamps internal origin outside the fixed Training operations", entry.Name())
				}
			}
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Execute" {
				return true
			}
			if stamp, ok := call.Args[0].(*ast.CallExpr); ok && isStamp(stamp.Fun) {
				inline++
			}
			return true
		})
	}
	if stamps != 3 || inline != stamps {
		t.Fatalf("found %d internal-origin references, %d inline native reads; want exactly three of each", stamps, inline)
	}
}
