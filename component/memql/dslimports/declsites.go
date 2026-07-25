package dslimports

// declsites.go answers "where is this construct declared" with a source
// POSITION, not just a file. It is the resolution half of editor
// go-to-definition (memql#2754).
//
// The AST carries no positions -- and is parsed from lowered source anyway --
// so the file is resolved from the declaration index (the same index the
// referential-integrity lanes use) and the line/column is recovered by
// re-lexing the raw source of that one file. Raw source is what the editor
// actually shows, so a position recovered from it lands where the author
// looks.

import (
	"sort"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// DeclSite is one declaration of a construct: the tree-relative file that
// declares it, the construct kind, and the 1-based position of the declared
// NAME token. Kind is a plain string because the index yields whatever
// declNameAndKind emits (16 distinct kinds today), not a closed enum.
type DeclSite struct {
	File   string
	Name   string
	Kind   string
	Line   int
	Column int
}

// DeclarationSites returns every declaration of name across the tree, sorted
// by file for a stable order.
//
// A name can legitimately be declared more than once -- 46 bare names collide
// across the engine tree -- so this returns ALL of them and leaves the choice
// to the caller, which knows the referencing file's domain. Returning a single
// "best" site here would silently pick one and send the editor to the wrong
// file.
//
// A site whose position cannot be recovered (unreadable or unlexable source)
// still comes back, anchored at the start of its file: the file is the useful
// part of the answer and is known independently of the scan.
func (ix *Index) DeclarationSites(name string) []DeclSite {
	if ix == nil || name == "" {
		return nil
	}
	kindByFile := make(map[string]string)
	files := make([]string, 0, 2)
	for file, decls := range ix.idx.byFile {
		if kind, ok := decls[name]; ok {
			files = append(files, file)
			kindByFile[file] = kind
		}
	}
	if len(files) == 0 {
		return nil
	}
	sort.Strings(files)

	sites := make([]DeclSite, 0, len(files))
	for _, file := range files {
		site := DeclSite{File: file, Name: name, Kind: kindByFile[file], Line: 1, Column: 1}
		if line, col, ok := declNamePosition(ix.tree.readSource(file), name); ok {
			site.Line, site.Column = line, col
		}
		sites = append(sites, site)
	}
	return sites
}

// declNamePosition finds the position of the NAME token in the declaration of
// name within source.
//
// A declaration header is a top-level (brace depth 0) identifier run that
// leads its line, and the declared name is the run's LAST identifier --
// `concept candidate {` names candidate, `shape action actionFull {` names
// actionFull. That is the same reading resolveEnclosingConstruct uses. `use`
// lines cannot be confused for headers because the lexer emits a keyword token
// for `use`, and annotations start with `@`.
func declNamePosition(source, name string) (line, column int, ok bool) {
	if source == "" || name == "" {
		return 0, 0, false
	}
	tokens, err := languageParser.NewLexer(source).Tokenize()
	if err != nil {
		return 0, 0, false
	}
	depth := 0
	for i, t := range tokens {
		switch t.Type {
		case languageParser.TokenBraceOpen:
			depth++
			continue
		case languageParser.TokenBraceClose:
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth != 0 || t.Type != languageParser.TokenIdentifier {
			continue
		}
		// A declaration header leads its line.
		if i > 0 && tokens[i-1].Line == t.Line {
			continue
		}
		last := t
		for j := i + 1; j < len(tokens) && tokens[j].Type == languageParser.TokenIdentifier; j++ {
			last = tokens[j]
		}
		if last.Literal == name && last.Pos != t.Pos {
			return last.Line, last.Column, true
		}
	}
	return 0, 0, false
}
