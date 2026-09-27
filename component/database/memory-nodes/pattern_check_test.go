package memoryNodes

import (
	"errors"
	"strings"
	"testing"
)

// TestConceptFieldPatternIsCompiledAtLoad (memql#5426): a concept field's
// @pattern that is not a regular expression is refused when the concept is
// built, at every depth a field can be declared, with the rule id and the
// field named -- not when the schema is first compiled, which is the
// concept's first write, and which then refused every write to it.
func TestConceptFieldPatternIsCompiledAtLoad(t *testing.T) {
	cases := []struct {
		name, src, field string
	}{
		{"a top-level field", "concept probe {\n  slug  string  @pattern(\"^[a-z\")\n}\n", `property "slug"`},
		{"a nested block's field", "concept probe {\n  details {\n    code  string  @pattern(\"(unclosed\")\n  }\n}\n", `nested property "code"`},
		{"a list's element", "concept probe {\n  tags  []string  @pattern(\"[z-a]\")\n}\n", `property "tags"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := buildConcept(t, c.src)
			if err == nil {
				t.Fatal("the concept built with a @pattern that is not a regular expression")
			}
			var pe *PatternError
			if !errors.As(err, &pe) {
				t.Fatalf("the refusal is not a *PatternError: %v", err)
			}
			if pe.RuleCode() != CodePatternInvalid || !strings.HasSuffix(err.Error(), "["+CodePatternInvalid+"]") {
				t.Errorf("the refusal does not carry %s last: %v", CodePatternInvalid, err)
			}
			if !strings.Contains(err.Error(), c.field) {
				t.Errorf("the refusal does not name %s: %v", c.field, err)
			}
		})
	}
}

// TestConceptFieldValidPatternStillBuilds is the control: a valid expression
// builds, and reaches the schema as written.
func TestConceptFieldValidPatternStillBuilds(t *testing.T) {
	c, err := buildConcept(t, "concept probe {\n  slug  string  @pattern(\"^[a-z][a-z0-9-]*$\")\n}\n")
	if err != nil {
		t.Fatalf("a valid @pattern was refused: %v", err)
	}
	raw, err := c.DefinitionSchema()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"pattern":"^[a-z][a-z0-9-]*$"`) {
		t.Errorf("the pattern did not reach the schema: %s", raw)
	}
}
