package parser

// edition.go -- the language line and editions (epic memql#5356, D4 and D6 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// Three labels name the language a tree is written in, coarse to fine:
//
//   - LanguageVersion is the language line a tree declares as
//     `memql = "1.0"` in its memql.toml. The engine refuses a tree that
//     declares a newer line than this one. It is also the key later epics hang
//     version-dependent meaning on, the way Go keys a module's semantics on its
//     `go` line: a behaviour that changes between two language lines reads the
//     tree's declared line rather than the engine's.
//   - Edition is the coarse label a tree declares as `edition = "2026"`. It
//     selects the parser front end the tree is read with (FrontEndFor), so two
//     trees on different editions load in one engine.
//   - GrammarVersion (grammar_version.go) is the fine label. It moves on every
//     change to the authored surface inside an edition, and an edition's front
//     end records the grammar version it was last read at.
//
// ONE CORE, THIN FRONT ENDS. An edition never forks the parser. There is one
// lexer, one parser, one AST and one compiler, and a front end is only the
// source-to-source step that brings one edition's surface to that core. The
// current edition's step is the identity, because the core IS the current
// edition. A later edition that changes a spelling registers a front end whose
// Prepare rewrites the old spelling into the new one, and the rest of the
// pipeline never learns which edition a file came from. That is the property
// the record's failure-mode list names: "An edition front end that forks the
// core" is the thing this shape exists to prevent.

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// LanguageVersion is the language line this engine speaks. A tree declares the
// line it was written against in its memql.toml; one newer than this is
// refused, naming both.
const LanguageVersion = "1.0"

// Edition is the edition this engine writes and the newest one it reads.
const Edition = "2026"

// EditorRelease is the first release of MemQL for VS Code that carries
// GrammarVersion (memql#5362; D25 of the design record above).
//
// The extension ships its own language server, so an editor speaks the
// grammar it was packaged with, whatever the cluster it connects to speaks.
// A cluster states this value on the handshake (ServerHello.editor_release)
// so an OLDER editor can name the release to install: the editor has never
// heard of a grammar newer than itself, and only the cluster can say which
// release carries it.
//
// It moves with the grammar. A GrammarVersion bump after this release has
// shipped needs a new extension release, and this constant, the extension's
// `version` and the `memql.grammarVersion` pin in editors/vscode/package.json
// move in the same change -- cmd/memql-lsp/editorparity_test.go refuses the
// change otherwise, and its failure names each edit.
//
// # Why it stayed at 0.5.1 while the extension went to 0.6.0 (memql#5390)
//
// The freeze shipped an extension release that carries no grammar change:
// edition 2026 was frozen, the engine began serving the generated BNF and
// vocabulary, and `make vscode-grammar` regenerated the TextMate grammar and
// the language configuration BYTE-IDENTICAL, because the tables they derive
// from did not move.
//
// So 0.5.1 is still the answer to the question this constant asks -- "which is
// the first release that carries this grammar" -- and 0.6.0 is not. Raising it
// would tell an older editor to install 0.6.0 to reach a grammar 0.5.1 already
// has, which is a true-sounding instruction that is wrong about the only fact
// it exists to state.
//
// It is therefore NORMAL for this constant to lag the extension's `version`,
// and the parity gate is built for that: it refuses this constant being NEWER
// than the extension, never older. If you are here because you bumped the
// extension and wondered whether this should follow, the question to ask is
// whether GrammarVersion moved. If it did not, leave this alone.
const EditorRelease = "0.5.1"

// FrontEnd is how one edition's source reaches the core parser.
type FrontEnd struct {
	// Edition is the label a tree declares as `edition = "<Edition>"`.
	Edition string
	// GrammarVersion is the grammar the edition was last read at. The current
	// edition's is GrammarVersion itself; a closed edition's stays where it
	// was when the next one opened.
	GrammarVersion string
	// Prepare rewrites one file of this edition into the core grammar's input.
	// It runs before the struct-form rewriter, so it sees exactly what the
	// author wrote. It must be pure: no I/O, no state between calls.
	Prepare func(src string) (string, error)
}

var (
	frontEndsMu sync.RWMutex
	frontEnds   = map[string]FrontEnd{
		Edition: {
			Edition:        Edition,
			GrammarVersion: GrammarVersion,
			Prepare:        func(src string) (string, error) { return src, nil },
		},
	}
)

// FrontEndFor returns the front end a tree declaring `edition` is read with.
// An edition this engine does not have is an error naming the ones it does,
// because the author's next move is to pick one of them.
func FrontEndFor(edition string) (FrontEnd, error) {
	frontEndsMu.RLock()
	defer frontEndsMu.RUnlock()
	if fe, ok := frontEnds[edition]; ok {
		return fe, nil
	}
	return FrontEnd{}, fmt.Errorf("edition %q is not one this engine reads (it reads: %s)",
		edition, strings.Join(editionsLocked(), ", "))
}

// Editions lists the editions this engine reads, oldest first. Editions are
// years, so the string order is the chronological order.
func Editions() []string {
	frontEndsMu.RLock()
	defer frontEndsMu.RUnlock()
	return editionsLocked()
}

func editionsLocked() []string {
	out := make([]string, 0, len(frontEnds))
	for e := range frontEnds {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// RegisterEdition adds the front end for one edition and returns the function
// that removes it again.
//
// The production caller is a later edition's front end, registered from an
// init() in this package. The returned remover exists for tests, which load a
// synthetic second edition to prove two editions share one engine
// (TestTwoEditionsLoadInOneEngine in component/memql) and must leave the table
// as they found it.
//
// Registering an edition twice, or with a nil Prepare, panics: both are
// programming errors in an init() path, and a silent overwrite would let two
// front ends disagree about one edition depending on registration order.
func RegisterEdition(fe FrontEnd) (unregister func()) {
	if fe.Edition == "" || fe.Prepare == nil {
		panic("parser.RegisterEdition: an edition needs a name and a Prepare step")
	}
	frontEndsMu.Lock()
	defer frontEndsMu.Unlock()
	if _, dup := frontEnds[fe.Edition]; dup {
		panic(fmt.Sprintf("parser.RegisterEdition: edition %q is already registered", fe.Edition))
	}
	frontEnds[fe.Edition] = fe
	return func() {
		frontEndsMu.Lock()
		defer frontEndsMu.Unlock()
		if fe.Edition != Edition {
			delete(frontEnds, fe.Edition)
		}
	}
}
