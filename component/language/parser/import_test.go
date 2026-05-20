package parser

import (
	"testing"
)

// TestParser_ImportBlock_SingleEntry locks the single-entry block
// form. The parser only handles syntax; alias defaulting, path
// resolution, and root-cap checks live in the loader.
func TestParser_ImportBlock_SingleEntry(t *testing.T) {
	source := `import (
		"./cognition/participant"
	)`
	file, err := ParseFile(source)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(file.Imports) != 1 {
		t.Fatalf("want 1 import, got %d", len(file.Imports))
	}
	got := file.Imports[0]
	if got.Path != "./cognition/participant" {
		t.Errorf("Path = %q, want %q", got.Path, "./cognition/participant")
	}
	if got.Alias != "" {
		t.Errorf("Alias = %q, want empty (default)", got.Alias)
	}
}

// TestParser_ImportBlock_MultiEntryWithAlias locks the multi-entry
// block, including the optional `as` clause.
func TestParser_ImportBlock_MultiEntryWithAlias(t *testing.T) {
	source := `import (
		"./cognition/participant"
		"./common/space" as cogSpace
		"../other/participant" as other
	)`
	file, err := ParseFile(source)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(file.Imports) != 3 {
		t.Fatalf("want 3 imports, got %d", len(file.Imports))
	}
	cases := []struct {
		path  string
		alias string
	}{
		{"./cognition/participant", ""},
		{"./common/space", "cogSpace"},
		{"../other/participant", "other"},
	}
	for i, want := range cases {
		got := file.Imports[i]
		if got.Path != want.path {
			t.Errorf("imports[%d].Path = %q, want %q", i, got.Path, want.path)
		}
		if got.Alias != want.alias {
			t.Errorf("imports[%d].Alias = %q, want %q", i, got.Alias, want.alias)
		}
	}
}

// TestParser_ImportBlock_SingleLine locks the no-parens shorthand
// form for one-off imports.
func TestParser_ImportBlock_SingleLine(t *testing.T) {
	source := `import "./common/space"`
	file, err := ParseFile(source)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(file.Imports) != 1 {
		t.Fatalf("want 1 import, got %d", len(file.Imports))
	}
	if file.Imports[0].Path != "./common/space" {
		t.Errorf("Path = %q, want %q", file.Imports[0].Path, "./common/space")
	}
}

// (The two tests that asserted legacy `use <ns>.<concept>` coexistence
// with the Go-style `import (...)` block were retired in PR C
// together with the Form A `use` parser arm; Form A now hard-fails
// at parse time. See TestParseUseDeclaration_FormA_BareConcept_Rejected
// in use_decl_form_b_test.go for the lockdown contract.)

// TestParser_ImportBlock_MissingPath locks the error path when an
// import block opens but contains no string entry.
func TestParser_ImportBlock_MissingPath(t *testing.T) {
	source := `import (
		notAString
	)`
	_, err := ParseFile(source)
	if err == nil {
		t.Fatal("expected parse error for missing-string entry, got nil")
	}
}

// TestParser_ImportBlock_MissingAliasAfterAs locks the error path
// when `as` is followed by a non-identifier.
func TestParser_ImportBlock_MissingAliasAfterAs(t *testing.T) {
	source := `import (
		"./foo" as
	)`
	_, err := ParseFile(source)
	if err == nil {
		t.Fatal("expected parse error for `as` without identifier, got nil")
	}
}
