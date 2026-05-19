package parser

import (
	"testing"
)

// TestParseUseDeclaration_FormA_BareConcept locks the legacy single-
// concept import shape.
func TestParseUseDeclaration_FormA_BareConcept(t *testing.T) {
	source := `use cognition.participant
query queryFoo { filter id == args.id; shape participantFull }`
	tokens, err := NewLexer(source).Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	p := NewParser(tokens)
	file := &File{}
	for p.check(TokenKeywordUse) {
		decl, err := p.parseUseDeclaration()
		if err != nil {
			t.Fatalf("parseUseDeclaration: %v", err)
		}
		file.Uses = append(file.Uses, decl)
	}
	if len(file.Uses) != 1 {
		t.Fatalf("got %d use decls, want 1", len(file.Uses))
	}
	u := file.Uses[0]
	if u.Path != "cognition.participant" {
		t.Errorf("Path = %q, want cognition.participant", u.Path)
	}
	if len(u.Names) != 0 {
		t.Errorf("Form A use should not populate Names; got %v", u.Names)
	}
}

// TestParseUseDeclaration_FormA_WithAlias locks `use path as alias`.
func TestParseUseDeclaration_FormA_WithAlias(t *testing.T) {
	source := `use cognition.session as cognSess
query queryFoo { filter id == args.id; shape sessionFull }`
	tokens, err := NewLexer(source).Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	p := NewParser(tokens)
	decl, err := p.parseUseDeclaration()
	if err != nil {
		t.Fatalf("parseUseDeclaration: %v", err)
	}
	if decl.Alias != "cognSess" {
		t.Errorf("Alias = %q, want cognSess", decl.Alias)
	}
	if len(decl.Names) != 0 {
		t.Errorf("Form A should not populate Names; got %v", decl.Names)
	}
}

// TestParseUseDeclaration_FormB_SingleLine locks the canonical
// post-migration shape: `use path.{ name, name }`.
func TestParseUseDeclaration_FormB_SingleLine(t *testing.T) {
	source := `use cognition.shapes.{ participantFull, sessionFull }
query queryFoo { filter id == args.id; shape participantFull }`
	tokens, err := NewLexer(source).Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	p := NewParser(tokens)
	decl, err := p.parseUseDeclaration()
	if err != nil {
		t.Fatalf("parseUseDeclaration: %v", err)
	}
	if decl.Path != "cognition.shapes" {
		t.Errorf("Path = %q, want cognition.shapes", decl.Path)
	}
	wantNames := []string{"participantFull", "sessionFull"}
	if len(decl.Names) != len(wantNames) {
		t.Fatalf("Names = %v, want %v", decl.Names, wantNames)
	}
	for i, want := range wantNames {
		if decl.Names[i] != want {
			t.Errorf("Names[%d] = %q, want %q", i, decl.Names[i], want)
		}
	}
	if decl.Alias != "" {
		t.Errorf("Form B should not populate Alias; got %q", decl.Alias)
	}
}

// TestParseUseDeclaration_FormB_MultiLine confirms the brace body
// can span multiple lines (the lexer strips newlines).
func TestParseUseDeclaration_FormB_MultiLine(t *testing.T) {
	source := `use common.traits.{
    traitIsActiveRecord,
    traitIsNotDeleted,
    traitStatusIsActive,
}
query queryFoo { filter id == args.id; shape someFull }`
	tokens, err := NewLexer(source).Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	p := NewParser(tokens)
	decl, err := p.parseUseDeclaration()
	if err != nil {
		t.Fatalf("parseUseDeclaration: %v", err)
	}
	if decl.Path != "common.traits" {
		t.Errorf("Path = %q, want common.traits", decl.Path)
	}
	if len(decl.Names) != 3 {
		t.Fatalf("Names = %v, want 3 entries", decl.Names)
	}
}

// TestParseUseDeclaration_FormB_RejectsEmpty locks the rule that an
// empty Form B import list is not valid.
func TestParseUseDeclaration_FormB_RejectsEmpty(t *testing.T) {
	source := `use common.traits.{ }`
	tokens, err := NewLexer(source).Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	p := NewParser(tokens)
	if _, err := p.parseUseDeclaration(); err == nil {
		t.Fatal("expected error for empty Form B name list, got nil")
	}
}

// TestParseUseDeclaration_FormB_DottedNestedPath locks support for
// multi-segment module paths.
func TestParseUseDeclaration_FormB_DottedNestedPath(t *testing.T) {
	source := `use agents.roles.agriculture.{ row_crop_farmer, livestock_rancher }`
	tokens, err := NewLexer(source).Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	p := NewParser(tokens)
	decl, err := p.parseUseDeclaration()
	if err != nil {
		t.Fatalf("parseUseDeclaration: %v", err)
	}
	if decl.Path != "agents.roles.agriculture" {
		t.Errorf("Path = %q, want agents.roles.agriculture", decl.Path)
	}
	if len(decl.Names) != 2 {
		t.Errorf("Names count = %d, want 2", len(decl.Names))
	}
}
