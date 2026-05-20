package ast

import (
	"strings"
	"testing"
)

// sv is a tiny constructor helper for SemverVersion in tests.
func sv(major, minor, patch int64) SemverVersion {
	return SemverVersion{Major: major, Minor: minor, Patch: patch}
}

// TestValidateAssemblyInputs_HappyPaths locks the well-formed cases.
func TestValidateAssemblyInputs_HappyPaths(t *testing.T) {
	cases := []struct {
		version   SemverVersion
		namespace string
		name      string
	}{
		{sv(1, 0, 0), "cognition", "participant"},
		{sv(1, 0, 0), "common", "agent"},
		{sv(1, 0, 0), "copresent", "canvasState"},
		{sv(1, 2, 3), "identity", "auditEvent"},
		{sv(1, 0, 0), "cluster", "nodeType"},
		{sv(2, 0, 0), "future", "thing"},
		{sv(42, 7, 1), "weird", "name"},
	}
	for _, c := range cases {
		if err := ValidateAssemblyInputs(c.version, c.namespace, c.name); err != nil {
			t.Errorf("ValidateAssemblyInputs(%s, %q, %q): unexpected error %v", c.version, c.namespace, c.name, err)
		}
	}
}

// TestValidateAssemblyInputs_RejectsBadVersion locks the
// major-must-be-positive rule.
func TestValidateAssemblyInputs_RejectsBadVersion(t *testing.T) {
	for _, v := range []SemverVersion{sv(0, 0, 0), sv(-1, 0, 0)} {
		err := ValidateAssemblyInputs(v, "ns", "name")
		if err == nil {
			t.Errorf("expected error for version=%s, got nil", v)
		}
	}
}

// TestValidateAssemblyInputs_RejectsBadNamespace locks the namespace
// pattern. Colon-separated namespaces are valid; dotted ones,
// PascalCase, dashes, spaces, leading digits, and empty segments are
// all rejected.
func TestValidateAssemblyInputs_RejectsBadNamespace(t *testing.T) {
	cases := []string{
		"",                 // empty
		"Cognition",        // PascalCase
		"1cognition",       // leading digit
		"cognition-foo",    // dash
		"cognition foo",    // space
		":cognition",       // leading colon
		"cognition:",       // trailing colon
		"cog::foo",         // empty segment
		"cog:Foo",          // PascalCase segment
		"cog.foo",          // dot separator no longer allowed
		"cognition.client", // dot separator no longer allowed
	}
	for _, ns := range cases {
		err := ValidateAssemblyInputs(sv(1, 0, 0), ns, "name")
		if err == nil {
			t.Errorf("expected error for namespace=%q, got nil", ns)
		}
	}
}

// TestValidateAssemblyInputs_AcceptsColonNamespace locks the
// nested-concept path: colon-separated namespace segments expand to
// colon-separated concept IDs.
func TestValidateAssemblyInputs_AcceptsColonNamespace(t *testing.T) {
	for _, ns := range []string{
		"cognition:client:tool",
		"cognition:participant",
		"copresent:unmet:capability",
	} {
		if err := ValidateAssemblyInputs(sv(1, 0, 0), ns, "request"); err != nil {
			t.Errorf("ValidateAssemblyInputs for namespace=%q: unexpected error %v", ns, err)
		}
	}
}

// TestAssembleConceptId_ColonNamespace locks the colon-namespace
// passthrough at assembly time.
func TestAssembleConceptId_ColonNamespace(t *testing.T) {
	cases := []struct {
		ns, name, want string
	}{
		{"cognition:client:tool", "request", "v1:cognition:client:tool:request"},
		{"cognition:participant", "presence", "v1:cognition:participant:presence"},
		{"copresent:unmet", "capability", "v1:copresent:unmet:capability"},
	}
	for _, c := range cases {
		got := AssembleConceptId(sv(1, 0, 0), c.ns, c.name)
		if got != c.want {
			t.Errorf("AssembleConceptId(1.0.0, %q, %q) = %q, want %q", c.ns, c.name, got, c.want)
		}
	}
}

// TestValidateAssemblyInputs_RejectsBadName locks the name pattern.
func TestValidateAssemblyInputs_RejectsBadName(t *testing.T) {
	cases := []string{"", "Participant", "1foo", "_internal", "foo bar", "foo-bar"}
	for _, name := range cases {
		err := ValidateAssemblyInputs(sv(1, 0, 0), "ns", name)
		if err == nil {
			t.Errorf("expected error for name=%q, got nil", name)
		}
	}
}

// TestAssembleConceptId locks the format string. Major is the only
// version segment that flows into the prefix.
func TestAssembleConceptId(t *testing.T) {
	cases := []struct {
		version   SemverVersion
		namespace string
		name      string
		want      string
	}{
		{sv(1, 0, 0), "cognition", "participant", "v1:cognition:participant"},
		{sv(1, 5, 3), "copresent", "canvasState", "v1:copresent:canvasState"},
		{sv(2, 0, 0), "future", "thing", "v2:future:thing"},
	}
	for _, c := range cases {
		got := AssembleConceptId(c.version, c.namespace, c.name)
		if got != c.want {
			t.Errorf("AssembleConceptId(%s, %q, %q) = %q, want %q", c.version, c.namespace, c.name, got, c.want)
		}
	}
}

// TestParseSemver locks the strict-format parser. Anything other
// than X.Y.Z (three non-negative integer segments) is rejected.
func TestParseSemver(t *testing.T) {
	ok := map[string]SemverVersion{
		"1.0.0":     sv(1, 0, 0),
		"2.5.7":     sv(2, 5, 7),
		"100.200.0": sv(100, 200, 0),
	}
	for s, want := range ok {
		got, err := ParseSemver(s)
		if err != nil {
			t.Errorf("ParseSemver(%q): unexpected error %v", s, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSemver(%q) = %s, want %s", s, got, want)
		}
	}
	rejected := []string{
		"",          // empty
		"1",         // single-segment
		"1.0",       // two-segment
		"1.0.0.0",   // four-segment
		"v1.0.0",    // leading v
		"1.0.0-rc1", // suffix
		"1.0.0+meta",
		"1.x.0", // non-integer
		"0.1.0", // major must be >= 1
	}
	for _, s := range rejected {
		_, err := ParseSemver(s)
		if err == nil {
			t.Errorf("ParseSemver(%q): expected error, got nil", s)
		}
	}
}

// TestExtractVersionAttribute_HappyPath locks the @version("1.0.0") shape.
func TestExtractVersionAttribute_HappyPath(t *testing.T) {
	attrs := []*Attribute{
		{Name: "description", Value: "foo"},
		{Name: "version", Value: "1.0.0"},
	}
	v, ok, err := ExtractVersionAttribute(attrs)
	if err != nil {
		t.Fatalf("ExtractVersionAttribute: %v", err)
	}
	if !ok || v != sv(1, 0, 0) {
		t.Errorf("got (%s, %v), want (1.0.0, true)", v, ok)
	}
}

// TestExtractVersionAttribute_RejectsIntegerForm locks that the
// legacy integer @version(1) form is rejected (strict-semver only).
func TestExtractVersionAttribute_RejectsIntegerForm(t *testing.T) {
	for _, v := range []any{int64(1), 1} {
		attrs := []*Attribute{{Name: "version", Value: v}}
		_, _, err := ExtractVersionAttribute(attrs)
		if err == nil {
			t.Fatalf("expected error for integer-typed @version(%v), got nil", v)
		}
	}
}

// TestExtractVersionAttribute_Absent locks the "no attribute" return.
func TestExtractVersionAttribute_Absent(t *testing.T) {
	attrs := []*Attribute{{Name: "description", Value: "foo"}}
	_, ok, err := ExtractVersionAttribute(attrs)
	if err != nil {
		t.Fatalf("ExtractVersionAttribute: %v", err)
	}
	if ok {
		t.Error("ok should be false when attribute absent")
	}
}

// TestExtractNamespaceAttribute_HappyPath locks the @namespace("x") shape.
func TestExtractNamespaceAttribute_HappyPath(t *testing.T) {
	attrs := []*Attribute{{Name: "namespace", Value: "cognition"}}
	v, ok, err := ExtractNamespaceAttribute(attrs)
	if err != nil {
		t.Fatalf("ExtractNamespaceAttribute: %v", err)
	}
	if !ok || v != "cognition" {
		t.Errorf("got (%q, %v), want (\"cognition\", true)", v, ok)
	}
}

// TestExtractNamespaceAttribute_RejectsBadValue locks both the type
// check and the pattern check.
func TestExtractNamespaceAttribute_RejectsBadValue(t *testing.T) {
	cases := []any{
		int64(1),       // wrong type
		"Cognition",    // not lowercase
		"my-namespace", // has dash
		"my namespace", // has space
		"",             // empty
		"cog.foo",      // dot separator no longer allowed
	}
	for _, val := range cases {
		attrs := []*Attribute{{Name: "namespace", Value: val}}
		_, _, err := ExtractNamespaceAttribute(attrs)
		if err == nil {
			t.Errorf("expected error for @namespace=%v, got nil", val)
		}
	}
}

// TestAssembleConceptIdFromDecl_HappyPath locks the end-to-end
// declaration-to-ID flow.
func TestAssembleConceptIdFromDecl_HappyPath(t *testing.T) {
	decl := &ConceptDecl{
		Name: "participant",
		Attributes: []*Attribute{
			{Name: "version", Value: "1.0.0"},
			{Name: "namespace", Value: "cognition"},
		},
	}
	id, err := AssembleConceptIdFromDecl(decl)
	if err != nil {
		t.Fatalf("AssembleConceptIdFromDecl: %v", err)
	}
	if id != "v1:cognition:participant" {
		t.Errorf("got %q, want v1:cognition:participant", id)
	}
}

// TestAssembleConceptIdFromDecl_PartialAttributes locks the
// transitional-state path: when @version or @namespace is missing
// the function returns "" + nil so the caller can decide whether to
// warn or error.
func TestAssembleConceptIdFromDecl_PartialAttributes(t *testing.T) {
	cases := []*ConceptDecl{
		{Name: "x", Attributes: []*Attribute{{Name: "version", Value: "1.0.0"}}},
		{Name: "x", Attributes: []*Attribute{{Name: "namespace", Value: "ns"}}},
		{Name: "x", Attributes: []*Attribute{}},
	}
	for _, decl := range cases {
		id, err := AssembleConceptIdFromDecl(decl)
		if err != nil {
			t.Errorf("AssembleConceptIdFromDecl: unexpected error: %v", err)
		}
		if id != "" {
			t.Errorf("got id=%q, want \"\" for partial attributes", id)
		}
	}
}

// TestAssembleConceptIdFromDecl_BadNameSurfaces locks that
// invalid concept names produce errors at assembly time.
func TestAssembleConceptIdFromDecl_BadNameSurfaces(t *testing.T) {
	decl := &ConceptDecl{
		Name: "Participant", // PascalCase, not allowed
		Attributes: []*Attribute{
			{Name: "version", Value: "1.0.0"},
			{Name: "namespace", Value: "cognition"},
		},
	}
	_, err := AssembleConceptIdFromDecl(decl)
	if err == nil {
		t.Fatal("expected error for PascalCase concept name, got nil")
	}
	if !strings.Contains(err.Error(), "lowercase") {
		t.Errorf("error %q should mention lowercase", err.Error())
	}
}
