package memql

import (
	"strings"
	"testing"
)

// TestParseSpecMemQL_GoldenPath_RowSpec locks the canonical
// struct-form spec syntax for a row-spec bound to a concept. The
// body references payload via the bareName (`participant.X`) which
// the parser rewrites to `payload.X` before expression parsing.
func TestParseSpecMemQL_GoldenPath_RowSpec(t *testing.T) {
	src := []byte(`@useConcept(participant)
@description("Matches participants with human participantType.")
spec specIsHumanParticipant {
  participant.participantType == "human"
}`)

	got, err := parseSpecMemQL("test.memql", src)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil *Spec")
	}
	if got.Name != "specIsHumanParticipant" {
		t.Errorf("Name = %q, want specIsHumanParticipant", got.Name)
	}
	if got.UseConceptName != "participant" {
		t.Errorf("UseConceptName = %q, want participant", got.UseConceptName)
	}
	if got.IsTrait {
		t.Error("IsTrait = true, want false (this is a spec, not a trait)")
	}
}

// TestParseSpecMemQL_TraitForbidsBinding locks the rule: traits are
// concept-agnostic. @useConcept on a trait is an error.
func TestParseSpecMemQL_TraitForbidsBinding(t *testing.T) {
	src := []byte(`@useConcept(participant)
trait traitBoom {
  payload.active == true
}`)

	_, err := parseSpecMemQL("test.memql", src)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "@useConcept") {
		t.Errorf("error should mention @useConcept rejection on trait, got %v", err)
	}
}

// TestParseSpecMemQL_SpecRequiresBinding locks the rule: specs MUST
// declare exactly one of @useConcept / @useShape.
func TestParseSpecMemQL_SpecRequiresBinding(t *testing.T) {
	src := []byte(`@description("missing binding")
spec specBoom {
  payload.active == true
}`)

	_, err := parseSpecMemQL("test.memql", src)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "binding") {
		t.Errorf("error should mention missing binding, got %v", err)
	}
}

// TestParseSpecMemQL_SpecRejectsDualBinding catches the case where
// an author declares both @useConcept and @useShape on the same spec.
func TestParseSpecMemQL_SpecRejectsDualBinding(t *testing.T) {
	src := []byte(`@useConcept(foo)
@useShape(fooFull)
spec specDualBound {
  payload.active == true
}`)

	_, err := parseSpecMemQL("test.memql", src)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "both") && !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("error should reject dual binding, got %v", err)
	}
}

// TestParseSpecMemQL_RejectsLifecycleOnSpec locks Decision: the
// engine controls spec lifecycle. @enabled / @disabled on a spec
// is an error; the same annotations are no-op on traits.
func TestParseSpecMemQL_RejectsLifecycleOnSpec(t *testing.T) {
	src := []byte(`@enabled
@useConcept(participant)
spec specBoom {
  participant.active == true
}`)

	_, err := parseSpecMemQL("test.memql", src)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "@enabled") {
		t.Errorf("error should mention @enabled rejection on spec, got %v", err)
	}
}

// TestParseSpecMemQL_RejectsUnknownAnnotation locks the
// annotation allow-list.
func TestParseSpecMemQL_RejectsUnknownAnnotation(t *testing.T) {
	src := []byte(`@bogusKnob
@useConcept(participant)
spec specFoo {
  participant.active == true
}`)

	_, err := parseSpecMemQL("test.memql", src)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "bogusKnob") {
		t.Errorf("error should mention bogusKnob, got %v", err)
	}
}

// TestParseSpecMemQL_TraitGoldenPath locks the trait surface.
func TestParseSpecMemQL_TraitGoldenPath(t *testing.T) {
	src := []byte(`@enabled
@description("Records with active==true.")
trait traitIsActiveRecord {
  payload.active == true
}`)

	got, err := parseSpecMemQL("test.memql", src)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil *Spec")
	}
	if !got.IsTrait {
		t.Error("IsTrait = false, want true")
	}
	if got.Name != "traitIsActiveRecord" {
		t.Errorf("Name = %q, want traitIsActiveRecord", got.Name)
	}
}
