package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// xmTree is a small tree whose predicates live in other files than the
// clauses that use them -- the reason `expressions` is a tree rewrite.
func xmTree() map[string][]byte {
	return map[string][]byte{
		"common/traits.memql": []byte("/// Active rows.\ntrait isActiveRecord {\n  return active==true\n}\n"),
		"common/shapes.memql": []byte("/// The caller.\n@actor\nshape actorEnvelope {\n  actor.userId\n  actor.role\n}\n"),
		"deploy/specs.memql": []byte("use common.shapes.{ actorEnvelope }\n\n" +
			"/// Owner only.\nspec actorEnvelope requiresOwner {\n  return role == \"owner\"\n}\n"),
		"deploy/queries.memql": []byte("use deploy.specs.{ requiresOwner }\nuse common.traits.{ isActiveRecord }\n\n" +
			"query deployment deployments {\n  args {\n    id string\n  }\n" +
			"  filter  row.id==args.id && requiresOwner && isActiveRecord\n}\n"),
		"deploy/notes.txt": []byte("filter x==y is not a .memql file\n"),
	}
}

func TestExpressionsResolvesPredicatesAcrossFiles(t *testing.T) {
	out, err := rewriteExpressions("root", xmTree())
	if err != nil {
		t.Fatalf("rewriteExpressions: %v", err)
	}
	var changed []string
	for p := range out {
		changed = append(changed, p)
	}
	sort.Strings(changed)
	if want := []string{"common/traits.memql", "deploy/queries.memql", "deploy/specs.memql"}; strings.Join(changed, ",") != strings.Join(want, ",") {
		t.Errorf("changed files = %v, want exactly %v (an unchanged or non-.memql file is not returned)", changed, want)
	}
	for file, line := range map[string]string{
		// requiresOwner is a spec over an @actor shape declared two files
		// away, so it is applied to actor; the trait is applied to row.
		"deploy/queries.memql": "  filter  row => row.id == args.id && requiresOwner(actor) && isActiveRecord(row)\n",
		"deploy/specs.memql":   "/// Owner only.\nspec actorEnvelope requiresOwner = actor => actor.role == \"owner\"\n",
		"common/traits.memql":  "/// Active rows.\ntrait isActiveRecord = row => row.active == true\n",
	} {
		if !strings.Contains(string(out[file]), line) {
			t.Errorf("%s:\n%s\nwant it to contain:\n%s", file, out[file], line)
		}
	}
}

// Every refused clause in every file is named, so one run shows the whole list.
func TestExpressionsRefusalNamesEveryFileAndLine(t *testing.T) {
	files := xmTree()
	files["deploy/queries.memql"] = []byte("query deployment deployments {\n  filter  isNotDeclared\n}\n")
	files["other/queries.memql"] = []byte("query other others {\n  filter  alsoNotDeclared && a==1\n}\n")
	out, err := rewriteExpressions("root", files)
	if err == nil {
		t.Fatalf("want a refusal, got %v", out)
	}
	for _, want := range []string{
		`deploy/queries.memql: line 2: query deployments: filter "isNotDeclared"`,
		`other/queries.memql: line 2: query others: filter "alsoNotDeclared && a==1"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q:\n%v", want, err)
		}
	}
}

func TestExpressionsRewriteThroughTheCLI(t *testing.T) {
	root := t.TempDir()
	for rel, content := range xmTree() {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	checked, _, err := runMigrate(t, "--rewrite=expressions", "-check", root)
	if !errors.Is(err, errChanged) {
		t.Fatalf("-check err = %v, want errChanged", err)
	}
	for _, rel := range []string{"common/traits.memql", "deploy/queries.memql", "deploy/specs.memql"} {
		if !strings.Contains(checked, filepath.Join(root, filepath.FromSlash(rel))) {
			t.Errorf("-check does not list %s:\n%s", rel, checked)
		}
	}
	if strings.Contains(checked, "shapes.memql") {
		t.Errorf("-check lists a file that does not change:\n%s", checked)
	}
	if _, stderr, err := runMigrate(t, "--rewrite=expressions", "-w", root); err != nil {
		t.Fatalf("-w: %v\n%s", err, stderr)
	}
	got, err := os.ReadFile(filepath.Join(root, "deploy", "queries.memql"))
	if err != nil || !strings.Contains(string(got), "filter  row => row.id == args.id && requiresOwner(actor) && isActiveRecord(row)") {
		t.Errorf("written queries.memql = %q, %v", got, err)
	}
	// Idempotent: a second run changes nothing.
	if out, _, err := runMigrate(t, "--rewrite=expressions", "-check", root); err != nil || out != "" {
		t.Errorf("second -check = %q, %v; want nothing to change", out, err)
	}
}

// The real tree migrates without a refusal. The run is read-only: the tree is
// loaded into memory the way the CLI loads it and nothing is written back.
// After the tree is migrated (memql#5368's next task) the rewrite is a no-op
// here and this stays green.
func TestExpressionsRewriteTheRealTree(t *testing.T) {
	root := filepath.Join("..", "..", "dsl")
	files := map[string][]byte{}
	err := fs.WalkDir(os.DirFS(root), ".", func(rel string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || !d.Type().IsRegular() {
			return werr
		}
		data, err := fs.ReadFile(os.DirFS(root), rel)
		files[rel] = data
		return err
	})
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	if len(files) == 0 {
		t.Fatalf("read no files under %s; the real-tree run would pass vacuously", root)
	}
	out, err := rewriteExpressions(root, files)
	if err != nil {
		t.Fatalf("the real tree refuses:\n%v", err)
	}
	t.Logf("%d of %d files would change", len(out), len(files))
}

// writeTree writes files under a fresh directory and returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A bundle's spec over the core @actor shape actorEnvelope is an actor
// predicate, though the bundle declares no such shape: the tree is read over
// the core tree it loads over. Before, the spec came out `= row => row.role`,
// which a real engine refuses at define -- and a second run would keep it.
func TestExpressionsBundleBindsACoreActorShape(t *testing.T) {
	root := writeTree(t, map[string]string{
		"fylo/concepts.memql": "@namespace(\"fylo\")\nconcept order {\n  status  string\n}\n",
		"fylo/specs.memql":    "use common.shapes.{ actorEnvelope }\n\nspec actorEnvelope isAdmin {\n  return role == \"admin\"\n}\n",
		"fylo/queries.memql": "use fylo.concepts.{ order }\nuse fylo.specs.{ isAdmin }\nuse common.traits.{ isActiveRecord }\n\n" +
			"query order orders {\n  filter  status == \"open\" && isActiveRecord || isAdmin\n}\n",
	})
	if _, stderr, err := runMigrate(t, "--rewrite=expressions", "-w", root); err != nil {
		t.Fatalf("-w: %v\n%s", err, stderr)
	}
	for rel, want := range map[string]string{
		"fylo/specs.memql":   "spec actorEnvelope isAdmin = actor => actor.role == \"admin\"\n",
		"fylo/queries.memql": "  filter  row => row.status == \"open\" && isActiveRecord(row) || isAdmin(actor)\n",
	} {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil || !strings.Contains(string(got), want) {
			t.Errorf("%s = %q, %v; want it to contain %q", rel, got, err, want)
		}
	}
}

// A spec whose binding nothing declares -- not the tree, not the core tree, not
// any signature -- is refused by name rather than guessed a row.
func TestExpressionsRefusesABindingNothingDeclares(t *testing.T) {
	files := xmTree()
	files["deploy/specs.memql"] = []byte("spec gadget isShiny {\n  return shiny == true\n}\n")
	out, err := rewriteExpressions("root", files)
	if err == nil {
		t.Fatalf("want a refusal, got %v", out)
	}
	if want := "deploy/specs.memql: line 1: spec isShiny binds gadget, which no shape and no concept declares"; !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not name the binding:\n%v", err)
	}
}

// The legacy `<boundConcept>.<field>` spelling is that row's field:
// `todo.ownerUserId` in a query bound to todo is `row.ownerUserId`, not
// `row.todo.ownerUserId`.
func TestExpressionsBoundConceptPrefixThroughTheCLI(t *testing.T) {
	root := writeTree(t, map[string]string{
		"todos/queries.memql": "@actor\nquery todo sandboxOwned {\n  filter todo.ownerUserId == actor.userId\n}\n",
	})
	if _, stderr, err := runMigrate(t, "--rewrite=expressions", "-w", root); err != nil {
		t.Fatalf("-w: %v\n%s", err, stderr)
	}
	got, err := os.ReadFile(filepath.Join(root, "todos", "queries.memql"))
	if want := "@actor\nquery todo sandboxOwned {\n  filter row => row.ownerUserId == actor.userId\n}\n"; err != nil || string(got) != want {
		t.Errorf("queries.memql = %q, %v; want %q", got, err, want)
	}
}
