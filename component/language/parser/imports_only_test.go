package parser

import (
	"testing"
)

// TestExtractImports_ShapeFile locks the canonical motivating case:
// a shape file whose body uses a dedicated runtime parser, but whose
// import block should still be validated by the lint pipeline.
func TestExtractImports_ShapeFile(t *testing.T) {
	source := `import (
		"../participant"
		"../common/space"
	)

@row
@useConcept(participant)
@description("Participant projection")
shape participantFull {
  row.id
  participant.spaceId
  participant.status
}`
	file, err := ExtractImports(source)
	if err != nil {
		t.Fatalf("ExtractImports: %v", err)
	}
	if len(file.Imports) != 2 {
		t.Fatalf("got %d imports, want 2", len(file.Imports))
	}
	if file.Imports[0].Path != "../participant" {
		t.Errorf("imports[0].Path = %q, want %q", file.Imports[0].Path, "../participant")
	}
}

// TestExtractImports_ProviderFile locks tolerance of a provider
// declaration in the body.
func TestExtractImports_ProviderFile(t *testing.T) {
	source := `import (
		"./openai_base"
	)

@description("GPT-4 chat provider")
@extends("openai")
provider chat54 {
  params {
    contextWindow 128000
  }
}`
	file, err := ExtractImports(source)
	if err != nil {
		t.Fatalf("ExtractImports: %v", err)
	}
	if len(file.Imports) != 1 {
		t.Fatalf("got %d imports, want 1", len(file.Imports))
	}
}

// TestExtractImports_BuiltinFile locks tolerance for builtin bodies.
func TestExtractImports_BuiltinFile(t *testing.T) {
	source := `import (
		"./contracts"
	)

@enabled
@executor("integration.email.sendEmail")
builtin emailSend {
  to       string  @required
  subject  string  @required
  body     string  @required
}`
	file, err := ExtractImports(source)
	if err != nil {
		t.Fatalf("ExtractImports: %v", err)
	}
	if len(file.Imports) != 1 {
		t.Fatalf("got %d imports, want 1", len(file.Imports))
	}
}

// TestExtractImports_NoImports locks the empty-imports case (file
// uses legacy use declarations or has no imports at all).
func TestExtractImports_NoImports(t *testing.T) {
	source := `@description("just a body")
shape foo {
  row.id
}`
	file, err := ExtractImports(source)
	if err != nil {
		t.Fatalf("ExtractImports: %v", err)
	}
	if len(file.Imports) != 0 {
		t.Errorf("got %d imports, want 0", len(file.Imports))
	}
}

// TestExtractImports_OnlyImports locks the no-body case.
func TestExtractImports_OnlyImports(t *testing.T) {
	source := `import (
		"./foo"
		"./bar" as b
	)`
	file, err := ExtractImports(source)
	if err != nil {
		t.Fatalf("ExtractImports: %v", err)
	}
	if len(file.Imports) != 2 {
		t.Fatalf("got %d imports, want 2", len(file.Imports))
	}
	if file.Imports[1].Alias != "b" {
		t.Errorf("imports[1].Alias = %q, want b", file.Imports[1].Alias)
	}
}

// (TestExtractImports_LegacyUseStripped was retired in PR C
// together with the Form A `use <ns>.<concept>` parser arm. Form A
// is no longer accepted at the lex/parse layer; ExtractImports
// would now reject the example source it used to validate.)

// TestExtractImports_BadImportSurfaces locks that malformed import
// blocks DO error even when the rest of the file would be tolerated.
func TestExtractImports_BadImportSurfaces(t *testing.T) {
	source := `import (
		"./foo" as
	)
shape body { row.id }`
	_, err := ExtractImports(source)
	if err == nil {
		t.Fatal("expected error for malformed import, got nil")
	}
}

// TestExtractImports_EmptyFile locks the trivial case.
func TestExtractImports_EmptyFile(t *testing.T) {
	file, err := ExtractImports("")
	if err != nil {
		t.Fatalf("ExtractImports(\"\"): %v", err)
	}
	if file == nil {
		t.Fatal("file should not be nil")
	}
	if len(file.Imports) != 0 {
		t.Errorf("got %d imports, want 0", len(file.Imports))
	}
}
