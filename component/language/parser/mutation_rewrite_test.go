package parser

import (
	"strings"
	"testing"
)

// The struct-form mutation rewriter requires the write block opener
// to name its target concept: `insert <bareName> { ... }` or
// `update <bareName> { ... }`. The bare name must match the file's
// `@useConcept(<bareName>)` binding (or the trailing segment of a
// legacy `use <ns>.<concept>` directive). The bare `insert { ... }`
// shape that earlier phases of the DSL accepted is now an error
// with a migration hint.

func TestNormaliseMutationSource_RequiresInsertTargetName(t *testing.T) {
	src := `@useConcept(space)
mutation createSpace {
  insert {
    id: "x"
    name: "untitled"
  }
}`
	_, err := NormaliseMutationSource(src)
	if err == nil {
		t.Fatal("expected error: bare `insert {` should be rejected")
	}
	if !strings.Contains(err.Error(), "retired") || !strings.Contains(err.Error(), "<conceptName>") {
		t.Fatalf("error should explain the migration; got %q", err.Error())
	}
}

func TestNormaliseMutationSource_RejectsMismatchedInsertTarget(t *testing.T) {
	src := `@useConcept(space)
mutation createSpace {
  insert participant {
    id: "x"
    name: "untitled"
  }
}`
	_, err := NormaliseMutationSource(src)
	if err == nil {
		t.Fatal("expected error: insert target must match the concept binding")
	}
	if !strings.Contains(err.Error(), "participant") || !strings.Contains(err.Error(), "space") {
		t.Fatalf("error should name both the offered target and the expected one; got %q", err.Error())
	}
}

func TestNormaliseMutationSource_AcceptsMatchingInsertTarget(t *testing.T) {
	src := `@useConcept(space)
mutation createSpace {
  insert space {
    id: "x"
    name: "untitled"
  }
}`
	out, err := NormaliseMutationSource(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "func (Mutation) createSpace") {
		t.Fatalf("rewriter should produce the legacy procedural form; got %q", out)
	}
}

func TestNormaliseMutationSource_RejectsMismatchedUpdateTarget(t *testing.T) {
	src := `@useConcept(space)
mutation renameSpace {
  update participant {
    id: "x"
    name: "new"
  }
}`
	_, err := NormaliseMutationSource(src)
	if err == nil {
		t.Fatal("expected error: update target must match the concept binding")
	}
	if !strings.Contains(err.Error(), "participant") {
		t.Fatalf("error should name the offered target; got %q", err.Error())
	}
}
