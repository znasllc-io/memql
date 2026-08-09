package main

// imports.go serves `memql/imports`, the custom LSP request that tells a client
// which files a .memql buffer imports (memql#3335).
//
// It exists to DELETE A SECOND PARSER. The VS Code runtime panel assembles the
// bundle it session-defines from the active file plus any transitively
// `use`-imported workspace file with unsaved edits, and to walk that graph it
// was scanning for `use <dotted>.{` with a line regex. The runtime-panel
// design's first principle is that there is exactly one parser; a regex cannot
// know what the lexer knows (a `use` line inside a block comment is not an
// import), and the failure mode is the invisible kind -- a missed dirty
// dependency means the run silently executes the SAVED copy of a file the
// developer is looking at.
//
// The analysis lives in Sense (component/memql/sense/imports.go) next to the
// real lexer; this file is a transport adapter -- JSON shape and nothing more.
//
// WHY THE CLIENT MAY SUPPLY THE TEXT. `memql/runnableConstructs` answers only
// for OPEN documents, which is right for it: a CodeLens exists only where
// there is a visible buffer. An import walk is different -- it traverses files
// the editor has never opened, because a CLEAN file is a legitimate route to a
// DIRTY one. The client already holds each file's current content (it prefers
// an open document's live text and falls back to disk, which is what makes the
// dirty/clean distinction it is walking for possible at all), so it passes
// that text in rather than asking the server to re-derive a file view it does
// not have. The server still owns what counts as an import; the client only
// says which bytes to look at.
//
// The wire shape below is a FIXED contract shared with the TypeScript
// consumer. Field names and the "empty array, never null" rule on `imports`
// and `names` are load-bearing; changing either means changing both sides in
// the same commit.

import (
	protocol "github.com/tliron/glsp/protocol_3_16"
)

// methodImports is the custom request's method name.
const methodImports = "memql/imports"

// capabilityImports is the key the server advertises under
// `capabilities.experimental` so a client can feature-detect the request
// instead of calling it blind and handling MethodNotFound. A client that does
// not see it falls back to its own behaviour -- which, for the runtime panel,
// means running against the saved copy of a dirty dependency rather than not
// running at all.
const capabilityImports = "memqlImports"

type importsParams struct {
	TextDocument protocol.TextDocumentIdentifier `json:"textDocument"`
	// Text, when present, is what gets analysed -- INSTEAD of the server's copy
	// of the document.
	//
	// A pointer so "absent" and "empty string" stay distinguishable. They mean
	// genuinely different things here: absent is "use your copy", while an
	// explicit "" is a client saying this file is empty, and an empty file
	// imports nothing. Collapsing them would make a cleared buffer silently
	// report its last-known imports.
	Text *string `json:"text,omitempty"`
}

type importsResult struct {
	Imports []importEntry `json:"imports"`
}

type importEntry struct {
	// Path is the dotted MODULE path as written -- "cognition.concepts". It
	// names a FILE (dsl/cognition/concepts.memql), not a construct. Resolving
	// it onto disk is the client's job: the workspace layout being walked is
	// the client's, and the brace list is what names constructs.
	Path string `json:"path"`
	// Names is the imported-construct list, in authored order. Always non-nil.
	Names []string `json:"names"`
}

// imports answers the request for one document.
//
// It never fails. An unknown URI with no supplied text, a Sense service that
// is not built yet, and a buffer that does not lex all produce the same empty
// result -- consistent with `memql/runnableConstructs`, and for the same
// reason: an error here would surface to the user as a popup during an
// ordinary run.
//
// An empty result is not distinguishable from "this file has no imports", and
// that is fine for the caller: both mean the walk has nowhere further to go
// down this branch, and the bundle simply omits a dependency it could not
// establish -- the pre-#3335 behaviour of a mis-read line, and visible (the
// sandbox compiles what it got and says so) rather than corrupting.
func (s *server) imports(params importsParams) importsResult {
	// Allocated up front so the field marshals as [] and never as null: the
	// TypeScript consumer iterates it directly.
	out := importsResult{Imports: []importEntry{}}

	text, ok := s.importsSource(params)
	if !ok {
		return out
	}
	svc := s.getSense()
	if svc == nil {
		return out
	}

	for _, imp := range svc.Imports(text) {
		names := imp.Names
		if names == nil {
			names = []string{}
		}
		out.Imports = append(out.Imports, importEntry{Path: imp.Path, Names: names})
	}
	return out
}

// importsSource picks the bytes to analyse: the client's explicit text when it
// supplied any, otherwise the server's copy of the open document.
func (s *server) importsSource(params importsParams) (string, bool) {
	if params.Text != nil {
		return *params.Text, true
	}
	return s.docs.get(params.TextDocument.URI)
}
