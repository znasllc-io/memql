package memoryNodes

import (
	"encoding/json"
	"sort"
	"testing"
)

// concept_pii_test.go guards the annotation-driven PII scrub mechanism
// at the schema layer (memql#1711): the @pii field annotation must
// surface as the x-pii custom JSON-Schema keyword, and Concept.PIIFields()
// must enumerate exactly the annotated fields. The engine's hard-delete
// scrub consults PIIFields() so a newly-annotated field is cleared
// automatically -- these tests prove the derivation is generic, not a
// hand-maintained list.

func TestParseConcept_PIIAnnotation_EmitsXPii(t *testing.T) {
	content := []byte(`
@description("A person record.")
concept Person {
  displayName  string  @required  @pii  @description("Shown in UI.")
  email        string  @required  @pii
  age          int     @pii
  tenantId     string  @description("Not PII.")
}
`)

	c, err := ParseConceptMemQL(content, "v1/test/person")
	if err != nil {
		t.Fatalf("ParseConceptMemQL failed: %v", err)
	}

	var schema map[string]any
	if err := json.Unmarshal(c.Schemas["definition"], &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("missing properties in schema")
	}

	wantPii := map[string]bool{
		"displayName": true,
		"email":       true,
		"age":         true,
		"tenantId":    false,
	}
	for field, want := range wantPii {
		fp, ok := props[field].(map[string]any)
		if !ok {
			t.Fatalf("missing property %q", field)
		}
		got, _ := fp["x-pii"].(bool)
		if got != want {
			t.Errorf("property %q x-pii = %v, want %v", field, got, want)
		}
	}
}

func TestConcept_PIIFields_EnumeratesAnnotatedFields(t *testing.T) {
	content := []byte(`
@description("A person record.")
concept Person {
  displayName  string  @required  @pii
  email        string  @required  @pii
  phone        string  @pii
  age          int     @pii
  tenantId     string
}
`)

	c, err := ParseConceptMemQL(content, "v1/test/person")
	if err != nil {
		t.Fatalf("ParseConceptMemQL failed: %v", err)
	}

	got := c.PIIFields()
	sort.Strings(got)
	want := []string{"age", "displayName", "email", "phone"}
	if len(got) != len(want) {
		t.Fatalf("PIIFields() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("PIIFields() = %v, want %v", got, want)
		}
	}
}

// TestConcept_PIIFields_NewlyAnnotatedFieldAutoIncluded is the
// regression contract for "adding a PII field requires only the
// annotation": a concept that gains a brand-new @pii field exposes it
// through PIIFields() with no other code change, so the engine scrub
// covers it automatically.
func TestConcept_PIIFields_NewlyAnnotatedFieldAutoIncluded(t *testing.T) {
	before := []byte(`
concept Person {
  email  string  @required  @pii
}
`)
	after := []byte(`
concept Person {
  email          string  @required  @pii
  socialSecurity string  @pii
}
`)

	cBefore, err := ParseConceptMemQL(before, "v1/test/person")
	if err != nil {
		t.Fatalf("parse before: %v", err)
	}
	cAfter, err := ParseConceptMemQL(after, "v1/test/person")
	if err != nil {
		t.Fatalf("parse after: %v", err)
	}

	if containsStr(cBefore.PIIFields(), "socialSecurity") {
		t.Fatal("socialSecurity should not be PII before annotation")
	}
	if !containsStr(cAfter.PIIFields(), "socialSecurity") {
		t.Fatal("socialSecurity must be auto-included in PIIFields() once @pii-annotated")
	}
}

// TestConcept_PIIFields_NoneWhenUnannotated confirms the common case:
// a concept with no @pii fields returns no PII fields, so @scrubPii is a
// no-op there.
func TestConcept_PIIFields_NoneWhenUnannotated(t *testing.T) {
	content := []byte(`
concept Widget {
  label  string  @required
  count  int
}
`)
	c, err := ParseConceptMemQL(content, "v1/test/widget")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(c.PIIFields()) != 0 {
		t.Fatalf("PIIFields() = %v, want none", c.PIIFields())
	}
}

func containsStr(xs []string, target string) bool {
	for _, x := range xs {
		if x == target {
			return true
		}
	}
	return false
}
