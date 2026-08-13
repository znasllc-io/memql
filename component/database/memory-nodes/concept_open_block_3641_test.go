package memoryNodes

// concept_open_block_3641_test.go covers the nested-block closure memql#3641
// landed, and @open, the per-block escape from it.
//
// A nested block used to ACCEPT undeclared keys while the top level refused
// them -- an asymmetry with no rule behind it. What it cost, in the words of
// the issue that found it: a typo'd write to v1:identity:user.preferences lands
// beside the real field, and the computer-use kill switch keeps its old value.
// Nothing errored at either end, because the emitter closed the top level only,
// so `computerUseEnbaled` stored happily next to `computerUseEnabled`.
//
// Closed is now the DEFAULT, everywhere -- the embedded tree and a product
// bundle mounted at MEMQL_DSL_PATH alike. @open marks a block as free-form BY
// DESIGN, for keys that are data rather than schema. A block that merely has an
// undeclared key today wants the key DECLARED instead, which is what the sweep
// behind this flip did nearly everywhere it looked.

import (
	"strings"
	"testing"
)

// blockClosureConcept builds a concept with one default (closed) block and one
// @open block, so every assertion below has its own control in the same schema.
func blockClosureConcept(t *testing.T) *Concept {
	t.Helper()
	src := `
concept prefsHolder {
  topLevel  string

  closedBlock {
    known      string
    alsoKnown  bool
  }

  openBlock  object  @open {
    known  string
  }
}
`
	c, err := ParseConceptMemQL([]byte(src), "v1/test/prefsHolder")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}

// TestNestedBlockRefusesUndeclaredKeyByDefault is the memql#3641 lock: the key
// the issue named -- a near-miss spelling of a kill switch -- must be REFUSED,
// with no annotation asked for.
func TestNestedBlockRefusesUndeclaredKeyByDefault(t *testing.T) {
	c := blockClosureConcept(t)

	if err := c.validate("definition", map[string]any{
		"closedBlock": map[string]any{"known": "x"},
	}); err != nil {
		t.Fatalf("a declared key inside a nested block was rejected: %v", err)
	}

	err := c.validate("definition", map[string]any{
		"closedBlock": map[string]any{"known": "x", "knwon": "typo"},
	})
	if err == nil {
		t.Fatal("an undeclared key inside a nested block was ACCEPTED -- the typo lands beside the " +
			"real field and the value the author meant to set keeps its old value, which is the whole " +
			"exposure memql#3623 named")
	}
}

// TestOpenBlockStillAcceptsUndeclaredKeys pins the escape. @open is what a
// block declares when its keys are DATA rather than schema, and without a
// working escape the flip would force every such block to either lie about its
// shape or drop to a bare `object` and lose the sub-fields it does know.
func TestOpenBlockStillAcceptsUndeclaredKeys(t *testing.T) {
	c := blockClosureConcept(t)

	if err := c.validate("definition", map[string]any{
		"openBlock": map[string]any{"known": "x", "undeclared": "by design"},
	}); err != nil {
		t.Fatalf("an undeclared key inside an @open block was rejected, so the escape does not "+
			"work and every free-form block has to drop to a bare `object`: %v", err)
	}
}

// TestTopLevelStaysClosed guards against the flip being read as a replacement
// for the closure that already existed.
func TestTopLevelStaysClosed(t *testing.T) {
	c := blockClosureConcept(t)
	if err := c.validate("definition", map[string]any{"typoAtTop": "x"}); err == nil {
		t.Fatal("an undeclared TOP-LEVEL key was accepted")
	}
}

// TestOpenOnAFreeFormObjectIsRefused: @open on an object that declares no
// sub-fields relaxes a schema that was never there -- a bare `object` already
// accepts any shape. Refused at load, because the annotation claims a decision
// nobody made.
func TestOpenOnAFreeFormObjectIsRefused(t *testing.T) {
	_, err := ParseConceptMemQL([]byte(`
concept freeForm {
  payload  object  @open
}
`), "v1/test/freeForm")
	if err == nil {
		t.Fatal("@open on a block-less object was accepted; there is no closure to escape, so the " +
			"annotation is stored and means nothing")
	}
	if !strings.Contains(err.Error(), "no block to relax") {
		t.Errorf("the refusal must say what is wrong with the declaration, got: %v", err)
	}
}

// TestOpenBesideVariantIsRefused: "accept keys no branch declares" is not a
// relaxation of a discriminated union, it is the union giving up. The emitter
// cannot express both, so the contradiction is refused at load rather than
// resolved silently in one direction.
func TestOpenBesideVariantIsRefused(t *testing.T) {
	_, err := ParseConceptMemQL([]byte(`
concept unionHolder {
  kind  string
  credentials  object  @open  @variant(discriminator="kind") {
    oauth    { provider  string }
    api_key  { keyHash   string }
  }
}
`), "v1/test/unionHolder")
	if err == nil {
		t.Fatal("@open beside @variant was accepted; the two say opposite things about the same " +
			"object and the emitter cannot honour both")
	}
	if !strings.Contains(err.Error(), "@variant") {
		t.Errorf("the refusal must name the combination, got: %v", err)
	}
}

// TestOpenOnAScalarIsRefused: @open rides the same rule as @variant and a
// nested block -- it needs an object. Same family, same check, rather than a
// second one that could drift from it.
func TestOpenOnAScalarIsRefused(t *testing.T) {
	_, err := ParseConceptMemQL([]byte(`
concept scalarHolder {
  name  string  @open
}
`), "v1/test/scalarHolder")
	if err == nil {
		t.Fatal("@open on a string was accepted; additionalProperties means nothing on a scalar, " +
			"so the annotation would be stored and ignored")
	}
	if !strings.Contains(err.Error(), "@open") {
		t.Errorf("the refusal must name the annotation, got: %v", err)
	}
}
