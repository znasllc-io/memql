package dslspec

import (
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
)

// The page's content is held to the registry by the root package
// (TestAttributeMatrixListsWhatTheParserAccepts reads the committed page).
// These tests hold the renderer's own promises: every receiver has a column,
// every argument form has a word, every link lands on a heading, every table
// is rectangular, and registry text cannot break the markdown.

// TestMatrixFamiliesCoverEveryReceiver: every receiver sits in exactly one
// column, so a receiver the registry gains cannot be left off the page.
func TestMatrixFamiliesCoverEveryReceiver(t *testing.T) {
	seen := map[annotations.Receiver]int{}
	for _, fam := range matrixFamilies {
		for _, col := range fam.columns {
			for _, r := range col {
				seen[r]++
			}
		}
	}
	for _, r := range annotations.Receivers() {
		if seen[r] != 1 {
			t.Errorf("receiver %s sits in %d columns of the attribute matrix, want 1 -- add it to matrixFamilies", r, seen[r])
		}
		delete(seen, r)
	}
	for r := range seen {
		t.Errorf("matrixFamilies names %q, which is not a registry receiver", r)
	}
}

// TestMatrixFormWordsCoverEveryForm: every argument form the registry defines
// has a short word for a matrix cell. A form is defined when Form.String reads
// it as words rather than as a bit pattern.
func TestMatrixFormWordsCoverEveryForm(t *testing.T) {
	words := map[annotations.Form]bool{}
	for _, fw := range formWords {
		words[fw.form] = true
	}
	for bit := 0; bit < 16; bit++ {
		f := annotations.Form(1 << bit)
		if strings.Contains(f.String(), "Form(") {
			continue // not a form the registry defines
		}
		if !words[f] {
			t.Errorf("the form %q has no short word in formWords, so its cells would print the long form", f.String())
		}
	}
	if got := shortForm(annotations.FormString | annotations.FormStrings); got != "strings" {
		t.Errorf("one or more strings reads %q, want strings", got)
	}
	if got := shortForm(annotations.FormNumber | annotations.FormKeywords); got != "number or keywords" {
		t.Errorf("a number or keywords reads %q", got)
	}
}

// TestReceiverNameIsTheConstructKeyword: a construct is named by the keyword an
// author types, and a field receiver by the kind of field.
func TestReceiverNameIsTheConstructKeyword(t *testing.T) {
	for r, want := range map[annotations.Receiver]string{
		annotations.Mutation:    "mutate",
		annotations.Spec:        "spec and trait",
		annotations.ConceptBody: "concept",
		annotations.Concept:     "concept",
		annotations.ArgsField:   "args field",
		annotations.ToolField:   "tool field",
	} {
		if got := ReceiverName(r); got != want {
			t.Errorf("ReceiverName(%s) = %q, want %q", r, got, want)
		}
	}
}

// TestMatrixLinksLandOnHeadings: every in-page link names the anchor of a
// heading on the page, computed the way GitHub computes it.
func TestMatrixLinksLandOnHeadings(t *testing.T) {
	page := AttributeMatrix()
	anchors := map[string]bool{}
	var s slugger
	inFrontMatter := false
	for i, line := range strings.Split(page, "\n") {
		if line == "---" && (i == 0 || inFrontMatter) {
			inFrontMatter = !inFrontMatter
			continue
		}
		if inFrontMatter || !strings.HasPrefix(line, "#") {
			continue
		}
		text := strings.TrimLeft(line, "#")
		if !strings.HasPrefix(text, " ") {
			t.Errorf("heading %q has no space after its hashes", line)
			continue
		}
		anchors[s.slug(strings.TrimSpace(text))] = true
	}
	links := regexp.MustCompile(`\]\(#([^)]*)\)`).FindAllStringSubmatch(page, -1)
	if len(links) < 100 {
		t.Fatalf("only %d in-page links -- the page stopped linking its names", len(links))
	}
	for _, m := range links {
		if !anchors[m[1]] {
			t.Errorf("the page links to #%s, which no heading carries", m[1])
		}
	}
	// The one heading GitHub has to disambiguate: the policy construct comes
	// first, so the rule's @policy entry is policy-1.
	if !strings.Contains(page, "[`@policy`](#policy-1)") || !strings.Contains(page, "[policy](#policy)") {
		t.Error("the policy construct and the @policy annotation do not link to their own anchors")
	}
}

// TestMatrixHasNoRawHTML: outside code spans, no "<" reaches the page except
// the GENERATED comment. A registry doc's <field> placeholder written raw is an
// HTML tag to a markdown renderer, and GitHub drops it silently.
func TestMatrixHasNoRawHTML(t *testing.T) {
	for i, line := range strings.Split(AttributeMatrix(), "\n") {
		if strings.HasPrefix(line, "<!-- GENERATED") {
			continue
		}
		if strings.Contains(stripCodeSpans(line), "<") {
			t.Errorf("line %d carries a raw \"<\" outside a code span: %s", i+1, line)
		}
	}
}

// stripCodeSpans removes the code spans from a line, which print "<" as text.
// It walks them with the renderer's own codeSegments, so a span of any
// backtick length is read the way the renderer reads it.
func stripCodeSpans(line string) string {
	var b strings.Builder
	codeSegments(line, func(segment string, code bool) {
		if !code {
			b.WriteString(segment)
		}
	})
	return b.String()
}

// TestMatrixHasNoWhitespaceOnlyLines: no line of the page ends in whitespace,
// which includes a blank line inside a list item. A multi-paragraph doc in an
// entry whose receivers' docs differ is the case that would produce one.
func TestMatrixHasNoWhitespaceOnlyLines(t *testing.T) {
	for i, line := range strings.Split(AttributeMatrix(), "\n") {
		if strings.TrimRight(line, " \t") != line {
			t.Errorf("line %d ends in whitespace: %q", i+1, line)
		}
	}
	item := indentContinuation("On a query: first paragraph.\n\nsecond paragraph\n- a nested item")
	if want := "On a query: first paragraph.\n\n  second paragraph\n  - a nested item"; item != want {
		t.Errorf("indentContinuation = %q, want %q", item, want)
	}
}

// TestMatrixLinksDocPages: a docs/public page a registry doc names by its
// repository path is a link relative to the page on the published page, and no
// bare path is left outside a code span.
func TestMatrixLinksDocPages(t *testing.T) {
	page := AttributeMatrix()
	if !strings.Contains(page, "[per-row-authz-audit.md](../operate/auth/per-row-authz-audit.md)") {
		t.Error("the @rowAuthz doc's docs/public/operate/auth/per-row-authz-audit.md is not a relative link")
	}
	for i, line := range strings.Split(page, "\n") {
		if strings.Contains(stripCodeSpans(line), "(docs/public/") || strings.Contains(stripCodeSpans(line), " docs/public/") {
			t.Errorf("line %d names a docs/public path a reader cannot follow: %s", i+1, line)
		}
	}
}

func TestMatrixIsDeterministic(t *testing.T) {
	if AttributeMatrix() != AttributeMatrix() {
		t.Error("two renders of the same registry differ")
	}
}

// TestSlugMatchesGitHub pins the anchor rule to github-slugger's: lower-case,
// drop punctuation, spaces to hyphens, and a repeat suffixed -1, -2.
func TestSlugMatchesGitHub(t *testing.T) {
	var s slugger
	for _, tc := range []struct{ heading, want string }{
		{"MemQL attribute matrix", "memql-attribute-matrix"},
		{"@addToSet", "addtoset"},
		{"spec and trait", "spec-and-trait"},
		{"policy", "policy"},
		{"@policy", "policy-1"},
		{"policy", "policy-2"},
		{"policy-1", "policy-1-1"},
		{"How to read it", "how-to-read-it"},
	} {
		if got := s.slug(tc.heading); got != tc.want {
			t.Errorf("slug(%q) = %q, want %q", tc.heading, got, tc.want)
		}
	}
}

func TestRegistryText(t *testing.T) {
	for _, tc := range []struct {
		in     string
		inCell bool
		want   string
	}{
		{"owner=\"<field>\"", false, "owner=\"&lt;field>\""},
		{"`use <module>.{ a }`", false, "`use <module>.{ a }`"},
		{"a ` <b>", false, "a ` &lt;b>"},
		{"``a ` <b>`` <c>", false, "``a ` <b>`` &lt;c>"},
		{"read|write", true, `read\|write`},
		{"`read|write`", true, "`read\\|write`"},
		{"read|write", false, "read|write"},
		{"two\nlines", true, "two lines"},
		{"**bold** stays", false, "**bold** stays"},
		{"See docs/public/operate/auth/per-row-authz-audit.md.", false, "See [per-row-authz-audit.md](../operate/auth/per-row-authz-audit.md)."},
		{"See docs/public/language/authoring-rules.md#pagination", false, "See [authoring-rules.md](authoring-rules.md#pagination)"},
		{"`docs/public/language/memql.md` stays code", false, "`docs/public/language/memql.md` stays code"},
		{"[the page](docs/public/language/memql.md)", false, "[the page](docs/public/language/memql.md)"},
		{"a|b in docs/public/concepts/data-origins.md", true, `a\|b in [data-origins.md](../concepts/data-origins.md)`},
	} {
		if got := registryText(tc.in, tc.inCell); got != tc.want {
			t.Errorf("registryText(%q, %v) = %q, want %q", tc.in, tc.inCell, got, tc.want)
		}
	}
}
