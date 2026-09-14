package parser

import (
	"strings"
	"testing"
)

// Multi-construct rewriter tests
// ==============================
//
// The consolidated DSL layout (Phase 2 of the import-model refactor)
// puts many constructs of different kinds into one .memql file. The
// rewriter has to bind each construct to its OWN concept; memql#314
// retired the annotation-walkback fallback so every struct-form
// construct now declares its concept binding in the signature
// (`<kind> <Concept> <name> { ... }`). These tests prove the
// per-construct binding rule holds.
//
// Note: the rewriter emits each construct's procedural form WITHOUT
// translating bareName.X -> payload.X. That translation happens
// downstream in component/memql/function_loader.go via
// translateConceptPathsToPayload.

// TestRewriter_MultipleQueriesDifferentConcepts locks the case the
// import-model refactor most depends on: two queries in one file,
// each bound to a different concept via the signature.
func TestRewriter_MultipleQueriesDifferentConcepts(t *testing.T) {
	source := `@description("Active participants in a space.")
query participant queryActiveParticipants {
  filter participant.partitionId == args.partitionId
  shape  participantFull
}

@description("Active spaces for a user.")
query space queryActiveSpaces {
  filter space.ownerId == args.userId
  shape  spaceFull
}`

	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}

	// Both queries must have rewritten without binding-conflict errors.
	for _, name := range []string{"queryActiveParticipants", "queryActiveSpaces"} {
		if !strings.Contains(out, "func (Query) "+name+"(") {
			t.Errorf("expected procedural form for %q in output, got:\n%s", name, out)
		}
	}

	// Each construct must be bound to its OWN concept via the
	// generated `concept==<bareName>` clause in the procedural body.
	// This is the per-construct binding property the import-model
	// refactor relies on.
	if !strings.Contains(out, "concept==participant") {
		t.Errorf("expected queryActiveParticipants bound to `concept==participant`, got:\n%s", out)
	}
	if !strings.Contains(out, "concept==space") {
		t.Errorf("expected queryActiveSpaces bound to `concept==space`, got:\n%s", out)
	}
}

// TestRewriter_MultipleMutationsDifferentConcepts is the mutation
// twin of the query test above.
func TestRewriter_MultipleMutationsDifferentConcepts(t *testing.T) {
	source := `@description("Create a cognition space.")
mutation space mutationCreateSpace {
  args { name string @required }
  insert {
    id:   args.id
    name: args.name
  }
}

@description("Add a participant to a space.")
mutation participant mutationAddParticipant {
  args { partitionId string @required; userId string @required }
  insert {
    partitionId: args.partitionId
    userId:  args.userId
  }
}`

	out, err := NormaliseMutationSource(source)
	if err != nil {
		t.Fatalf("NormaliseMutationSource: %v", err)
	}

	for _, name := range []string{"mutationCreateSpace", "mutationAddParticipant"} {
		if !strings.Contains(out, "func (Mutation) "+name+"(") {
			t.Errorf("expected procedural form for %q in output, got:\n%s", name, out)
		}
	}
	// Each mutation's insert call must reference its own concept.
	if !strings.Contains(out, "insert(space") {
		t.Errorf("expected mutationCreateSpace to call insert(space, ...), got:\n%s", out)
	}
	if !strings.Contains(out, "insert(participant") {
		t.Errorf("expected mutationAddParticipant to call insert(participant, ...), got:\n%s", out)
	}
}

// TestRewriter_MissingConceptBindingErrors locks the migration-error
// path: a struct-form query/mutation without a signature-bound concept
// must fail with a clear "missing concept binding" message. Previously
// the rewriter would fall back to an annotation or file-top directive;
// memql#314 retired those fallbacks.
func TestRewriter_MissingConceptBindingErrors(t *testing.T) {
	source := `@description("Bare-signature query, no concept binding.")
query queryActiveSpaces {
  filter space.ownerId == args.userId
  shape  spaceFull
}`

	_, err := NormaliseQuerySource(source)
	if err == nil {
		t.Fatal("expected error: query missing signature-bound concept")
	}
	if !strings.Contains(err.Error(), "missing concept binding") {
		t.Fatalf("error should name the missing binding; got %q", err.Error())
	}
}
