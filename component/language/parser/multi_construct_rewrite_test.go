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
// rewriter has to bind each construct to its OWN concept (via the
// nearest @useConcept(...) annotation walking backward from the
// block header) rather than to a single file-level binding.
//
// extractConceptBindingForBlock implements that walk-back. These
// tests prove the rule holds for representative multi-construct
// shapes.
//
// Note: the rewriter emits each construct's procedural form WITHOUT
// translating bareName.X -> payload.X. That translation happens
// downstream in component/memql/function_loader.go via
// extractAllUseConceptNames + translateConceptPathsToPayload (the
// multi-name iteration is the Phase 1 fix that lets multi-construct
// files load cleanly). These tests stay focused on the rewriter's
// own job: emitting the right per-construct concept binding.

// TestRewriter_MultipleQueriesDifferentConcepts locks the case the
// import-model refactor most depends on: two queries in one file,
// each bound to a different concept via per-construct @useConcept.
func TestRewriter_MultipleQueriesDifferentConcepts(t *testing.T) {
	source := `@useConcept(participant)
@description("Active participants in a space.")
query queryActiveParticipants {
  filter participant.spaceId == args.spaceId
  shape  participantFull
}

@useConcept(space)
@description("Active spaces for a user.")
query queryActiveSpaces {
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
	source := `@useConcept(space)
@description("Create a cognition space.")
mutation mutationCreateSpace {
  args { name string @required }
  insert space {
    id:   args.id
    name: args.name
  }
}

@useConcept(participant)
@description("Add a participant to a space.")
mutation mutationAddParticipant {
  args { spaceId string @required; userId string @required }
  insert participant {
    spaceId: args.spaceId
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

// TestRewriter_LeftoverFileTopUseStillWorks locks the legacy
// single-construct path: a file with a file-top `use <ns>.<concept>`
// directive and no per-construct annotation still rewrites cleanly.
// (Backward compatibility during the import-model migration window.)
func TestRewriter_LeftoverFileTopUseStillWorks(t *testing.T) {
	source := `use cognition.participant

@description("Single-construct legacy file.")
query queryParticipantById {
  args { id string @required }
  filter id == args.id
  shape  participantFull
}`

	out, err := NormaliseQuerySource(source)
	if err != nil {
		t.Fatalf("NormaliseQuerySource: %v", err)
	}
	if !strings.Contains(out, "func (Query) queryParticipantById(") {
		t.Errorf("expected procedural form, got:\n%s", out)
	}
	// The file-top `use` produces a fully-qualified concept id.
	if !strings.Contains(out, "concept==v1:cognition:participant") {
		t.Errorf("expected fully-qualified concept binding from file-top `use`, got:\n%s", out)
	}
}
