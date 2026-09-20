package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
	"github.com/znasllc-io/memql/component/language/dslspec"
)

// attribute_matrix_parity_test.go -- memql#5360.
//
// docs/public/language/attribute-matrix.md is GENERATED from the annotation
// registry (component/language/annotations), the one table the construct
// parsers and the concept loader check annotations against. It replaces a hand
// page whose parity test (#2712) pinned three columns of one table and nothing
// else, so the page went on listing three policy annotations the parser had
// refused since memql#5127. The two tests below close both halves:
//
//   - TestAttributeMatrixIsGenerated holds the committed page to the renderer,
//     byte for byte, so a registry change that is not regenerated fails, and so
//     does a hand edit of the page.
//   - TestAttributeMatrixListsWhatTheParserAccepts holds the committed page to
//     the REGISTRY rather than to the renderer, so a renderer that drops a
//     placement or lists a refused name cannot pass the first test by agreeing
//     with itself.

// TestAttributeMatrixIsGenerated compares the committed page with what the
// registry renders. The page is derived, so a stale page is a diff rather than
// a judgement call, and hand-editing it is always the wrong repair.
func TestAttributeMatrixIsGenerated(t *testing.T) {
	raw, err := os.ReadFile(dslspec.AttributeMatrixPath)
	if err != nil {
		t.Fatalf("%s is missing (%v); generate it with `make docs-matrix`", dslspec.AttributeMatrixPath, err)
	}
	want := dslspec.AttributeMatrix()
	if string(raw) == want {
		return
	}
	line, committed, rendered := firstDifferentLine(string(raw), want)
	t.Errorf("%s is stale against the annotation registry.\n\n"+
		"First difference, line %d:\n  committed: %q\n  rendered:  %q\n\n"+
		"Run `make docs-matrix` and commit the result. The page is generated from "+
		"component/language/annotations, so a hand edit is always the wrong repair: "+
		"change the registry (a placement, its example or its doc) and regenerate.",
		dslspec.AttributeMatrixPath, line, committed, rendered)
}

// firstDifferentLine returns the 1-based number of the first line on which a
// and b differ, and that line from each ("" past the end of one of them).
func firstDifferentLine(a, b string) (int, string, string) {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y || i >= len(al) || i >= len(bl) {
			return i + 1, x, y
		}
	}
	return 0, "", ""
}

// TestAttributeMatrixListsWhatTheParserAccepts reads the committed page's three
// listings -- the At a glance matrices, the By construct tables and the
// per-annotation tables -- and holds each to the registry:
//
//   - every placement appears in its construct's By construct section, in its
//     column of the family matrix, and in its own annotation entry;
//   - nothing appears as accepted where the check refuses it. A retired name
//     (@internal on a query, @scope on a concept) is the case this exists for:
//     it may appear only in the Retired table, never as accepted.
func TestAttributeMatrixListsWhatTheParserAccepts(t *testing.T) {
	raw, err := os.ReadFile(dslspec.AttributeMatrixPath)
	if err != nil {
		t.Fatalf("read %s: %v", dslspec.AttributeMatrixPath, err)
	}
	tables := matrixTables(string(raw))

	// A column title or section heading names one construct or kind of field;
	// the concept's names two receivers (the concept and its body).
	receiversNamed := map[string][]annotations.Receiver{}
	for _, r := range annotations.Receivers() {
		name := dslspec.ReceiverName(r)
		receiversNamed[name] = append(receiversNamed[name], r)
	}

	type listing struct {
		where string                                   // the page section: "By construct", "At a glance", "Annotations"
		lists map[annotations.Receiver]map[string]bool // receiver -> names the page lists as accepted there
	}
	newListing := func(where string) *listing {
		return &listing{where: where, lists: map[annotations.Receiver]map[string]bool{}}
	}
	// record notes that the page lists name as accepted by the construct
	// titled title, and fails when none of that construct's receivers accepts
	// it -- naming the code the check refuses it with. Every receiver of the
	// construct that accepts the name is recorded, not only the first: a name
	// the concept accepted both before the braces and inside them would
	// otherwise read as missing from one of the two.
	record := func(l *listing, title, name string) {
		receivers := receiversNamed[title]
		if len(receivers) == 0 {
			t.Errorf("%s: %q names no construct or kind of field the registry knows", l.where, title)
			return
		}
		accepted := false
		for _, r := range receivers {
			if _, ok := annotations.Lookup(r, name); ok {
				if l.lists[r] == nil {
					l.lists[r] = map[string]bool{}
				}
				l.lists[r][name] = true
				accepted = true
			}
		}
		if accepted {
			return
		}
		code := "no refusal"
		if ref := annotations.Check(receivers[0], annotations.Use{Name: name}); ref != nil {
			code = ref.Code
		}
		t.Errorf("%s lists @%s as accepted on %s, but the registry refuses it there [%s]",
			l.where, name, title, code)
	}

	byConstruct := newListing("By construct")
	glance := newListing("At a glance")
	entries := newListing("Annotations")
	for _, tb := range tables {
		// Every row has its header's number of cells, counted on the pipes GFM
		// splits on: a "|" in a doc or an example that escaped the escaping
		// shows up here as a row that is too wide.
		for _, row := range tb.rows {
			if len(row) != len(tb.header) {
				t.Errorf("%s, %s: a row has %d cells under a %d-cell header: %q",
					tb.section, tb.subsection, len(row), len(tb.header), row)
			}
		}
		switch tb.section {
		case "By construct":
			for _, row := range tb.rows {
				if name := annotationIn(row[0]); name != "" {
					record(byConstruct, tb.subsection, name)
				} else {
					t.Errorf("By construct, %s: a row names no annotation: %q", tb.subsection, row[0])
				}
			}
		case "At a glance":
			for _, row := range tb.rows {
				name := annotationIn(row[0])
				if name == "" {
					t.Errorf("At a glance, %s: a row names no annotation: %q", tb.subsection, row[0])
					continue
				}
				for i := 1; i < len(row) && i < len(tb.header); i++ {
					if strings.TrimSpace(row[i]) != "" {
						record(glance, tb.header[i], name)
					}
				}
			}
		case "Annotations":
			name := strings.TrimPrefix(tb.subsection, "@")
			for _, row := range tb.rows {
				if len(tb.header) == 0 || tb.header[0] != "On" {
					continue // a keys table
				}
				for _, title := range linkTexts(row[0]) {
					record(entries, title, name)
				}
			}
		}
	}

	for _, l := range []*listing{byConstruct, glance, entries} {
		if len(l.lists) == 0 {
			t.Errorf("found no %s listing in %s -- the page's layout changed under this test", l.where, dslspec.AttributeMatrixPath)
			continue
		}
		for _, p := range annotations.Placements() {
			if !l.lists[p.Receiver][p.Name] {
				t.Errorf("%s does not list @%s on %s, which the registry accepts (%s)",
					l.where, p.Name, dslspec.ReceiverName(p.Receiver), p.Example)
			}
		}
	}
}

// matrixTable is one GFM table on the page, with the headings it sits under.
type matrixTable struct {
	section, subsection string // the enclosing "## " and "### " headings
	header              []string
	rows                [][]string
}

// matrixTables reads every table on the page. A table is a run of lines
// starting with "|": the first is its header, the second its delimiter row.
func matrixTables(page string) []matrixTable {
	var (
		out                 []matrixTable
		section, subsection string
		current             *matrixTable
	)
	for _, line := range strings.Split(page, "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			section, subsection = strings.TrimPrefix(line, "## "), ""
		case strings.HasPrefix(line, "### "):
			subsection = strings.TrimPrefix(line, "### ")
		}
		if !strings.HasPrefix(line, "|") {
			if current != nil {
				out = append(out, *current)
				current = nil
			}
			continue
		}
		cells := tableCells(line)
		switch {
		case current == nil:
			current = &matrixTable{section: section, subsection: subsection, header: cells}
		case isDelimiterRow(cells):
		default:
			current.rows = append(current.rows, cells)
		}
	}
	if current != nil {
		out = append(out, *current)
	}
	return out
}

// tableCells splits one table row on the pipes GFM splits on: every "|" that
// is not escaped as "\|".
func tableCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	var (
		cells []string
		cell  strings.Builder
	)
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line) && line[i+1] == '|':
			cell.WriteByte('|')
			i++
		case line[i] == '|':
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()
		default:
			cell.WriteByte(line[i])
		}
	}
	return append(cells, strings.TrimSpace(cell.String()))
}

func isDelimiterRow(cells []string) bool {
	for _, c := range cells {
		if strings.Trim(c, ":-") != "" || c == "" {
			return false
		}
	}
	return len(cells) > 0
}

var (
	annotationCell = regexp.MustCompile("`@([A-Za-z][A-Za-z0-9_]*)`")
	markdownLink   = regexp.MustCompile(`\[([^\]]+)\]\(#[^)]*\)`)
)

// annotationIn returns the annotation a row's first cell names, or "".
func annotationIn(cell string) string {
	if m := annotationCell.FindStringSubmatch(cell); m != nil {
		return m[1]
	}
	return ""
}

// linkTexts returns the text of every in-page link in a cell.
func linkTexts(cell string) []string {
	var out []string
	for _, m := range markdownLink.FindAllStringSubmatch(cell, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestGrammarPageIsGenerated and TestVocabularyPageIsGenerated are the
// attribute matrix's siblings for the two pages memql#5388 added: the EBNF
// authoring grammar and the vocabulary. They live in this file, in this shape,
// on purpose -- a reader who has understood why the matrix is generated has
// understood why these are, and a second shape for the same rule is a second
// thing to learn.
//
// Both pages exist to be handed to a MODEL. That is what makes staleness worse
// here than on a page a human reads: a stale sentence a person reads is
// confusing, and a stale PRODUCTION is a form the model emits confidently and
// the parser refuses.

// TestGrammarPageIsGenerated compares the committed grammar with what the
// language tables render.
func TestGrammarPageIsGenerated(t *testing.T) {
	assertGeneratedPage(t, dslspec.GrammarPath, dslspec.RenderGrammar(),
		"the parser's construct and clause tables, the annotation registry and the function catalog")
}

// TestVocabularyPageIsGenerated compares the committed vocabulary with what
// the language tables render.
func TestVocabularyPageIsGenerated(t *testing.T) {
	assertGeneratedPage(t, dslspec.VocabularyPath, dslspec.RenderVocabulary(),
		"the doc each language table carries for the names it owns")
}

// assertGeneratedPage holds one committed page to its renderer, byte for byte,
// and reports the first line they differ on.
func assertGeneratedPage(t *testing.T, path, want, derivedFrom string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s is missing (%v); generate it with `make docs-grammar`", path, err)
	}
	if string(raw) == want {
		return
	}
	line, committed, rendered := firstDifferentLine(string(raw), want)
	t.Errorf("%s is stale against %s.\n\n"+
		"First difference, line %d:\n  committed: %q\n  rendered:  %q\n\n"+
		"Run `make docs-grammar` and commit the result. The page is generated, so a hand edit is always "+
		"the wrong repair: change the TABLE (component/language/{parser,annotations,functions,dslspec}) "+
		"and regenerate.",
		path, derivedFrom, line, committed, rendered)
}
