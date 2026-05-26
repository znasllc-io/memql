package parser

import (
	"fmt"

	"github.com/znasllc-io/memql/component/language/ast"
)

// ParsePolicyDecl tokenises a single `policy NAME { }` slice (with
// its leading attribute set) and returns the typed *ast.PolicyDecl.
// The caller -- typically the unified policy loader -- has already
// sliced one policy out of a multi-construct `.memql` file via
// ExtractKeywordSlices, so the input here is expected to contain
// exactly one policy declaration.
//
// Returns an error when the slice's syntax is malformed or doesn't
// produce a policy node.
func ParsePolicyDecl(source string) (*ast.PolicyDecl, error) {
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
	decl, ok := def.(*ast.PolicyDecl)
	if !ok {
		return nil, fmt.Errorf("expected policy declaration, got %T", def)
	}
	return decl, nil
}
