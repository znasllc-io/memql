package parser

import (
	"fmt"

	"github.com/znasllc-io/memql/component/language/ast"
)

// ParseProviderDecl tokenises a single `provider NAME { ... }` slice
// (with its leading attribute set) and returns the typed
// *ast.ProviderDecl. The caller -- typically the unified provider
// loader -- already sliced one provider out of a multi-construct
// `.memql` file via ExtractKeywordSlices, so the input here is
// expected to contain exactly one provider declaration.
//
// Returns an error when the slice's syntax is malformed or doesn't
// produce a provider node.
func ParseProviderDecl(source string) (*ast.ProviderDecl, error) {
	lexer := NewLexer(source)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return nil, fmt.Errorf("tokenise: %w", err)
	}
	p := NewParser(tokens)

	// The slice arrives with any leading attribute set already in
	// place: walk the same dispatch parseFile would use so the
	// attribute collection + provider keyword recognition stays in
	// one canonical place.
	def, err := p.parseDefinition()
	if err != nil {
		return nil, err
	}
	decl, ok := def.(*ast.ProviderDecl)
	if !ok {
		return nil, fmt.Errorf("expected provider declaration, got %T", def)
	}
	return decl, nil
}
