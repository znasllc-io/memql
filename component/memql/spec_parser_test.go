package memql

import (
	"strings"
	"testing"
)

// TestParseSpecMemQL_GoldenPath_RowSpec locks the canonical
// struct-form spec syntax for a row-spec. Body authors write
// payload references directly as `payload.X`; the legacy
// `@useConcept(<bareName>)` + bareName-rewrite path was retired
// (see issue #301).
func TestParseSpecMemQL_GoldenPath_RowSpec(t *testing.T) {
	src := []byte(`@description("Matches participants with human participantType.")
spec isHumanParticipant {
  payload.participantType == "human"
}`)

	got, err := parseSpecMemQL("test.memql", src)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil *Spec")
	}
	if got.Name != "isHumanParticipant" {
		t.Errorf("Name = %q, want isHumanParticipant", got.Name)
	}
	if got.IsTrait {
		t.Error("IsTrait = true, want false (this is a spec, not a trait)")
	}
	if got.Kind != SpecKindRow {
		t.Errorf("Kind = %q, want %q (payload.X body classifies as row-spec)", got.Kind, SpecKindRow)
	}
}

// TestParseSpecMemQL_GoldenPath_ContextSpec locks the canonical
// caller-only predicate (no row references; evaluated in-process
// from the auth-context envelope).
func TestParseSpecMemQL_GoldenPath_ContextSpec(t *testing.T) {
	src := []byte(`@description("Actor holds the admin role.")
spec requiresAdmin {
  actor.role == "admin"
}`)

	got, err := parseSpecMemQL("test.memql", src)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if got.Kind != SpecKindContext {
		t.Errorf("Kind = %q, want %q (actor.X body classifies as context-spec)", got.Kind, SpecKindContext)
	}
}

// TestParseSpecMemQL_RejectsLifecycleOnSpec locks: the engine
// controls spec lifecycle. @enabled / @disabled on a spec is an
// error; the same annotations are no-op on traits.
func TestParseSpecMemQL_RejectsLifecycleOnSpec(t *testing.T) {
	src := []byte(`@enabled
spec specBoom {
  payload.active == true
}`)

	_, err := parseSpecMemQL("test.memql", src)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "@enabled") {
		t.Errorf("error should mention @enabled rejection on spec, got %v", err)
	}
}

// TestParseSpecMemQL_RejectsUnknownAnnotation locks the annotation
// allow-list. Includes the retired legacy bindings @useConcept /
// @useShape (#301) -- both should now error as unknown.
func TestParseSpecMemQL_RejectsUnknownAnnotation(t *testing.T) {
	cases := []struct {
		name        string
		src         string
		mustMention string
	}{
		{"bogus", `@bogusKnob
spec specFoo {
  payload.active == true
}`, "bogusKnob"},
		{"retired useConcept", `@useConcept(participant)
spec specFoo {
  payload.active == true
}`, "useConcept"},
		{"retired useShape", `@useShape(participantFull)
spec specFoo {
  payload.active == true
}`, "useShape"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSpecMemQL("test.memql", []byte(tc.src))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.mustMention) {
				t.Errorf("error should mention %q, got %v", tc.mustMention, err)
			}
		})
	}
}

// TestParseSpecMemQL_TraitGoldenPath locks the trait surface.
func TestParseSpecMemQL_TraitGoldenPath(t *testing.T) {
	src := []byte(`@enabled
@description("Records with active==true.")
trait isActiveRecord {
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
	if got.Name != "isActiveRecord" {
		t.Errorf("Name = %q, want isActiveRecord", got.Name)
	}
}
