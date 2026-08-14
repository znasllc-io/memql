// component/edge/dogfood_test.go
package edge

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE DOGFOOD GATE. The portal is site #1: same concept, same resolution,
// same bundle opener, same headers as any customer site. The only thing that
// differs is where its hostname comes from -- the cluster install rather than
// the portal UI -- and that is data, not a code path.
//
// This is a source scan rather than a behavioural test because the failure it
// prevents is a branch someone adds later "just for the portal", which every
// behavioural test would still pass.
func TestPortalHasNoSpecialCaseInTheServingPath(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v := strings.ToLower(strings.Trim(lit.Value, `"`))
			if strings.Contains(v, "portal") {
				t.Errorf("%s:%d names the portal in the serving path: %s\n"+
					"The portal is site #1 and must take the same path as any other "+
					"site. If it needs different DATA, put it in the seeded row.",
					name, fset.Position(lit.Pos()).Line, lit.Value)
			}
			return true
		})
	}
}
