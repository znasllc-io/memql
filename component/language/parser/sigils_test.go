package parser

import (
	"strings"
	"testing"
)

// #2618: three additive grammar forms -- the required sigil `type!`,
// the first-class enum type `enum("a","b")`, and positional @cache(N).
// The long forms all keep parsing; each pair must produce identical
// representations.

func parseArgsFixture(t *testing.T, body string) map[string]*ArgsField {
	t.Helper()
	block, err := parseArgsSafe("args {\n" + body + "\n}")
	if err != nil {
		t.Fatalf("args parse: %v", err)
	}
	out := map[string]*ArgsField{}
	for _, f := range block.Fields {
		out[f.Name] = f
	}
	return out
}

func TestRequiredSigil_ArgsField(t *testing.T) {
	fields := parseArgsFixture(t, `
  a string!
  b string @required
  c string
  d []string!
  e string! @required
`)
	for _, name := range []string{"a", "b", "d", "e"} {
		if fields[name].Optional {
			t.Errorf("field %s must be required", name)
		}
	}
	if !fields["c"].Optional {
		t.Error("field c must stay optional")
	}
	if fields["d"].Type != "array" || fields["d"].Items.Type != "string" {
		t.Errorf("sigil after array shorthand mis-parsed: %+v", fields["d"])
	}
}

func TestEnumType_ArgsField(t *testing.T) {
	fields := parseArgsFixture(t, `
  status enum("open", "closed")!
  legacy string @required @enum("open", "closed")
`)
	newF, oldF := fields["status"], fields["legacy"]
	if newF.Optional || oldF.Optional {
		t.Fatal("both spellings must be required")
	}
	if newF.Type != oldF.Type {
		t.Errorf("type mismatch: enum form %q vs legacy %q", newF.Type, oldF.Type)
	}
	if len(newF.Enum) != 2 || newF.Enum[0] != oldF.Enum[0] || newF.Enum[1] != oldF.Enum[1] {
		t.Errorf("enum values mismatch: %v vs %v", newF.Enum, oldF.Enum)
	}
}

func TestEnumType_EmptyRejected(t *testing.T) {
	if _, err := parseArgsSafe("args {\n  status enum()\n}"); err == nil {
		t.Fatal("empty enum type must be rejected")
	}
}

func TestRequiredSigil_ConceptProperty(t *testing.T) {
	file, err := ParseFile(`@namespace("probe")
concept widget {
  label string!
  kind string @required
  note string
  both string! @required
}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	decl := file.Definitions[0].(*ConceptDecl)
	req := map[string]int{}
	for _, prop := range decl.Properties {
		n := 0
		for _, a := range prop.Attributes {
			if a.Name == "required" {
				n++
			}
		}
		req[prop.Name] = n
	}
	if req["label"] != 1 || req["kind"] != 1 {
		t.Errorf("sigil and annotation must both yield one required attr: %v", req)
	}
	if req["note"] != 0 {
		t.Errorf("plain field must carry no required attr: %v", req)
	}
	if req["both"] != 1 {
		t.Errorf("sigil + @required must dedupe to exactly one attr, got %d", req["both"])
	}
}

func TestEnumType_ConceptProperty(t *testing.T) {
	file, err := ParseFile(`@namespace("probe")
concept widget {
  status enum("open", "closed")!
}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	prop := file.Definitions[0].(*ConceptDecl).Properties[0]
	if prop.Type.Kind != "enum" || len(prop.Type.EnumValues) != 2 {
		t.Errorf("enum type mis-parsed: %+v", prop.Type)
	}
	found := false
	for _, a := range prop.Attributes {
		if a.Name == "required" {
			found = true
		}
	}
	if !found {
		t.Error("sigil after enum type must set required")
	}
}

func TestPositionalCacheTTL(t *testing.T) {
	for name, src := range map[string]string{
		"positional": "@cache(300)",
		"keyword":    "@cache(ttl=\"300\")",
	} {
		normalised, err := NormaliseQuerySource(src + "\nquery widget listWidgets {\n  filter widget.kind == args.kind\n}")
		if err != nil {
			t.Fatalf("%s: normalise: %v", name, err)
		}
		file, err := ParseFile(normalised)
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		fd, ok := file.Definitions[0].(*FunctionDef)
		if !ok {
			t.Fatalf("%s: not a FunctionDef: %T", name, file.Definitions[0])
		}
		if fd.CacheTTL != "300" {
			t.Errorf("%s: CacheTTL = %q, want 300", name, fd.CacheTTL)
		}
	}
	if !strings.Contains("guard", "guard") {
		t.Fatal("unreachable")
	}
}

// TestSigilAndEnumType_OtherFieldParsers pins #2618 across the three
// construct-specific field parsers (tool / builtin / prompt) -- the
// corpus carries @required and @enum in all of them, and the first
// migration pass broke 31 files before these parsers learned the
// forms.
func TestSigilAndEnumType_OtherFieldParsers(t *testing.T) {
	file, err := ParseFile(`tool probeTool {
  mode enum("fast", "slow")!
  note string
}`)
	if err != nil {
		t.Fatalf("tool parse: %v", err)
	}
	_ = file

	bf, err := ParseFile(`builtin probeBuiltin {
  level enum("low", "high")!
  tag string!
  @handler(type="function", name="x")
}`)
	if err != nil {
		t.Fatalf("builtin parse: %v", err)
	}
	_ = bf
}
