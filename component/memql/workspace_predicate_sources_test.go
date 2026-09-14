package memql

import (
	"strings"
	"testing"
	"testing/fstest"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// A bundle workspace is its own .memql files -- every one under the DSL tree,
// none under a directory the repository walkers skip -- and, resolved over the
// core tree the way memqlmigrate resolves it, a spec the bundle declares over
// the core @actor shape is an actor predicate and the core trait resolves.
func TestWorkspacePredicateSources_BundleResolvesOverTheCoreTree(t *testing.T) {
	root := fstest.MapFS{
		"fylo/specs.memql":               {Data: []byte("spec actorEnvelope isAdmin = actor => actor.role == \"admin\"\n")},
		"fylo/node_modules/stray.memql":  {Data: []byte("trait strayTrait = row => row.x == 1\n")},
		"fylo/queries.memql":             {Data: []byte("query order orders {\n  filter  row => isAdmin(actor)\n}\n")},
		"fylo/prompts/readme.txt":        {Data: []byte("not a memql file")},
		"fylo/nested/more/deeper.memql":  {Data: []byte("trait isDeep = row => row.deep == true\n")},
		"other/unrelated/ignored.memql":  {Data: []byte("trait isOther = row => row.other == true\n")},
		"other/unrelated/ignored2.memql": {Data: []byte("\n")},
	}
	files, dir := WorkspacePredicateSources(root)
	if dir != "" {
		t.Errorf("dir = %q, want the root itself", dir)
	}
	var got []string
	for p := range files {
		got = append(got, p)
		if strings.Contains(p, "node_modules") || !strings.HasSuffix(p, ".memql") {
			t.Errorf("read %s", p)
		}
	}
	if len(files) != 5 {
		t.Errorf("read %v, want the five workspace .memql files", got)
	}
	if _, ok := files["common/traits.memql"]; ok {
		t.Error("the core tree leaked into the workspace's own sources")
	}

	base, err := langparser.ScanPredicateTree(memqldsl.Tree())
	if err != nil {
		t.Fatal(err)
	}
	var local []langparser.PredicateDeclarations
	for p, b := range files {
		local = append(local, langparser.ScanPredicateDeclarations(p, b))
	}
	preds, err := langparser.ResolvePredicatesOver(base, local)
	if err != nil {
		t.Fatal(err)
	}
	if preds["isAdmin"] != (langparser.PredicateInfo{Actor: true}) {
		t.Errorf("isAdmin = %+v, want an actor predicate through the core actorEnvelope", preds["isAdmin"])
	}
	for _, name := range []string{"isDeep", "isActiveRecord"} {
		if _, ok := preds[name]; !ok {
			t.Errorf("predicate %s is not in the set", name)
		}
	}
	if _, ok := preds["strayTrait"]; ok {
		t.Error("a trait under node_modules is in the set")
	}
}

// The engine repository's own dsl/ is found below the workspace root.
func TestWorkspacePredicateSources_FindsTheDSLTree(t *testing.T) {
	mine := []byte("trait onlyMine = row => row.mine == true\n")
	root := fstest.MapFS{
		"dsl/common/traits.memql": {Data: mine},
		"dsl/fylo/queries.memql":  {Data: []byte("query order orders {\n  filter  row => onlyMine(row)\n}\n")},
		"README.md":               {Data: []byte("# repo")},
	}
	files, dir := WorkspacePredicateSources(root)
	if dir != "dsl" {
		t.Fatalf("dir = %q, want dsl", dir)
	}
	if len(files) != 2 || string(files["common/traits.memql"]) != string(mine) {
		t.Errorf("files = %v, want the two files of dsl/", files)
	}
}
