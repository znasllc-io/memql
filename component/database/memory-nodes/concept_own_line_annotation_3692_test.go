package memoryNodes

// concept_own_line_annotation_3692_test.go covers the refusal memql#3692 adds:
// a property annotation must sit on its property's own declaration line.
//
// The defect it replaces was silent and failed OPEN. The lexer strips newlines,
// so
//
//	a  string
//	@pii
//	b  string
//
// is byte-for-byte the token stream of `a string @pii` followed by `b string`.
// The trailing-annotation loop therefore marked `a` -- the field ABOVE the one
// the author wrote it over. With @pii / @secret / @internal that leaves the
// field the author marked exposed and marks a different one.
//
// The rule, stated once: an own-line annotation binds FORWARD, and is REFUSED
// where the property above could claim the same token.
//
// Both halves are load-bearing. Refusing everywhere would break the reading
// memql#3623 deliberately pinned after a nested block, where forward is the
// only meaning available. Binding forward everywhere would silently move a
// wrapped continuation
//
//	someField  string
//	  @description("…")
//
// onto the following field -- the same misattribution pointing the other way.
// Refusing exactly the ambiguous position is the only reading that is never
// quietly wrong, and it is the shape memql#3623 chose for the mirror spelling.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestOwnLinePropertyAnnotationIsRefused is the memql#3692 lock, written as the
// exact reproduction from the issue.
func TestOwnLinePropertyAnnotationIsRefused(t *testing.T) {
	_, err := ParseConceptMemQL([]byte(`
concept prefixProbe {
  a  string
  @pii
  b  string
}
`), "v1/test/prefixProbe")
	if err == nil {
		t.Fatal("an own-line @pii was accepted. It binds to the property ABOVE it, so the field the " +
			"author marked stays exposed and a different one is marked -- silently, in the direction " +
			"that fails open (memql#3692)")
	}
	if !strings.Contains(err.Error(), "own line") {
		t.Errorf("the refusal must name the ambiguity, got: %v", err)
	}
}

// TestOwnLineAnnotationAfterABlockStillBindsForward is the OTHER position the
// same spelling reaches, and it is deliberately NOT refused.
//
// After a nested block there is only one reading available: a block-bodied
// property cannot take a trailing annotation at all -- parsePropertyDecl
// returns at the closing brace, and memql#3623 refuses the one spelling
// (same-line-as-the-brace) that could reach it. So the annotation cannot belong
// to the property above, and prefix-of-the-next-property is the only thing left
// for it to mean.
//
// That is the whole rule, stated once: an own-line annotation binds FORWARD,
// and is refused only where the property above could claim the same token. The
// refusal is not "own-line annotations are bad", it is "an ambiguous one is".
// memql#3623 pinned this forward-binding reading on purpose
// (TestNestedBlock_PrefixAnnotationOnALaterLineStillBindsForward); this test
// says so from the memql#3692 side so a later change cannot break one while
// reading only the other.
func TestOwnLineAnnotationAfterABlockStillBindsForward(t *testing.T) {
	c, err := ParseConceptMemQL([]byte(`
concept afterBlock {
  outer {
    inner  string
  }
  @pii
  b  string
}
`), "v1/test/afterBlock")
	if err != nil {
		t.Fatalf("an own-line annotation after a nested block is unambiguous and must keep "+
			"parsing: %v", err)
	}
	raw, err := c.DefinitionSchema()
	if err != nil {
		t.Fatalf("DefinitionSchema: %v", err)
	}
	var doc struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Properties["b"]["x-pii"] != true {
		t.Errorf("@pii after a block must bind FORWARD to `b`: %v", doc.Properties["b"])
	}
	if _, marked := doc.Properties["outer"]["x-pii"]; marked {
		t.Errorf("@pii was consumed by the preceding block: %v", doc.Properties["outer"])
	}
}

// TestPropertyAnnotationOnItsOwnLineIsFineWhenTrailing pins what stays legal:
// the annotation at the end of the property's declaration, which is how the
// entire corpus is written.
func TestPropertyAnnotationOnItsOwnLineIsFineWhenTrailing(t *testing.T) {
	c, err := ParseConceptMemQL([]byte(`
concept trailingOk {
  a  string  @pii  @description("marked, and on its own line")
  b  string
}
`), "v1/test/trailingOk")
	if err != nil {
		t.Fatalf("the canonical trailing form must keep parsing: %v", err)
	}
	if c == nil {
		t.Fatal("no concept returned")
	}
}

// TestRelationshipStaysBodyLevel is the exemption that has to survive: every
// concept in the tree writes @relationship on its own line, and it is body-
// level wherever it appears -- so its position carries no ambiguity to resolve
// and refusing it would refuse 141 live declarations.
func TestRelationshipStaysBodyLevel(t *testing.T) {
	c, err := ParseConceptMemQL([]byte(`
concept withRel {
  ownerUserId  string
  title        string

  @relationship(type="parent", field="ownerUserId", target="v1:test:owner", direction="outgoing")
}
`), "v1/test/withRel")
	if err != nil {
		t.Fatalf("@relationship on its own line must keep parsing -- every concept in the tree writes "+
			"it that way: %v", err)
	}
	if len(c.Relationships) != 1 {
		t.Fatalf("expected the relationship to be hoisted to the concept, got %d", len(c.Relationships))
	}
}
