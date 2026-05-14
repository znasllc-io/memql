package parser

import (
	"testing"
)

// TestParser_VersionAttribute_OnConcept locks the new @version(N)
// attribute parsing as an integer literal. Required for the
// structural concept-ID assembly (docs/dsl-import-model-refactor.md
// decision #11).
func TestParser_VersionAttribute_OnConcept(t *testing.T) {
	source := `@version(1)
@namespace("cognition")
@description("test")
concept participant {
  name string
}`
	file, err := ParseFile(source)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(file.Definitions) != 1 {
		t.Fatalf("got %d definitions, want 1", len(file.Definitions))
	}
	decl, ok := file.Definitions[0].(*ConceptDecl)
	if !ok {
		t.Fatalf("expected *ConceptDecl, got %T", file.Definitions[0])
	}

	var versionAttr, namespaceAttr *Attribute
	for _, a := range decl.Attributes {
		if a.Name == "version" {
			versionAttr = a
		}
		if a.Name == "namespace" {
			namespaceAttr = a
		}
	}

	if versionAttr == nil {
		t.Fatal("missing @version attribute")
	}
	if v, ok := versionAttr.Value.(int64); !ok || v != 1 {
		t.Errorf("@version value = %v (%T), want int64(1)", versionAttr.Value, versionAttr.Value)
	}

	if namespaceAttr == nil {
		t.Fatal("missing @namespace attribute")
	}
	if v, ok := namespaceAttr.Value.(string); !ok || v != "cognition" {
		t.Errorf("@namespace value = %v (%T), want string \"cognition\"", namespaceAttr.Value, namespaceAttr.Value)
	}
}

// TestParser_VersionAttribute_HigherVersions locks that any
// reasonable integer is accepted as @version(N), preparing the
// ground for schema evolution.
func TestParser_VersionAttribute_HigherVersions(t *testing.T) {
	cases := []int64{1, 2, 5, 10, 42, 100}
	for _, want := range cases {
		source := `@version(` + fmtInt(want) + `)
@namespace("foo")
concept bar { x string }`
		file, err := ParseFile(source)
		if err != nil {
			t.Errorf("ParseFile for @version(%d): %v", want, err)
			continue
		}
		decl := file.Definitions[0].(*ConceptDecl)
		got := int64(-1)
		for _, a := range decl.Attributes {
			if a.Name == "version" {
				if v, ok := a.Value.(int64); ok {
					got = v
				}
			}
		}
		if got != want {
			t.Errorf("@version(%d): got %d", want, got)
		}
	}
}

// TestParser_VersionAttribute_CoexistsWithLegacyConcepts locks that
// the new attributes are additive -- existing concepts without
// @version/@namespace still parse.
func TestParser_VersionAttribute_CoexistsWithLegacyConcepts(t *testing.T) {
	source := `@description("legacy concept, no version/namespace yet")
@scope("global")
concept oldStyle {
  name string
}`
	_, err := ParseFile(source)
	if err != nil {
		t.Fatalf("legacy concept should still parse: %v", err)
	}
}

func fmtInt(n int64) string {
	if n == 0 {
		return "0"
	}
	out := ""
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		out = string(rune('0'+(n%10))) + out
		n /= 10
	}
	if neg {
		out = "-" + out
	}
	return out
}
