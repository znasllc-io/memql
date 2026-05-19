package memql

import (
	"strings"
	"testing"
)

// TestParseMutationMemQL_GoldenPath locks the canonical struct-form
// mutation syntax with an `insert` block.
func TestParseMutationMemQL_GoldenPath(t *testing.T) {
	src := `@enabled
@useConcept(space)
@description("Create a cognition space.")
mutation mutationCreateSpace {
  args {
    spaceId  string  @required
    name     string  @required
  }
  insert space {
    id:        args.spaceId
    name:      args.name
    status:    "active"
    createdAt: now
    createdBy: actor.userId
  }
}`

	fn, err := parseMutationMemQL("mutationCreateSpace", "test.memql", src, nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if fn == nil {
		t.Fatal("expected non-nil *Function")
	}
	if fn.Name != "mutationCreateSpace" {
		t.Errorf("Name = %q, want mutationCreateSpace", fn.Name)
	}
}

// TestParseMutationMemQL_UpdateForm exercises the partial-update
// counterpart: `update <concept> { id: ..., fieldA: ... }`.
func TestParseMutationMemQL_UpdateForm(t *testing.T) {
	src := `@useConcept(user)
@description("Stamp dataExportLastAt on a user row.")
mutation mutationBumpUserExport {
  args {
    userId  string  @required
  }
  update user {
    id:               args.userId
    dataExportLastAt: timestamp()
  }
}`

	fn, err := parseMutationMemQL("mutationBumpUserExport", "test.memql", src, nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if fn == nil {
		t.Fatal("expected non-nil *Function")
	}
}

// TestParseMutationMemQL_RejectsUnknownAnnotation locks the
// per-construct annotation allow-list.
func TestParseMutationMemQL_RejectsUnknownAnnotation(t *testing.T) {
	src := `@useConcept(space)
@bogusKnob
mutation mutationFoo {
  args { id string @required }
  insert space {
    id: args.id
  }
}`

	_, err := parseMutationMemQL("mutationFoo", "test.memql", src, nil)
	if err == nil {
		t.Fatal("expected unknown-annotation error, got nil")
	}
	if !strings.Contains(err.Error(), "bogusKnob") {
		t.Errorf("error should mention bogusKnob, got %v", err)
	}
}

// TestParseMutationMemQL_AcceptsMutationOnlyAnnotations sweeps the
// mutation-specific annotations not present on queries.
func TestParseMutationMemQL_AcceptsMutationOnlyAnnotations(t *testing.T) {
	mutationOnly := []string{
		`@idempotent`,
		`@destructive`,
		`@requiresConfirmation`,
		`@actor("system")`,
		`@useMutation(other)`,
	}
	for _, ann := range mutationOnly {
		t.Run(ann, func(t *testing.T) {
			src := ann + `
@useConcept(space)
mutation mutationFoo {
  args { id string @required }
  insert space {
    id: args.id
  }
}`
			_, err := parseMutationMemQL("mutationFoo", "test.memql", src, nil)
			if err != nil && strings.Contains(err.Error(), "unknown annotation") {
				t.Errorf("allow-listed mutation annotation %q reported as unknown: %v", ann, err)
			}
		})
	}
}
