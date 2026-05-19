package memql

import (
	"strings"
	"testing"
)

// TestParseQueryMemQL_GoldenPath locks the canonical struct-form
// query syntax. A query that declares args, filter, shape, and the
// concept binding via @useConcept must parse without error and
// populate the resulting *Function.
func TestParseQueryMemQL_GoldenPath(t *testing.T) {
	src := `@enabled
@useConcept(participant)
@description("Active participants in a space.")
query queryActiveParticipantsForSpace {
  args {
    spaceId  string  @required
  }
  filter participant.spaceId == args.spaceId
  shape  participantFull
}`

	fn, err := parseQueryMemQL("queryActiveParticipantsForSpace", "test.memql", src, nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if fn == nil {
		t.Fatal("expected non-nil *Function")
	}
	if fn.Name != "queryActiveParticipantsForSpace" {
		t.Errorf("Name = %q, want queryActiveParticipantsForSpace", fn.Name)
	}
}

// TestParseQueryMemQL_RejectsUnknownAnnotation verifies the per-construct
// annotation allow-list catches typos -- the value the dedicated parser
// adds over the general parser.
func TestParseQueryMemQL_RejectsUnknownAnnotation(t *testing.T) {
	src := `@useConcept(participant)
@bogusAnnotation("x")
query queryFoo {
  filter participant.id == "x"
  shape  participantFull
}`

	_, err := parseQueryMemQL("queryFoo", "test.memql", src, nil)
	if err == nil {
		t.Fatal("expected unknown-annotation error, got nil")
	}
	if !strings.Contains(err.Error(), "bogusAnnotation") {
		t.Errorf("error should mention bogusAnnotation, got %v", err)
	}
}

// TestParseQueryMemQL_AcceptsAllAllowedAnnotations sweeps the
// full allow-list to lock the surface against accidental removal.
// If an annotation is later removed from allowedQueryAnnotations,
// this test points at the removed name.
func TestParseQueryMemQL_AcceptsAllAllowedAnnotations(t *testing.T) {
	cases := []string{
		`@description("d")`,
		`@enabled`,
		`@disabled`,
		`@deprecated("hint")`,
		`@internal`,
		`@cacheTTL("5m")`,
		`@timeout("30s")`,
		`@audit`,
		`@role("admin")`,
		`@permission("p")`,
		`@useConcept(foo)`,
		`@useShape(s)`,
		`@useSpec(x)`,
		`@useTrait(t)`,
		`@useQuery(q)`,
		`@useLogic(l)`,
		`@useBuiltin(b)`,
	}
	for _, ann := range cases {
		t.Run(ann, func(t *testing.T) {
			src := ann + `
@useConcept(foo)
query queryFoo {
  filter foo.id == "x"
  shape  fooFull
}`
			_, err := parseQueryMemQL("queryFoo", "test.memql", src, nil)
			// We only care that the annotation-allow-list pass does
			// not reject the annotation. Downstream errors (missing
			// concept registry entries, etc.) are acceptable -- those
			// come from a different layer.
			if err != nil && strings.Contains(err.Error(), "unknown annotation") {
				t.Errorf("allow-listed annotation %q reported as unknown: %v", ann, err)
			}
		})
	}
}
