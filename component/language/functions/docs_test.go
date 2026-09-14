package functions

// docs_test.go -- two tables in the language docs are generated from this
// package, and these tests hold them to it: the operator table in
// docs/public/language/memql.md (from Operators) and the function catalog in
// docs/public/language/functions.md (from Catalog). Neither is restated by
// hand: after a change here, run
//
//	go test github.com/znasllc-io/memql/component/language/functions -run Published -update-docs
//
// and the rows between each pair of markers are rewritten.

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
)

var updateDocs = flag.Bool("update-docs", false, "rewrite the generated tables in docs/public/language")

const regenerate = "go test github.com/znasllc-io/memql/component/language/functions -run Published -update-docs"

// TestOperatorTableIsPublished: the reference's operator table is Operators(),
// row for row.
func TestOperatorTableIsPublished(t *testing.T) {
	checkGeneratedRegion(t, "../../../docs/public/language/memql.md", "operators", renderOperatorTable(), regenerate)
}

// TestCatalogIsPublished: the function reference's catalog table is Catalog(),
// entry for entry.
func TestCatalogIsPublished(t *testing.T) {
	checkGeneratedRegion(t, "../../../docs/public/language/functions.md", "function catalog", renderCatalogTable(), regenerate)
}

// renderOperatorTable renders Operators() in precedence order: how each is
// written, the form a hover opens with, what it does, and what it does with
// an unset or absent operand where the record's table has a rule.
func renderOperatorTable() string {
	var b strings.Builder
	b.WriteString("| Operator | Form | What it does | Unset or absent operand |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, op := range Operators() {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", code(op.Symbol), code(op.Form), cell(op.Doc), cell(op.Absence))
	}
	return b.String()
}

// renderCatalogTable renders Catalog() in its own order -- functions by name,
// then methods by receiver and name -- with where each entry runs and the
// retired spellings it replaces.
func renderCatalogTable() string {
	var b strings.Builder
	b.WriteString("| Signature | Runs | What it does | Replaces |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, f := range Catalog() {
		replaces := make([]string, len(f.Retired))
		for i, r := range f.Retired {
			replaces[i] = code(r)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", code(f.Signature()), runs(f), cell(f.Doc), strings.Join(replaces, ", "))
	}
	return b.String()
}

// runs says where a catalog entry runs. A traversal selects rows, which only
// SQL does; the other pushdown entries lower when their input is a row field
// and run in process on any other value; an in-process entry runs in process
// everywhere, which in a filter or spec means on values computed before the
// query.
func runs(f Function) string {
	switch {
	case f.Returns == TypeRows:
		return "Pushed down"
	case f.Tier == TierP:
		return "Pushed down on a row field, in process otherwise"
	}
	return "In process"
}

// code renders text as a table-safe code span.
func code(text string) string {
	return "`" + strings.ReplaceAll(text, "|", `\|`) + "`"
}

// cell makes prose table-safe: a pipe would end the cell.
func cell(text string) string {
	return strings.ReplaceAll(text, "|", `\|`)
}

// checkGeneratedRegion compares the content between a region's markers with
// want, or -- under -update-docs -- rewrites it.
func checkGeneratedRegion(t *testing.T, path, region, want, command string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	begin := "<!-- BEGIN GENERATED: " + region + "."
	end := "<!-- END GENERATED: " + region + " -->"
	text := string(raw)
	b := strings.Index(text, begin)
	if b < 0 {
		t.Fatalf("%s has no %q marker", path, begin)
	}
	open := b + strings.Index(text[b:], "\n") + 1
	e := strings.Index(text[open:], end)
	if e < 0 {
		t.Fatalf("%s has no %q marker after %q", path, end, begin)
	}
	got := text[open : open+e]
	body := "\n" + want + "\n"
	if got == body {
		return
	}
	if *updateDocs {
		if err := os.WriteFile(path, []byte(text[:open]+body+text[open+e:]), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote the %q region of %s", region, path)
		return
	}
	t.Errorf("the %q table in %s is not what the code says; regenerate it with\n\t%s\nwant:\n%s\ngot:\n%s", region, path, command, body, got)
}
