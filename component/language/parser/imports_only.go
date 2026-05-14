package parser

// Imports-only fallback parser. The full parser only accepts files
// with `func` or `concept` declarations at the top level; constructs
// like shape, provider, builtin, prompt, tool have dedicated runtime
// parsers (component/memql/shape_parser.go, provider_loader.go, etc.)
// and are not understood by the generic parser.
//
// For the new import-model validator (docs/dsl-import-model-refactor.md),
// we still want to validate the `import (...)` block of every .memql
// file -- across all construct kinds -- so the import graph is
// complete and cycle/collision detection covers everything.
//
// ExtractImports parses only the file-top `import (...)` block(s)
// and stops. It tolerates everything else: unknown keywords, malformed
// bodies, comments, attributes, anything. The function returns a
// *File whose Imports slice is populated and whose Definitions slice
// is empty. Callers must treat the returned File as "imports valid,
// body unverified by this layer" -- downstream validators (the
// dedicated runtime parsers) cover the body separately.

// ExtractImports tokenises the source and returns a *File carrying
// just the parsed `import (...)` block(s). Returns an empty File
// (no Imports, no Definitions) when the file has no imports.
// Returns an error only for lexer failures or malformed import
// blocks; everything outside the import block is silently
// tolerated.
func ExtractImports(source string) (*File, error) {
	lexer := NewLexer(source)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return nil, err
	}

	p := NewParser(tokens)
	file := &File{Definitions: []Node{}}

	// Walk the token stream looking for `import` keywords at file
	// top. We stop at the first non-import, non-`use`, non-attribute
	// token -- that's the body of the file, which we leave alone.
	//
	// Tolerance: stray semicolons, commas, line-break artifacts of
	// the lexer all get skipped.
	for !p.check(TokenEOF) {
		switch {
		case p.check(TokenKeywordImport):
			entries, err := p.parseImportBlock()
			if err != nil {
				return nil, err
			}
			file.Imports = append(file.Imports, entries...)
		case p.check(TokenKeywordUse):
			// Legacy `use` declaration -- consume but don't store on
			// the returned File. We only care about imports here.
			if _, err := p.parseUseDeclaration(); err != nil {
				return nil, err
			}
		default:
			// Hit the body. Stop scanning.
			return file, nil
		}
	}

	return file, nil
}
