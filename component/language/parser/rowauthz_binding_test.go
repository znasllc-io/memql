package parser

import (
	"strings"
	"testing"
)

// parseConceptAttrs parses src and returns the attributes attached to
// the first concept declaration.
func parseConceptAttrs(t *testing.T, src string) []*Attribute {
	t.Helper()
	file, err := ParseFile(src)
	if err != nil {
		t.Fatalf("ParseFile(%q): %v", src, err)
	}
	for _, def := range file.Definitions {
		if cd, ok := def.(*ConceptDecl); ok {
			return cd.Attributes
		}
	}
	t.Fatalf("no concept declaration in %q", src)
	return nil
}

// rowAuthzAttr parses a single annotation line above a stub concept and
// returns the @rowAuthz attribute.
func rowAuthzAttr(t *testing.T, line string) *Attribute {
	t.Helper()
	src := line + "\nconcept probe {\n  ownerUserId string\n}\n"
	for _, a := range parseConceptAttrs(t, src) {
		if a.Name == RowAuthzAnnotation {
			return a
		}
	}
	t.Fatalf("no @%s attribute parsed from %q", RowAuthzAnnotation, line)
	return nil
}

// Each of the four tiers has exactly one accepted spelling, and it
// parses to exactly one meaning.
func TestParseRowAuthzAcceptsEachTier(t *testing.T) {
	cases := []struct {
		line string
		want RowAuthzDecl
	}{
		{`@rowAuthz(public)`, RowAuthzDecl{Tier: RowAuthzPublic}},
		{`@rowAuthz(clusterOwner)`, RowAuthzDecl{Tier: RowAuthzClusterOwner}},
		{`@rowAuthz(owner="ownerUserId")`, RowAuthzDecl{Tier: RowAuthzOwned, Owner: "ownerUserId"}},
		{`@rowAuthz(via="spaceMember")`, RowAuthzDecl{Tier: RowAuthzGranted, Spec: "spaceMember"}},
	}
	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			got, err := ParseRowAuthz(rowAuthzAttr(t, tc.line))
			if err != nil {
				t.Fatalf("ParseRowAuthz(%s): unexpected error: %v", tc.line, err)
			}
			if *got != tc.want {
				t.Fatalf("ParseRowAuthz(%s) = %+v, want %+v", tc.line, *got, tc.want)
			}
		})
	}
}

// Every malformed form is rejected, and the diagnostic says what to
// write instead rather than merely that something is wrong.
func TestParseRowAuthzRejectsMalformedForms(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		wantHint string
	}{
		{"no tier", `@rowAuthz`, "requires a tier"},
		{"empty parens", `@rowAuthz()`, "requires a tier"},
		{"unknown tier", `@rowAuthz(everyone)`, `unknown tier "everyone"`},
		{"bare string value", `@rowAuthz("public")`, "does not take a bare value"},
		{"two tiers", `@rowAuthz(public, clusterOwner)`, "takes exactly one tier"},
		{"owner with no field", `@rowAuthz(owner="")`, "is empty"},
		{"via with no spec", `@rowAuthz(via="")`, "is empty"},
		{"owner as a flag", `@rowAuthz(owner)`, "requires a quoted field name"},
		{"via as a flag", `@rowAuthz(via)`, "requires a quoted spec name"},
		{"public with a value", `@rowAuthz(public="yes")`, "takes no value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRowAuthz(rowAuthzAttr(t, tc.line))
			if err == nil {
				t.Fatalf("ParseRowAuthz(%s): want error, got nil", tc.line)
			}
			if !strings.Contains(err.Error(), tc.wantHint) {
				t.Fatalf("ParseRowAuthz(%s) error = %q, want it to contain %q", tc.line, err, tc.wantHint)
			}
		})
	}
}

// FormatRowAuthz is the codemod's only way to emit a declaration and
// ParseRowAuthz is the loader's only way to read one. If they ever
// disagree, a codemod run writes a tree the loader rejects. This is
// the parser-side half of that guarantee; the loader-side half lives
// in component/database/memory-nodes.
func TestRowAuthzFormatParseRoundTrip(t *testing.T) {
	decls := []RowAuthzDecl{
		{Tier: RowAuthzPublic},
		{Tier: RowAuthzClusterOwner},
		{Tier: RowAuthzOwned, Owner: "ownerUserId"},
		{Tier: RowAuthzOwned, Owner: "requestedBy"},
		{Tier: RowAuthzGranted, Spec: "spaceMember"},
	}
	for _, want := range decls {
		line, err := FormatRowAuthz(want)
		if err != nil {
			t.Fatalf("FormatRowAuthz(%+v): %v", want, err)
		}
		got, err := ParseRowAuthz(rowAuthzAttr(t, line))
		if err != nil {
			t.Fatalf("ParseRowAuthz(%s) after FormatRowAuthz(%+v): %v", line, want, err)
		}
		if *got != want {
			t.Fatalf("round trip of %+v via %q = %+v", want, line, *got)
		}
	}
}

func TestFormatRowAuthzRejectsIncompleteDecls(t *testing.T) {
	cases := []RowAuthzDecl{
		{Tier: RowAuthzOwned},                // no owner field
		{Tier: RowAuthzGranted},              // no spec
		{Tier: RowAuthzTier("everyone")},     // not a tier
		{Tier: RowAuthzOwned, Owner: "   "},  // whitespace is not a field
		{Tier: RowAuthzGranted, Spec: "\t "}, // whitespace is not a spec
	}
	for _, d := range cases {
		if _, err := FormatRowAuthz(d); err == nil {
			t.Fatalf("FormatRowAuthz(%+v): want error, got nil", d)
		}
	}
}

// ---- codemod: header location ----

func TestConceptHeadersFindsEveryDeclaration(t *testing.T) {
	src := `use common.concepts.{ other }

/// A plan.
@displayCard(primary="goal")
concept plan {
  goal string
}

concept task {
  planId string
}
`
	headers := ConceptHeaders(src)
	if len(headers) != 2 {
		t.Fatalf("ConceptHeaders found %d headers, want 2: %+v", len(headers), headers)
	}
	if headers[0].Name != "plan" || headers[1].Name != "task" {
		t.Fatalf("names = %q, %q; want plan, task", headers[0].Name, headers[1].Name)
	}
	// The preamble must reach back over the doc comment AND the
	// annotation, or the idempotency check reads the wrong region.
	if got := src[headers[0].PreambleStart:headers[0].Start]; !strings.Contains(got, "@displayCard") ||
		!strings.Contains(got, "/// A plan.") {
		t.Fatalf("preamble for plan = %q, want it to include the doc comment and @displayCard", got)
	}
	// The body must end at the concept's own closing brace, not run
	// into the next declaration.
	if got := src[headers[0].Start:headers[0].End]; strings.Contains(got, "task") {
		t.Fatalf("plan body leaked into the next concept: %q", got)
	}
}

// The word "concept" inside a comment or a string is prose, not a
// declaration. Matching on raw source would find these.
func TestConceptHeadersIgnoresCommentsAndStrings(t *testing.T) {
	src := `/// This doc comment mentions:
/// concept ghostFromDocComment {
// concept ghostFromLineComment {
/*
concept ghostFromBlockComment {
*/
@description("see concept ghostFromString { for details")
concept real {
  name string
}
`
	headers := ConceptHeaders(src)
	if len(headers) != 1 {
		names := make([]string, 0, len(headers))
		for _, h := range headers {
			names = append(names, h.Name)
		}
		t.Fatalf("ConceptHeaders found %d headers (%v), want only `real`", len(headers), names)
	}
	if headers[0].Name != "real" {
		t.Fatalf("name = %q, want real", headers[0].Name)
	}
}

// An escaped quote must not end a string early -- otherwise the text
// after it is scanned as code.
func TestConceptHeadersHandlesEscapedQuotes(t *testing.T) {
	src := `@description("a \" quote then concept ghost { in the same string")
concept real {
  name string
}
`
	headers := ConceptHeaders(src)
	if len(headers) != 1 || headers[0].Name != "real" {
		t.Fatalf("ConceptHeaders = %+v, want exactly one header named real", headers)
	}
}

// blankCommentsAndStrings must preserve every byte offset, or the
// offsets it yields index the wrong bytes of the raw source.
func TestBlankCommentsAndStringsPreservesOffsets(t *testing.T) {
	sources := []string{
		"concept a { x string }\n",
		"// line\nconcept a {}\n",
		"/* block */ concept a {}\n",
		"@description(\"quoted \\\" text\")\nconcept a {}\n",
		"@description(\"unterminated\nconcept a {}\n",
		"/* unterminated block\nconcept a {}\n",
	}
	for _, src := range sources {
		got := blankCommentsAndStrings(src)
		if len(got) != len(src) {
			t.Fatalf("blankCommentsAndStrings(%q): length %d, want %d", src, len(got), len(src))
		}
		if strings.Count(got, "\n") != strings.Count(src, "\n") {
			t.Fatalf("blankCommentsAndStrings(%q): newline count changed", src)
		}
	}
}

// ---- codemod: the rewrite ----

func TestRewriteRowAuthzInsertsAboveThePreamble(t *testing.T) {
	src := []byte(`/// A plan.
@displayCard(primary="goal")
concept plan {
  requestedBy string
}
`)
	out, err := RewriteRowAuthz(src, map[string]RowAuthzDecl{
		"plan": {Tier: RowAuthzOwned, Owner: "requestedBy"},
	})
	if err != nil {
		t.Fatalf("RewriteRowAuthz: %v", err)
	}
	got := string(out)
	want := `/// A plan.
@displayCard(primary="goal")
@rowAuthz(owner="requestedBy")
concept plan {
  requestedBy string
}
`
	if got != want {
		t.Fatalf("RewriteRowAuthz =\n%s\nwant\n%s", got, want)
	}
}

// Re-running the codemod must be a no-op, and a hand-written
// declaration must never be replaced by an inferred one.
func TestRewriteRowAuthzIsIdempotent(t *testing.T) {
	src := []byte(`@rowAuthz(clusterOwner)
concept node {
  ownerUserId string
}
`)
	out, err := RewriteRowAuthz(src, map[string]RowAuthzDecl{
		"node": {Tier: RowAuthzOwned, Owner: "ownerUserId"},
	})
	if err != nil {
		t.Fatalf("RewriteRowAuthz: %v", err)
	}
	if string(out) != string(src) {
		t.Fatalf("RewriteRowAuthz rewrote an already-declared concept:\n%s", out)
	}

	// And a second pass over freshly-rewritten output changes nothing.
	fresh := []byte("concept node {\n  ownerUserId string\n}\n")
	tiers := map[string]RowAuthzDecl{"node": {Tier: RowAuthzOwned, Owner: "ownerUserId"}}
	first, err := RewriteRowAuthz(fresh, tiers)
	if err != nil {
		t.Fatalf("RewriteRowAuthz first pass: %v", err)
	}
	second, err := RewriteRowAuthz(first, tiers)
	if err != nil {
		t.Fatalf("RewriteRowAuthz second pass: %v", err)
	}
	if string(second) != string(first) {
		t.Fatalf("second pass differs from first:\n%s\n---\n%s", first, second)
	}
}

// A declaration sharing a line with another annotation is still a
// declaration. A line-anchored idempotency check does not see it, and
// the codemod then inserts a SECOND one -- silently replacing a
// hand-authored tier with an inferred one.
func TestRewriteRowAuthzSeesASameLineDeclaration(t *testing.T) {
	src := []byte(`@description("d") @rowAuthz(public)
concept node {
  ownerUserId string
}
`)
	out, err := RewriteRowAuthz(src, map[string]RowAuthzDecl{
		"node": {Tier: RowAuthzOwned, Owner: "ownerUserId"},
	})
	if err != nil {
		t.Fatalf("RewriteRowAuthz: %v", err)
	}
	if string(out) != string(src) {
		t.Fatalf("a same-line declaration was not detected, so a second one was inserted:\n%s", out)
	}
}

// A doc comment or a string MENTIONING the annotation is prose, not a
// declaration -- dropping the line anchor must not cost that.
func TestRowAuthzDeclaredInSourceIgnoresProse(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"doc comment", "/// remember to add @rowAuthz(public) here\nconcept a {}\n", false},
		{"line comment", "// @rowAuthz(public)\nconcept a {}\n", false},
		{"block comment", "/*\n@rowAuthz(public)\n*/\nconcept a {}\n", false},
		{"description string", "@description(\"use @rowAuthz(public)\")\nconcept a {}\n", false},
		{"real declaration", "@rowAuthz(public)\nconcept a {}\n", true},
		{"real, sharing a line", "@description(\"d\") @rowAuthz(public)\nconcept a {}\n", true},
		{"real, indented", "  @rowAuthz(public)\n  concept a {}\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RowAuthzDeclaredInSource(tc.src); got != tc.want {
				t.Fatalf("RowAuthzDeclaredInSource(%q) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

// A concept the inference had no evidence for stays undeclared. It
// must not be guessed at, because an undeclared concept is exactly the
// signal Phase 2 needs.
func TestRewriteRowAuthzLeavesUnlistedConceptsAlone(t *testing.T) {
	src := []byte(`concept known {
  ownerUserId string
}

concept unknown {
  x string
}
`)
	out, err := RewriteRowAuthz(src, map[string]RowAuthzDecl{
		"known": {Tier: RowAuthzPublic},
	})
	if err != nil {
		t.Fatalf("RewriteRowAuthz: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "@rowAuthz(public)\nconcept known") {
		t.Fatalf("known was not declared:\n%s", got)
	}
	if strings.Contains(got, "@rowAuthz(public)\nconcept unknown") {
		t.Fatalf("unknown was declared despite having no inferred tier:\n%s", got)
	}
}

// Every rewritten file must still parse, and the declaration it gained
// must read back as the tier the codemod intended.
func TestRewriteRowAuthzOutputLoadsBack(t *testing.T) {
	src := []byte(`concept plan {
  requestedBy string
}
`)
	want := RowAuthzDecl{Tier: RowAuthzOwned, Owner: "requestedBy"}
	out, err := RewriteRowAuthz(src, map[string]RowAuthzDecl{"plan": want})
	if err != nil {
		t.Fatalf("RewriteRowAuthz: %v", err)
	}
	var found *Attribute
	for _, a := range parseConceptAttrs(t, string(out)) {
		if a.Name == RowAuthzAnnotation {
			found = a
		}
	}
	if found == nil {
		t.Fatalf("rewritten source carries no @%s:\n%s", RowAuthzAnnotation, out)
	}
	got, err := ParseRowAuthz(found)
	if err != nil {
		t.Fatalf("ParseRowAuthz on rewritten source: %v", err)
	}
	if *got != want {
		t.Fatalf("rewritten source declares %+v, want %+v", *got, want)
	}
}
