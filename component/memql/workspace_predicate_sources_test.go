package memql

import (
	"strings"
	"testing"
	"testing/fstest"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// A bundle workspace resolves its own predicates and the core tree's: its
// filter naming a core trait rewrites the way the engine's flat registry
// resolves it once the bundle loads over the core tree.
func TestWorkspacePredicateSources_BundleSeesTheCoreTree(t *testing.T) {
	root := fstest.MapFS{
		"fylo/traits.memql":              {Data: []byte("trait isShipped {\n  return status == \"shipped\"\n}\n")},
		"fylo/node_modules/stray.memql":  {Data: []byte("trait strayTrait {\n  return x == 1\n}\n")},
		"fylo/queries.memql":             {Data: []byte("query order orders {\n  filter  isShipped\n}\n")},
		"fylo/prompts/readme.txt":        {Data: []byte("not a memql file")},
		"fylo/nested/more/deeper.memql":  {Data: []byte("trait isDeep {\n  return deep == true\n}\n")},
		"other/unrelated/ignored.memql":  {Data: []byte("trait isOther {\n  return other == true\n}\n")},
		"other/unrelated/ignored2.memql": {Data: []byte("\n")},
	}
	files, dir := WorkspacePredicateSources(root)
	if dir != "" {
		t.Errorf("dir = %q, want the root itself", dir)
	}
	for _, want := range []string{"fylo/traits.memql", "fylo/nested/more/deeper.memql", "other/unrelated/ignored.memql", "common/traits.memql"} {
		if _, ok := files[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
	for p := range files {
		if strings.Contains(p, "node_modules") || !strings.HasSuffix(p, ".memql") {
			t.Errorf("read %s", p)
		}
	}
	preds, err := langparser.CollectPredicates(files)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"isShipped", "isDeep", "isActiveRecord"} {
		if _, ok := preds[name]; !ok {
			t.Errorf("predicate %s is not in the set", name)
		}
	}
	if _, ok := preds["strayTrait"]; ok {
		t.Error("a trait under node_modules is in the set")
	}
}

// A workspace that carries a core domain answers for it: the engine
// repository's own dsl/ replaces the embedded copy of every domain it holds,
// and the embedded tree still answers for the rest.
func TestWorkspacePredicateSources_WorkspaceDomainReplacesTheCoreCopy(t *testing.T) {
	mine := []byte("trait onlyMine {\n  return mine == true\n}\n")
	root := fstest.MapFS{
		"dsl/common/traits.memql": {Data: mine},
		"dsl/fylo/queries.memql":  {Data: []byte("query order orders {\n  filter  onlyMine\n}\n")},
		"README.md":               {Data: []byte("# repo")},
	}
	files, dir := WorkspacePredicateSources(root)
	if dir != "dsl" {
		t.Fatalf("dir = %q, want dsl", dir)
	}
	if got := string(files["common/traits.memql"]); got != string(mine) {
		t.Errorf("common/traits.memql is the embedded copy, not the workspace's:\n%s", got)
	}
	for p := range files {
		if strings.HasPrefix(p, "common/") && p != "common/traits.memql" {
			t.Errorf("the embedded %s leaked into a workspace that carries common", p)
		}
	}
	if _, ok := files["identity/concepts.memql"]; !ok {
		t.Error("a core domain the workspace does not carry is missing")
	}
	preds, err := langparser.CollectPredicates(files)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := preds["isActiveRecord"]; ok {
		t.Error("isActiveRecord comes from the embedded common domain the workspace replaced")
	}
	if _, ok := preds["onlyMine"]; !ok {
		t.Error("the workspace's own trait is missing")
	}
}
