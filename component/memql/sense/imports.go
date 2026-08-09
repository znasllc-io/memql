package sense

// imports.go answers one question for an editor client: which files does this
// buffer import? It is the language-server half of `memql/imports`
// (memql#3335), and it exists to delete a SECOND PARSER.
//
// The VS Code runtime panel assembles the bundle it session-defines from the
// active file plus any transitively `use`-imported workspace file that
// currently has unsaved edits. To walk that graph it was scanning for
// `use <dotted>.{` with a line regex -- a regex that cannot know what a lexer
// knows. The design's first principle is that there is exactly one parser, and
// the failure mode of the second one is the invisible kind: a missed dirty
// dependency means the run silently executes the SAVED copy of a file the
// developer is looking at.
//
// Two things the regex gets wrong that this does not:
//
//  1. A `use` line inside a /* ... */ BLOCK COMMENT. The lexer skips block
//     comments outright, so a commented-out import produces no tokens at all;
//     a line-anchored regex sees an ordinary import and follows it.
//  2. Line structure generally. The lexer does not care where the newlines
//     are, so a declaration split across lines -- or two on one line -- reads
//     identically to the compiler and to this.
//
// WHY A TOKEN WALK RATHER THAN A FULL PARSE. Diagnose parses the whole file
// through applyRewriteChain, and one half-typed construct anywhere makes that
// fail. The editor asks this question during a run, on a buffer that is
// usually mid-edit, and "your imports vanished because line 200 is
// incomplete" is exactly the invisible failure the request is meant to remove.
// So this walks the LEXER's token stream: the real lexer, with comments and
// string literals already resolved, but tolerant of a body it never inspects.
// That keeps the single-parser guarantee where it matters -- what counts as an
// import is decided by the compiler's own tokenizer -- while surviving the
// buffer states the client actually asks about.
//
// Only Form B (`use <module>.{ names }`) is recognised, because that is the
// only import form the loader accepts: Form A (`use <ns>.<concept>`, with or
// without `as`) is retired and rejected at parse time, so following one would
// mean following something that cannot load.

import "github.com/znasllc-io/memql/component/language/parser"

// Import is one file-top Form-B `use` declaration, as authored.
//
// The wire projection of this type is a FIXED contract shared with the VS Code
// extension (memql#3335); field semantics must not drift without changing both
// sides together.
type Import struct {
	// Path is the dotted MODULE path as written: "cognition.concepts",
	// "common.traits". It names a FILE -- `cognition.shapes` is
	// dsl/cognition/shapes.memql -- not a construct. Resolving it to a path on
	// disk is the client's job: the workspace layout it is walking is the
	// client's, and a language server answering for a document it was never
	// told about would have to guess at it.
	Path string
	// Names is the imported-construct list from the brace group, in authored
	// order. Carried because it is free and a client may want it (a future
	// "which construct came from where" affordance); the bundle walk itself
	// only needs Path -- a file with any unsaved edit goes into the bundle
	// whole, whichever of its constructs are used.
	Names []string
}

// Imports returns every Form-B `use` declaration in source, in declaration
// order, INCLUDING duplicates of the same module path.
//
// Duplicates are kept because this reports what the buffer says rather than a
// deduplicated view of it: two `use` lines naming one module with different
// brace lists are two declarations, and a client that only wants the distinct
// module set can collapse them in one line. Collapsing here would throw away
// the Names of all but the first.
//
// It never returns an error. A buffer that does not lex yields an empty
// result, for the same reason RunnableConstructs does: the editor asks this on
// a mid-edit buffer as the ordinary case, and an error would surface as a
// popup on a document the developer is simply still typing.
//
// The returned slice is always non-nil.
func (s *Service) Imports(source string) []Import {
	out := []Import{}
	lexer := parser.NewLexer(source)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return out
	}
	for i := 0; i < len(tokens); i++ {
		if tokens[i].Type != parser.TokenKeywordUse {
			continue
		}
		imp, next, ok := importAt(tokens, i)
		if !ok {
			// Not a well-formed Form-B declaration. Skipped rather than
			// guessed at: `use` is a reserved keyword, so this is either the
			// retired Form A (which cannot load, so following it would walk to
			// a file the compiler will not read) or a half-typed line the
			// developer is still writing.
			continue
		}
		out = append(out, imp)
		i = next
	}
	return out
}

// importAt matches `use <dotted> . { name, name }` starting at the `use` token
// at tokens[i]. It returns the projected declaration and the index of the
// closing brace, so the caller resumes after the whole declaration rather than
// re-entering it.
//
// The shape it matches is exactly the one parseUseDeclaration accepts, token
// for token. Worth knowing about the lexer here: a dotted path lexes as ONE
// identifier -- `cognition.concepts` is a single token -- because the
// identifier scanner consumes a dot followed by an identifier character. The
// trailing dot before `{` is deliberately NOT consumed by that scanner, which
// is what makes the `.` `{` pair a reliable separator.
func importAt(tokens []parser.Token, i int) (Import, int, bool) {
	j := i + 1
	if j >= len(tokens) || tokens[j].Type != parser.TokenIdentifier {
		return Import{}, 0, false
	}
	path := tokens[j].Literal
	j++
	if j >= len(tokens) || tokens[j].Type != parser.TokenDot {
		return Import{}, 0, false
	}
	j++
	if j >= len(tokens) || tokens[j].Type != parser.TokenBraceOpen {
		return Import{}, 0, false
	}
	j++

	names := []string{}
	for ; j < len(tokens); j++ {
		switch tokens[j].Type {
		case parser.TokenComma, parser.TokenSemicolon:
			continue
		case parser.TokenIdentifier:
			names = append(names, tokens[j].Literal)
			continue
		case parser.TokenBraceClose:
			// An empty brace list is rejected by the parser
			// ("must list at least one imported name"), so it is not a
			// declaration the loader would accept either.
			if len(names) == 0 {
				return Import{}, 0, false
			}
			return Import{Path: path, Names: names}, j, true
		}
		// Anything else inside the braces means this is not the declaration it
		// looked like -- most often a buffer mid-edit where the brace group is
		// not closed yet.
		return Import{}, 0, false
	}
	return Import{}, 0, false
}
