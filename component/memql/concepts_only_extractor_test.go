package memql

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
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
