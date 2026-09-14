package main

// emit_dsl_v1_test.go -- the generated reads in the edition-2026 expression
// grammar (epic memql#5363, memql#5368). The generator writes the grammar the
// engine parses with (langparser.DefaultOptions); these tests pin the v1
// spelling it will write once that switch flips, without the flip.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/repowalk"
)

// recordedPlans plans every allowlisted type from the recorded fixture, as the
// drift gate does.
func recordedPlans(t *testing.T) (string, []*TypePlan) {
	t.Helper()
	repo := repoRoot()
	list, err := LoadAllowlist(filepath.Join(repo, "cmd", "shopifyschema", "allowlist.yaml"))
	if err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	schema, err := ReadSchemaFile(filepath.Join(repo, "cmd", "shopifyschema", "testdata", "schema-"+list.APIVersion+".json"))
	if err != nil {
		t.Fatalf("recorded schema: %v", err)
	}
	plans, err := NewPlanner(schema, list).Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plans) == 0 {
		t.Fatal("the recorded fixture plans no types")
	}
	return list.APIVersion, plans
}

// corpusPredicates collects every spec and trait the tree declares -- the set
// the expressions rewrite resolves a bare predicate conjunct against.
func corpusPredicates(t *testing.T) map[string]langparser.PredicateInfo {
	t.Helper()
	files := map[string][]byte{}
	root := filepath.Join(repoRoot(), "dsl")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && repowalk.SkipDir(d.Name()) {
			return filepath.SkipDir
		}
		if d.IsDir() || filepath.Ext(path) != ".memql" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[path] = b
		return nil
	})
	if err != nil {
		t.Fatalf("read the dsl tree: %v", err)
	}
	preds, err := langparser.CollectPredicates(files)
	if err != nil {
		t.Fatalf("collect predicates: %v", err)
	}
	if _, ok := preds["isNotDeleted"]; !ok {
		t.Fatal("the corpus declares no isNotDeleted trait; the forStore read applies it")
	}
	return preds
}

// TestEmitReadsV1IsTheExpressionsRewrite: for every generated file, the
// edition-2026 output is byte for byte what `memqlmigrate
// --rewrite=expressions` makes of the legacy output. A regenerated tree and a
// migrated one therefore cannot differ, and the flip may do either.
func TestEmitReadsV1IsTheExpressionsRewrite(t *testing.T) {
	version, plans := recordedPlans(t)
	preds := corpusPredicates(t)
	for _, p := range plans {
		legacy := emitConceptFile(version, p, false)
		v1 := emitConceptFile(version, p, true)
		if legacy == v1 {
			t.Fatalf("%s: the two grammars emit the same file", p.Concept)
		}
		migrated, err := langparser.RewriteExpressions([]byte(legacy), preds)
		if err != nil {
			t.Fatalf("%s: the expressions rewrite refused the legacy output: %v", p.Concept, err)
		}
		if string(migrated) != v1 {
			t.Fatalf("%s: the v1 output differs from the rewrite of the legacy output.\n%s", p.Concept, firstDiff(string(migrated), v1))
		}
	}
}

// TestEmitReadsV1Parses: the edition-2026 reads parse with the v1 grammar on,
// the filter a lambda over the row; the legacy reads are refused under it --
// which is why the generator follows the switch rather than writing either
// grammar unconditionally.
func TestEmitReadsV1Parses(t *testing.T) {
	_, plans := recordedPlans(t)
	p := plans[0]
	on := langparser.Options{ExpressionsV1: true}

	src, err := langparser.NormaliseAll(emitReads(p, true))
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	f, err := langparser.ParseFileWithOptions(src, on)
	if err != nil {
		t.Fatalf("the v1 reads do not parse with ExpressionsV1 on: %v\n%s", err, emitReads(p, true))
	}
	queries := 0
	for _, d := range f.Definitions {
		if fn, ok := d.(*langparser.FunctionDef); ok && fn.ExpressionsV1 {
			queries++
		}
	}
	if queries != 2 {
		t.Fatalf("parsed %d v1 queries, want the two reads", queries)
	}
	if !strings.Contains(emitReads(p, true), "filter  row => row.storeId == args.storeId") {
		t.Fatalf("the by-GID filter is not the lambda form:\n%s", emitReads(p, true))
	}

	legacy, err := langparser.NormaliseAll(emitReads(p, false))
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	if _, err := langparser.ParseFileWithOptions(legacy, on); err == nil {
		t.Fatal("the legacy reads parsed with ExpressionsV1 on; the generator's switch would be moot")
	}
	if _, err := langparser.ParseFileWithOptions(legacy, langparser.Options{}); err != nil {
		t.Fatalf("the legacy reads do not parse with the switch off: %v", err)
	}
}
