package parser

import (
	"strings"
	"testing"
)

// The canonical struct-form mutation write block is bare `insert { ... }`
// / `update { ... }` (#986): the target concept comes from the mutation's
// `mutation <Concept> <name>` signature, so restating it in the body is
// redundant. The named form `insert <conceptName> { ... }` is still
// accepted transitionally and its concept name is validated against the
// signature; its tree-wide migration + hard rejection is the codemod #988.
//
// memql#314 retired the legacy concept-binding-via-annotation form;
// every fixture below declares the binding through the signature.

func TestNormaliseMutationSource_AcceptsBareInsert(t *testing.T) {
	src := `mutation space createSpace {
  insert {
    id: "x"
    name: "untitled"
  }
}`
	out, err := NormaliseMutationSource(src)
	if err != nil {
		t.Fatalf("bare `insert {` should be accepted, got: %v", err)
	}
	if !strings.Contains(out, "func (Mutation) createSpace") {
		t.Fatalf("rewriter should produce the procedural form; got %q", out)
	}
	// The write target is derived from the signature-bound concept.
	if !strings.Contains(out, "insert(") || !strings.Contains(out, "space") {
		t.Fatalf("emitted insert should target the signature concept `space`; got %q", out)
	}
}

func TestNormaliseMutationSource_AcceptsBareUpdate(t *testing.T) {
	src := `mutation space renameSpace {
  update {
    id: args.id
    name: "new"
  }
}`
	out, err := NormaliseMutationSource(src)
	if err != nil {
		t.Fatalf("bare `update {` should be accepted, got: %v", err)
	}
	if !strings.Contains(out, "update(") {
		t.Fatalf("emitted body should contain an update call; got %q", out)
	}
}

func TestNormaliseMutationSource_RejectsMismatchedInsertTarget(t *testing.T) {
	src := `mutation space createSpace {
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
	src := `mutation space createSpace {
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
	src := `mutation space renameSpace {
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
