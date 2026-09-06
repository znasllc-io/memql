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

// THE DOGFOOD GATE. The platform's own site is served exactly like a
// customer's: same concept, same resolution, same bundle opener, same
// headers. The only thing that differs is where its hostname comes from --
// the cluster install rather than a UI -- and that is data, not a code path.
//
// This is a source scan rather than a behavioural test because the failure it
// prevents is a branch someone adds later "just for ours", which every
// behavioural test would still pass.
//
// IT SCANS FOR TWO THINGS, and the second is the one that matters now. The
// gate was written when the platform's site was the MemQL Portal and banned
// the word "portal"; epic memql#4984 retired the portal and made MemQL OS the
// platform's site, at which point banning "portal" alone guarded a site that
// no longer exists. "portal" is KEPT -- a special case must not come back
// under the old name either -- and the OS shell's own literals are added
// beside it.
//
// The OS ban is by LITERAL (`app/os`, the seeded hostname) rather than by the
// substring "os", which would match os.DirFS, Close, Hostname and most of
// this package. A gate that cannot be written precisely is worse than no gate:
// it gets disabled the first time it cries wolf.
func TestPlatformSiteHasNoSpecialCaseInTheServingPath(t *testing.T) {
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
			for _, banned := range []string{"portal", "app/os", "os.memql.localhost"} {
				if strings.Contains(v, banned) {
					t.Errorf("%s:%d names the platform's own site in the serving path (%q): %s\n"+
						"The platform's site is resolved and served exactly like any "+
						"other. If it needs different DATA, put it in the seeded row "+
						"(dsl/platform/seeds.memql), not in a branch here.",
						name, fset.Position(lit.Pos()).Line, banned, lit.Value)
				}
			}
			return true
		})
	}
}
