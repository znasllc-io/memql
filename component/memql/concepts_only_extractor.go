package memql

// concepts_only_extractor.go provides a parse path that pulls
// concept declarations out of mixed-content .memql files without
// running the struct-form rewriters.
//
// Why this exists: the new domain-first tree consolidates queries,
// mutations, shapes, specs, automations, and concepts into single
// files per entity. The struct-form rewriters
// (mutation_rewrite, query_rewrite, etc.) currently assume one
// concept per file and pick the wrong concept binding when they
// see multiple. ParseFileSource therefore errors out on most
// consolidated files, and the unified loader's ConceptDecl walk
// comes up empty.
//
// This extractor solves the immediate need (concept registration
// from the new tree) by scanning the source text for concept block
// boundaries, slicing each block + its preamble, and parsing each
// slice through the bare parser (which handles concept syntax
// natively and doesn't run any rewriters).

import (
	"fmt"
	"regexp"
	"strings"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// CodeConceptUnparsed is the stable rule id of a concept declaration that
// does not parse, when the parse error carries no rule id of its own.
const CodeConceptUnparsed = "concept_unparsed"

// ConceptParseError is one concept declaration that did not parse: the name
// its header declares, the line of its `concept` keyword, and the parser's
// refusal, positioned in the file rather than in the slice.
//
// It used to be dropped here with no error at all (memql#5426). The unified
// loader built what parsed and reported nothing about what did not, so strict
// boot -- whose contract is that a malformed construct refuses boot -- booted
// without the concept, and every query, mutation and shape bound to it failed
// at run time naming an import rather than the concept.
type ConceptParseError struct {
	Name string
	Line int
	Err  error
}

// Error names the position and the concept, then the parser's refusal, with a
// rule id last in brackets: the parser's own when it carries one (a retired
// spelling's rule), else concept_unparsed.
func (e *ConceptParseError) Error() string {
	msg := fmt.Sprintf("line %d: concept %q does not parse, so it is not registered: %v", e.Line, e.Name, e.Err)
	if baseloader.RuleCode(e.Err) == "" {
		msg += " [" + CodeConceptUnparsed + "]"
	}
	return msg
}

// Unwrap exposes the parser's refusal.
func (e *ConceptParseError) Unwrap() error { return e.Err }

// RuleCode is the refusal's stable rule id (baseloader.CodedRefusal).
func (e *ConceptParseError) RuleCode() string {
	if code := baseloader.RuleCode(e.Err); code != "" {
		return code
	}
	return CodeConceptUnparsed
}

// ExtractConceptDecls scans `source` for `concept <name> { ... }`
// blocks (with their preceding @-attribute preamble) and returns
// each parsed ConceptDecl, and every block that did not parse.
// Everything else in the file (queries, mutations, shapes, specs,
// etc.) is ignored.
//
// A block that does not parse is not in the declarations, and IS in the
// errors: a caller that registers concepts must refuse on one (strict boot),
// and a caller that only wants what parsed must say so by ignoring them in
// its own code, where a reviewer can see it.
func ExtractConceptDecls(source string) ([]*languageAst.ConceptDecl, []*ConceptParseError) {
	var out []*languageAst.ConceptDecl
	var failed []*ConceptParseError
	for _, slice := range conceptSlices(source) {
		file, err := languageParser.ParseFile(slice.Source)
		if err != nil {
			failed = append(failed, conceptParseError(source, slice, err))
			continue
		}
		for _, def := range file.Definitions {
			if decl, ok := def.(*languageAst.ConceptDecl); ok {
				out = append(out, decl)
			}
		}
	}
	return out, failed
}

// conceptParseError places a slice's parse failure in its file: the line of
// the slice's `concept` keyword, and the parser's refusal re-read with the
// slice anchored at its first line, so the error's own line and column are
// the file's rather than the slice's.
func conceptParseError(source string, slice languageParser.DeclarationSlice, err error) *ConceptParseError {
	first := 1 + strings.Count(source[:slice.Start], "\n")
	line := first
	if loc := conceptHeaderRe.FindStringIndex(languageParser.BlankComments(slice.Source)); loc != nil {
		line = first + strings.Count(slice.Source[:loc[0]], "\n")
	}
	if _, anchored := languageParser.ParseFile(languageParser.AnchorSource(slice.Source, first)); anchored != nil {
		err = anchored
	}
	return &ConceptParseError{Name: slice.Name, Line: line, Err: err}
}

// conceptHeaderRe is a top-level concept declaration's header: `concept`
// at column 0, its name (group 1), and the opening brace on the same line --
// the shape conceptSlices has always required.
var conceptHeaderRe = regexp.MustCompile(`(?m)^concept[ \t]+([A-Za-z_][A-Za-z0-9_-]*)[ \t]*\{`)

// conceptSlices returns each top-level concept declaration's preamble + body
// as a self-contained slice ready for parser.ParseFile, with its offset and
// the name its header declares.
//
// It is the shared comment- and string-safe declaration slicer
// (languageParser.ExtractDeclarationSlices, memql#2896) over the concept
// header. This used to be a line-oriented slicer of its own, which balanced
// braces on a comment-blanked view but not a string-blanked one, so a `}`
// inside a string literal -- `@description("a } brace")` -- closed the slice
// early and the concept was silently dropped (the KNOWN LIMIT the memql#2868
// review recorded). Once a slice that fails to parse became a load error
// (memql#5426), that limit would have refused a valid concept instead, so the
// slicer is the shared one now: the preamble walk (@-attribute and `//`
// lines, a blank line ending the run) is the one it always had, header
// detection and brace balance run on the comment-blanked view, and a brace
// inside a string of either quote form does not move the depth.
func conceptSlices(source string) []languageParser.DeclarationSlice {
	return languageParser.ExtractDeclarationSlices(source, conceptHeaderRe)
}
