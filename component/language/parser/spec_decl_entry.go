package parser

import (
	"fmt"

	"github.com/znasllc-io/memql/component/language/ast"
)

// ParseSpecDecl tokenises a single `spec NAME { ... }` or
// `trait NAME { ... }` slice (with its leading attribute set) and
// returns the typed *ast.SpecDecl. The caller -- typically the
// unified spec loader -- already sliced one declaration out of a
// multi-construct `.memql` file via ExtractKeywordSlices, so the
// input here is expected to contain exactly one spec or trait.
//
// Returns an error when the slice's syntax is malformed or doesn't
// produce a spec/trait node.
//
// memql#334 (sub-epic #329 / #310 Stage 1C).
func ParseSpecDecl(source string) (*ast.SpecDecl, error) {
	lexer := NewLexer(source)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return nil, fmt.Errorf("tokenise: %w", err)
	}
	p := NewParser(tokens)

	def, err := p.parseDefinition()
	if err != nil {
		return nil, err
	}
	decl, ok := def.(*ast.SpecDecl)
	if !ok {
		return nil, fmt.Errorf("expected spec or trait declaration, got %T", def)
	}
	return decl, nil
}
