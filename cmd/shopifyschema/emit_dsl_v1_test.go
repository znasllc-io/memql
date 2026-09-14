package main

// emit_dsl_v1_test.go -- the generated reads in the edition-2026 expression
// grammar (epic memql#5363, memql#5368).

import (
	"path/filepath"
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
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

// TestEmitReadsV1Parses: the generated reads parse, the filter a lambda over
// the row.
func TestEmitReadsV1Parses(t *testing.T) {
	_, plans := recordedPlans(t)
	p := plans[0]

	src, err := langparser.NormaliseAll(emitReads(p))
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	f, err := langparser.ParseFile(src)
	if err != nil {
		t.Fatalf("the reads do not parse: %v\n%s", err, emitReads(p))
	}
	queries := 0
	for _, d := range f.Definitions {
		if fn, ok := d.(*langparser.FunctionDef); ok && fn.Type == langparser.FunctionTypeQuery {
			queries++
		}
	}
	if queries != 2 {
		t.Fatalf("parsed %d queries, want the two reads", queries)
	}
	if !strings.Contains(emitReads(p), "filter  row => row.storeId == args.storeId") {
		t.Fatalf("the by-GID filter is not the lambda form:\n%s", emitReads(p))
	}
}
