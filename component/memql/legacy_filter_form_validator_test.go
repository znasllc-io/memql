package memql

import (
	"strings"
	"testing"
)

// TestValidateNoLegacyConceptPathRefs_RejectsConceptPrefix locks in
// the load-time rejection of the legacy filter form. A file that
// binds @useConcept(<name>) and then writes <name>.X==... in any
// body context must fail with a clear, actionable error.
func TestValidateNoLegacyConceptPathRefs_RejectsConceptPrefix(t *testing.T) {
	src := `@enabled
@useConcept(agent)
query queryAgent {
  args { id string @required }
  filter agent.id == args.id
  shape  agentFull
}`
	err := validateNoLegacyConceptPathRefs(src, "fixture.memql")
	if err == nil {
		t.Fatal("expected legacy-form rejection, got nil")
	}
	if !strings.Contains(err.Error(), "agent.X reference") {
		t.Errorf("error should name the offending bareName; got: %v", err)
	}
	if !strings.Contains(err.Error(), "payload.X") {
		t.Errorf("error should point at the canonical form (payload.X); got: %v", err)
	}
}

// TestValidateNoLegacyConceptPathRefs_AcceptsCanonical confirms the
// validator passes when the body uses payload.X.
func TestValidateNoLegacyConceptPathRefs_AcceptsCanonical(t *testing.T) {
	src := `@enabled
@useConcept(agent)
query queryAgent {
  args { id string @required }
  filter payload.id == args.id
  shape  agentFull
}`
	if err := validateNoLegacyConceptPathRefs(src, "fixture.memql"); err != nil {
		t.Errorf("canonical form should pass; got: %v", err)
	}
}

// TestValidateNoLegacyConceptPathRefs_IgnoresAnnotationLine confirms
// the @useConcept(<name>) annotation itself isn't flagged (the bare
// name there is an argument, not a field access).
func TestValidateNoLegacyConceptPathRefs_IgnoresAnnotationLine(t *testing.T) {
	src := `@useConcept(participant)
query q { filter payload.id == args.id; shape pFull }`
	if err := validateNoLegacyConceptPathRefs(src, "fixture.memql"); err != nil {
		t.Errorf("annotation line should be exempt; got: %v", err)
	}
}

// TestValidateNoLegacyConceptPathRefs_IgnoresComments confirms that
// `agent.X` text inside a // comment or /* */ block does not trip
// the validator -- comments are documentation, not code.
func TestValidateNoLegacyConceptPathRefs_IgnoresComments(t *testing.T) {
	src := `@useConcept(agent)
// agent.id was the old pre-migration form; now we use payload.id.
/* See dsl/conformance_test.go for agent.X enforcement. */
query q { filter payload.id == args.id; shape agentFull }`
	if err := validateNoLegacyConceptPathRefs(src, "fixture.memql"); err != nil {
		t.Errorf("comments should be exempt; got: %v", err)
	}
}

// TestValidateNoLegacyConceptPathRefs_IgnoresStringLiterals confirms
// that `agent.X` inside a quoted string is not flagged.
func TestValidateNoLegacyConceptPathRefs_IgnoresStringLiterals(t *testing.T) {
	src := `@useConcept(agent)
query q { filter payload.id == "agent.id" ; shape agentFull }`
	if err := validateNoLegacyConceptPathRefs(src, "fixture.memql"); err != nil {
		t.Errorf("string literals should be exempt; got: %v", err)
	}
}

// TestValidateNoLegacyConceptPathRefs_ProceduralForm confirms the
// rejection extends to procedural-form bodies (shape(concept; ...)).
// The conformance test already enforces this at file-walk time; the
// validator enforces it at load time.
func TestValidateNoLegacyConceptPathRefs_ProceduralForm(t *testing.T) {
	src := `@useConcept(space)
func (Query) queryActiveSpaces(_ any) (any, error) {
  return shape(concept; space.status=="active", "spaceFull"), nil
}`
	err := validateNoLegacyConceptPathRefs(src, "fixture.memql")
	if err == nil {
		t.Fatal("expected legacy-form rejection inside procedural shape(); got nil")
	}
	if !strings.Contains(err.Error(), "space.X reference") {
		t.Errorf("error should name the offending bareName; got: %v", err)
	}
}

// TestValidateNoLegacyConceptPathRefs_NoUseAnnotation confirms files
// that don't declare any @useConcept / @useShape are skipped (there
// is no concept binding to enforce against).
func TestValidateNoLegacyConceptPathRefs_NoUseAnnotation(t *testing.T) {
	src := `func (Query) queryRaw(_ any) (any, error) {
  return shape(concept==v1:identity:user; payload.active==true, "userFull"), nil
}`
	if err := validateNoLegacyConceptPathRefs(src, "fixture.memql"); err != nil {
		t.Errorf("file without @useConcept should pass; got: %v", err)
	}
}
