package parser

import (
	"strings"
	"testing"
)

// TestExtractPredicateDeclarationSlices pins the extent of an edition-2026
// brace-less spec or trait (memql#5364): preamble through the expression's
// last character, continuation lines included, trailing comment excluded.
func TestExtractPredicateDeclarationSlices(t *testing.T) {
	src := `use agents.concepts.{ agent }

/// Assistants only.
@enabled
spec agent isAssistant = row => row.role == "assistant"
                         && row.kind != "system"   // not the system agent

/// A braced spec beside it is not this slicer's.
spec agent isLegacy = row => row.role == "x"

/* spec agent commentedOut = row => row.a == 1 */
trait isActiveRecord = row => row.active == true
query agent q {
  filter  row => row.id == args.id
}
`
	specs := ExtractPredicateDeclarationSlices(src, "spec")
	if len(specs) != 1 || specs[0].Name != "isAssistant" {
		t.Fatalf("spec slices = %+v, want isAssistant alone", specs)
	}
	want := "/// Assistants only.\n@enabled\nspec agent isAssistant = row => row.role == \"assistant\"\n                         && row.kind != \"system\""
	if specs[0].Source != want {
		t.Errorf("slice = %q\nwant    %q", specs[0].Source, want)
	}
	if src[specs[0].Start:specs[0].End] != specs[0].Source {
		t.Error("Start/End do not index the slice's own text")
	}
	if strings.Contains(specs[0].Source, "//") && !strings.Contains(specs[0].Source, "///") {
		t.Error("the trailing comment leaked into the slice")
	}

	traits := ExtractPredicateDeclarationSlices(src, "trait")
	if len(traits) != 1 || traits[0].Name != "isActiveRecord" || traits[0].Source != "trait isActiveRecord = row => row.active == true" {
		t.Fatalf("trait slices = %+v", traits)
	}
	// The slice parses on its own.
	if _, err := ParseSpecDecl(traits[0].Source); err != nil {
		t.Errorf("the trait slice does not parse alone: %v", err)
	}
	if _, err := ParseSpecDecl(specs[0].Source); err != nil {
		t.Errorf("the spec slice does not parse alone: %v", err)
	}

	// `==` is never a header's `=`, and a header nested in a body is not a
	// top-level declaration.
	if got := ExtractPredicateDeclarationSlices("spec a b == c\n", "spec"); len(got) != 0 {
		t.Errorf("`==` read as a declaration: %+v", got)
	}
	if got := ExtractPredicateDeclarationSlices("logic x {\n  spec a b = row => row.c\n}\n", "spec"); len(got) != 0 {
		t.Errorf("a nested header read as top-level: %+v", got)
	}
}
