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

// The named write form `insert <concept> { ... }` is retired (#988) -- even
// when the restated concept matches the signature. The target always comes
// from the `mutation <Concept> <name>` signature now.
func TestNormaliseMutationSource_RejectsNamedInsert(t *testing.T) {
	for _, target := range []string{"participant", "space"} {
		src := "mutation space createSpace {\n  insert " + target + " {\n    id: \"x\"\n    name: \"untitled\"\n  }\n}"
		_, err := NormaliseMutationSource(src)
		if err == nil {
			t.Fatalf("expected the named form `insert %s {` to be rejected", target)
		}
		if !strings.Contains(err.Error(), "retired") || !strings.Contains(err.Error(), target) {
			t.Fatalf("error should name the retired form + the restated concept; got %q", err.Error())
		}
	}
}

func TestNormaliseMutationSource_RejectsNamedUpdate(t *testing.T) {
	src := `mutation space renameSpace {
  update space {
    id: "x"
    name: "new"
  }
}`
	_, err := NormaliseMutationSource(src)
	if err == nil {
		t.Fatal("expected the named form `update space {` to be rejected")
	}
	if !strings.Contains(err.Error(), "retired") {
		t.Fatalf("error should explain the migration; got %q", err.Error())
	}
}
