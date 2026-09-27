package memql

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// unparsedConceptTree is a domain with one concept that parses and one that
// does not, the second placed below the first so its line is the file's.
var unparsedConceptTree = fstest.MapFS{
	"support/memql.toml": languageLineFile(),
	"support/concepts.memql": {Data: []byte(`/// A note, which parses.
concept note {
  body  string
}

/// A ticket whose second field does not parse.
concept ticket {
  title   string
  status  string  @@description("x")
}
`)},
}

// TestAConceptThatDoesNotParseIsASkip (memql#5426): the unified loader used to
// drop a concept whose declaration failed to parse without a word, so strict
// boot booted without it. It is a ConceptSkip now -- which app/database.go
// refuses boot on -- naming the concept, the line of its `concept` keyword,
// the parser's refusal positioned in the file, and concept_unparsed; the
// concept beside it still builds.
func TestAConceptThatDoesNotParseIsASkip(t *testing.T) {
	concepts, skips, err := BuildUnifiedConcepts(nil, unparsedConceptTree)
	if err != nil {
		t.Fatalf("BuildUnifiedConcepts: %v", err)
	}
	if _, ok := concepts["v1:support:note"]; !ok {
		t.Errorf("the concept that parses did not build: %d built", len(concepts))
	}
	if _, ok := concepts["v1:support:ticket"]; ok {
		t.Error("the concept that does not parse was built")
	}
	if len(skips) != 1 {
		t.Fatalf("want one skip, got %d: %v", len(skips), skips)
	}
	s := skips[0]
	var pe *ConceptParseError
	if s.File != "support/concepts.memql" || s.Concept != "ticket" || !errors.As(s.Err, &pe) {
		t.Fatalf("skip = %+v, want support/concepts.memql / ticket / a *ConceptParseError", s)
	}
	if pe.RuleCode() != CodeConceptUnparsed {
		t.Errorf("code = %q, want %q", pe.RuleCode(), CodeConceptUnparsed)
	}
	for _, want := range []string{
		`concept "ticket" DROPPED`,
		`line 7: concept "ticket" does not parse, so it is not registered`,
		"parse error at line 9",
		"[" + CodeConceptUnparsed + "]",
	} {
		if !strings.Contains(s.String(), want) {
			t.Errorf("the skip does not say %q:\n%s", want, s.String())
		}
	}
}

// TestLintReportsAConceptThatDoesNotParse: the engine-parity pass memqllint
// and the package analysis run reports it too, so an offline lint and boot
// say the same thing about the tree.
func TestLintReportsAConceptThatDoesNotParse(t *testing.T) {
	root := fstest.MapFS{}
	for p, f := range unparsedConceptTree {
		root["probeunparsed/"+strings.TrimPrefix(p, "support/")] = f
	}
	diags, _, err := LintUnifiedTree(nil, root)
	if err != nil {
		t.Fatalf("LintUnifiedTree: %v", err)
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d.Message, `concept "ticket" does not parse`) && strings.HasSuffix(d.Message, "["+CodeConceptUnparsed+"]") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the lint did not report the concept that does not parse: %v", diags)
	}
}

// TestRowAuthzAboveAMultiLineAnnotationIsDeclared (memql#5426 review) is a
// security regression test. The concept slicer's preamble walk stopped at the
// first line of a multi-line annotation's argument list, so every annotation
// above it was cut out of the slice -- and a @rowAuthz there left the concept
// on the UNDECLARED tier, which admits every reader, with nothing refused and
// nothing logged but the undeclared-tier warning every legacy concept draws.
// The tier must be DECLARED, owned by the field it names.
func TestRowAuthzAboveAMultiLineAnnotationIsDeclared(t *testing.T) {
	tree := fstest.MapFS{
		"support/memql.toml": languageLineFile(),
		"support/concepts.memql": {Data: []byte(`@rowAuthz(owner="ownerUserId")
@description(
  "A ticket a person owns."
)
concept ticket {
  ownerUserId  string  @required
  title        string
}
`)},
	}
	concepts, skips, err := BuildUnifiedConcepts(nil, tree)
	if err != nil {
		t.Fatalf("BuildUnifiedConcepts: %v", err)
	}
	if len(skips) != 0 {
		t.Fatalf("unexpected skips: %v", skips)
	}
	c, ok := concepts["v1:support:ticket"]
	if !ok {
		t.Fatalf("the concept did not build: %d built", len(concepts))
	}
	if c.RowAuthz == nil {
		t.Fatal("the concept loaded on the UNDECLARED row-authz tier: the @rowAuthz above its multi-line @description was cut out of the slice")
	}
	if c.RowAuthz.Tier != languageParser.RowAuthzOwned || c.RowAuthz.Owner != "ownerUserId" {
		t.Errorf("row authz = %+v, want the owned tier on ownerUserId", *c.RowAuthz)
	}
	if c.Description != "A ticket a person owns." {
		t.Errorf("description = %q, want the multi-line one", c.Description)
	}
}
