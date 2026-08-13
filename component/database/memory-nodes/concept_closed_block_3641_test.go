package memoryNodes

// concept_closed_block_3641_test.go covers @closed, the per-block opt-in that
// makes a nested block reject undeclared keys (memql#3641).
//
// The exposure it closes, in the words of the issue that found it: a typo'd
// write to v1:identity:user.preferences lands beside the real field, and the
// computer-use kill switch keeps its old value. Nothing errors at either end,
// because the emitter closes the TOP level only -- so `computerUseEnbaled`
// stores happily next to `computerUseEnabled`.
//
// The flip is per-block rather than tree-wide because the blocks differ in who
// writes them. `preferences` has writers that are all in-repo. `capabilities`
// is read out of the database and written back wholesale by two agent paths, so
// closing it is a data migration. `utterance.source` takes an arbitrary
// caller-supplied map from insertSystemActionUtterance. Those two, and the
// caller-splat mutations that reach other blocks, are recorded at the emitter.

import (
	"strings"
	"testing"
)

// closedBlockConcept builds a concept with one closed block and one open one,
// so every assertion below has its own control in the same schema.
func closedBlockConcept(t *testing.T) *Concept {
	t.Helper()
	src := `
concept prefsHolder {
  topLevel  string

  closedBlock  object  @closed {
    known      string
    alsoKnown  bool
  }

  openBlock {
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

// TestClosedBlock_RefusesUndeclaredKey is the memql#3641 lock: the key the
// issue named -- a near-miss spelling of a kill switch -- must be REFUSED.
func TestClosedBlock_RefusesUndeclaredKey(t *testing.T) {
	c := closedBlockConcept(t)

	if err := c.validate("definition", map[string]any{
		"closedBlock": map[string]any{"known": "x"},
	}); err != nil {
		t.Fatalf("a declared key inside a @closed block was rejected: %v", err)
	}

	err := c.validate("definition", map[string]any{
		"closedBlock": map[string]any{"known": "x", "knwon": "typo"},
	})
	if err == nil {
		t.Fatal("an undeclared key inside a @closed block was ACCEPTED -- the typo lands beside the " +
			"real field and the value the author meant to set keeps its old value, which is the whole " +
			"exposure memql#3623 named")
	}
}

// TestClosedBlock_LeavesOtherBlocksOpen pins the scope of the flip. A block
// without @closed keeps taking undeclared keys, which is what lets
// `capabilities` and `utterance.source` wait for their migrations instead of
// blocking this one.
func TestClosedBlock_LeavesOtherBlocksOpen(t *testing.T) {
	c := closedBlockConcept(t)

	if err := c.validate("definition", map[string]any{
		"openBlock": map[string]any{"known": "x", "undeclared": "still fine"},
	}); err != nil {
		t.Fatalf("an undeclared key inside a block with NO @closed was rejected: %v.\n"+
			"@closed is per-block on purpose -- a tree-wide flip needs a data migration for "+
			"agent.capabilities and a decision about insertSystemActionUtterance's arbitrary map.", err)
	}
}

// TestClosedBlock_StillEnforcesTheTopLevel guards against the flip being read
// as a replacement for the closure that already existed.
func TestClosedBlock_StillEnforcesTheTopLevel(t *testing.T) {
	c := closedBlockConcept(t)
	if err := c.validate("definition", map[string]any{"typoAtTop": "x"}); err == nil {
		t.Fatal("an undeclared TOP-LEVEL key was accepted")
	}
}

// TestClosedOnAFreeFormObjectIsRefused: @closed on an object that declares no
// sub-fields would reject every key, not only undeclared ones. Refused at load,
// because there is no reading of it that helps -- a free-form `object` field is
// free-form on purpose.
func TestClosedOnAFreeFormObjectIsRefused(t *testing.T) {
	_, err := ParseConceptMemQL([]byte(`
concept freeForm {
  payload  object  @closed
}
`), "v1/test/freeForm")
	if err == nil {
		t.Fatal("@closed on a block-less object was accepted; it would emit additionalProperties:false " +
			"over an empty property set and refuse every row")
	}
	if !strings.Contains(err.Error(), "no block to close") {
		t.Errorf("the refusal must say what is wrong with the declaration, got: %v", err)
	}
}

// TestClosedBesideVariantIsRefused: a variant's fields live in its own oneOf
// branch, not in the parent's `properties`, so closing the parent rejects every
// branch-only key. v1:identity:identity's credentials block is exactly that
// shape -- `keyHash` lives in an arm -- so this refusal is what stops @closed
// from being applied there and refusing every credential row in the cluster.
func TestClosedBesideVariantIsRefused(t *testing.T) {
	_, err := ParseConceptMemQL([]byte(`
concept unionHolder {
  kind  string
  credentials  object  @closed  @variant(discriminator="kind") {
    oauth    { provider  string }
    api_key  { keyHash   string }
  }
}
`), "v1/test/unionHolder")
	if err == nil {
		t.Fatal("@closed beside @variant was accepted; the parent's additionalProperties does not " +
			"know about the arms' fields, so every row of the union would be refused")
	}
	if !strings.Contains(err.Error(), "@variant") {
		t.Errorf("the refusal must name the combination, got: %v", err)
	}
}

// TestClosedOnAScalarIsRefused: @closed rides the same rule as @variant and a
// nested block -- it needs an object. It is in the same family for the same
// reason, so it is refused by the same check rather than by a second one.
func TestClosedOnAScalarIsRefused(t *testing.T) {
	_, err := ParseConceptMemQL([]byte(`
concept scalarHolder {
  name  string  @closed
}
`), "v1/test/scalarHolder")
	if err == nil {
		t.Fatal("@closed on a string was accepted; additionalProperties means nothing on a scalar, " +
			"so the annotation would be stored and ignored")
	}
	if !strings.Contains(err.Error(), "@closed") {
		t.Errorf("the refusal must name the annotation, got: %v", err)
	}
}
