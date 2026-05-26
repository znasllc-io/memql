package parser

import (
	"fmt"

	"github.com/znasllc-io/memql/component/language/ast"
)

// ParseToolDecl tokenises a single `tool NAME { ... }` slice (with
// its leading attribute set) and returns the typed *ast.ToolDecl.
// The caller -- typically the unified tool loader -- has already
// sliced one tool out of a multi-construct `.memql` file via
// ExtractKeywordSlices, so the input here is expected to contain
// exactly one tool declaration.
//
// Returns an error when the slice's syntax is malformed or doesn't
// produce a tool node.
func ParseToolDecl(source string) (*ast.ToolDecl, error) {
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
	decl, ok := def.(*ast.ToolDecl)
	if !ok {
		return nil, fmt.Errorf("expected tool declaration, got %T", def)
	}
	return decl, nil
}
