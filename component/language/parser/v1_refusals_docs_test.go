package parser

// v1_refusals_docs_test.go -- the language reference lists the retired
// spellings from V1RetiredForms rather than restating them: the table between
// the markers in docs/public/language/memql.md is generated, and this test
// holds it to the parser. After a change to the table, run
//
//	go test github.com/znasllc-io/memql/component/language/parser -run TestV1RetiredFormsArePublished -update-docs

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

var updateDocs = flag.Bool("update-docs", false, "rewrite the generated retired-spellings table in docs/public/language/memql.md")

// TestV1RetiredFormsArePublished: the reference's retired-spellings table is
// V1RetiredForms, row for row, in the table's own order.
func TestV1RetiredFormsArePublished(t *testing.T) {
	path := "../../../docs/public/language/memql.md"
	region := "retired spellings"
	want := renderRetiredFormsTable()
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
	t.Errorf("the %q table in %s is not V1RetiredForms; regenerate it with\n\tgo test github.com/znasllc-io/memql/component/language/parser -run TestV1RetiredFormsArePublished -update-docs\nwant:\n%s\ngot:\n%s", region, path, body, got)
}

// retiredChoice matches the one kind of replacement the table writes as a
// choice between two forms: "<a> for <use> or <b> for <use>".
var retiredChoice = regexp.MustCompile(`^(.+?) for (.+?) or (.+?) for (.+)$`)

// renderRetiredFormsTable renders V1RetiredForms: the spelling as an author
// wrote it and what to write instead, each form in a code span and each
// connective named in words.
func renderRetiredFormsTable() string {
	span := func(s string) string { return "`" + strings.ReplaceAll(s, "|", `\|`) + "`" }
	var b strings.Builder
	b.WriteString("| Retired | Write instead |\n")
	b.WriteString("|---|---|\n")
	for _, f := range V1RetiredForms() {
		spelling := span(f.Spelling)
		if glyph, ok := strings.CutSuffix(f.Spelling, " as a connective"); ok {
			spelling = span(glyph) + " as a connective"
		}
		replacement := span(f.Replacement)
		if m := retiredChoice.FindStringSubmatch(f.Replacement); m != nil {
			replacement = fmt.Sprintf("%s for %s, %s for %s", span(m[1]), m[2], span(m[3]), m[4])
		}
		fmt.Fprintf(&b, "| %s | %s |\n", spelling, replacement)
	}
	return b.String()
}
