package memql

import (
	"strings"
	"testing"
)

// TestParseQueryMemQL_GoldenPath locks the canonical struct-form
// query syntax post-PR-C: concept binding lives in the signature
// (`query Concept name`), not in a `@useConcept(...)` annotation.
func TestParseQueryMemQL_GoldenPath(t *testing.T) {
	src := `@enabled
@description("Active participants in a space.")
query participant queryActiveParticipantsForSpace {
  args {
    spaceId  string  @required
  }
  filter payload.spaceId == args.spaceId
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
	src := `@bogusAnnotation("x")
query participant queryFoo {
  filter id == "x"
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

// TestParseQueryMemQL_RejectsUseAnnotations confirms the lockdown:
// every `@use*` annotation is rejected at parse time with a message
// pointing at the new file-top `use <module>.{ ... }` form.
func TestParseQueryMemQL_RejectsUseAnnotations(t *testing.T) {
	for _, attr := range []string{"useConcept", "useShape", "useQuery", "useMutation", "useLogic", "useBuiltin"} {
		t.Run(attr, func(t *testing.T) {
			src := `@` + attr + `(x)
query participant queryFoo {
  filter id == "x"
  shape  fooFull
}`
			_, err := parseQueryMemQL("queryFoo", "test.memql", src, nil)
			if err == nil {
				t.Fatalf("expected @%s to be rejected", attr)
			}
			if !strings.Contains(err.Error(), "retired") {
				t.Errorf("error should mention 'retired', got %v", err)
			}
		})
	}
}

// TestParseQueryMemQL_AcceptsAllAllowedAnnotations sweeps the
// full allow-list (post-lockdown) to lock the surface against
// accidental removal.
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
	}
	for _, ann := range cases {
		t.Run(ann, func(t *testing.T) {
			src := ann + `
query foo queryFoo {
  filter foo.id == "x"
  shape  fooFull
}`
			_, err := parseQueryMemQL("queryFoo", "test.memql", src, nil)
			if err != nil && strings.Contains(err.Error(), "unknown annotation") {
				t.Errorf("allow-listed annotation %q reported as unknown: %v", ann, err)
			}
		})
	}
}
