package parser

import (
	"fmt"

	"github.com/znasllc-io/memql/component/language/ast"
)

// ParseSeedDecl tokenises a single `seed NAME { ... }` slice (with
// its leading attribute set + any preserved file-top `use ...`
// preamble) and returns the typed *ast.SeedDecl. The caller --
// typically the unified seed loader -- pre-pends the file-top
// preamble onto each slice so signature-bound concept names resolve
// against the same Form B imports the file declares once at the top.
//
// Returns an error when the slice's syntax is malformed or doesn't
// produce a seed node. memql#335 (sub-epic #329 / Stage 1C of #310).
func ParseSeedDecl(source string) (*ast.SeedDecl, error) {
	file, err := ParseFile(source)
	if err != nil {
		return nil, err
	}
	// The slice's preamble carries `use` declarations which the
	// langparser emits as UseDeclaration nodes alongside the seed
	// definition; pluck the seed out and ignore the rest.
	for _, def := range file.Definitions {
		if decl, ok := def.(*ast.SeedDecl); ok {
			return decl, nil
		}
	}
	return nil, fmt.Errorf("no seed declaration found in source")
}
