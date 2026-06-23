package memoryNodes

import (
	"encoding/json"
	"reflect"
	"testing"
)

// concept_ssot_test.go covers the C5 (memql#2035) "concept is the
// single source of truth for fields" annotations: @internal (server-
// only -- never projected, never accepted from caller args) and
// @serverSet (server-stamped -- never accepted from caller args, but
// projectable). They surface as the x-internal / x-serverSet custom
// JSON-Schema keywords, and the Concept field-classification accessors
// (ProjectableFields / PublicFields / InternalFields / ServerSetFields)
// derive the projectable / writable subsets from them.

func TestParseConcept_InternalServerSet_EmitXKeywords(t *testing.T) {
	content := []byte(`
@description("A space.")
concept Space {
  name        string  @required
  description string
  status      string  @serverSet
  archivedBy  string  @serverSet
  secretSalt  string  @internal
}
`)
	c, err := ParseConceptMemQL(content, "v1/test/space")
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

	check := func(field, keyword string, want bool) {
		t.Helper()
		fp, ok := props[field].(map[string]any)
		if !ok {
			t.Fatalf("missing property %q", field)
		}
		got, _ := fp[keyword].(bool)
		if got != want {
			t.Errorf("property %q %s = %v, want %v", field, keyword, got, want)
		}
	}
	check("secretSalt", "x-internal", true)
	check("status", "x-serverSet", true)
	check("archivedBy", "x-serverSet", true)
	check("name", "x-internal", false)
	check("name", "x-serverSet", false)
	// internal != serverSet and vice-versa.
	check("secretSalt", "x-serverSet", false)
	check("status", "x-internal", false)
}

func TestConcept_FieldClassificationAccessors(t *testing.T) {
	content := []byte(`
concept Space {
  name        string  @required
  description string
  status      string  @serverSet
  lifecycle   string  @serverSet
  secretSalt  string  @internal
}
`)
	c, err := ParseConceptMemQL(content, "v1/test/space")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// ProjectableFields = everything EXCEPT @internal (public + serverSet).
	if got, want := c.ProjectableFields(), []string{"description", "lifecycle", "name", "status"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ProjectableFields() = %v, want %v", got, want)
	}
	// @internal is never projectable.
	if containsStr(c.ProjectableFields(), "secretSalt") {
		t.Errorf("ProjectableFields() must never include the @internal field")
	}
	// PublicFields = neither @internal nor @serverSet.
	if got, want := c.PublicFields(), []string{"description", "name"}; !reflect.DeepEqual(got, want) {
		t.Errorf("PublicFields() = %v, want %v", got, want)
	}
	if got, want := c.InternalFields(), []string{"secretSalt"}; !reflect.DeepEqual(got, want) {
		t.Errorf("InternalFields() = %v, want %v", got, want)
	}
	if got, want := c.ServerSetFields(), []string{"lifecycle", "status"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ServerSetFields() = %v, want %v", got, want)
	}
}

// TestConcept_Unannotated_AllPublic confirms the pre-C6 tree posture:
// a concept with no @internal/@serverSet fields treats every field as
// public + projectable, and the sensitive sets are empty -- so the C5
// gates are no-ops there.
func TestConcept_Unannotated_AllPublic(t *testing.T) {
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
	if got, want := c.ProjectableFields(), []string{"count", "label"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ProjectableFields() = %v, want %v", got, want)
	}
	if got, want := c.PublicFields(), []string{"count", "label"}; !reflect.DeepEqual(got, want) {
		t.Errorf("PublicFields() = %v, want %v", got, want)
	}
	if len(c.InternalFields()) != 0 || len(c.ServerSetFields()) != 0 {
		t.Errorf("unannotated concept must have no internal/serverSet fields")
	}
}
