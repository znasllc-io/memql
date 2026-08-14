package parser

import (
	"regexp"
	"strings"
)

// declaration_slices.go -- the ONE comment-safe declaration slicer (memql#2896).
//
// Slicing a .memql source into independently-parseable top-level declarations
// had been reimplemented six times, and every copy that scanned RAW source
// carried the same two bugs: a block-commented declaration got extracted and
// loaded, and a `}` inside a block comment truncated the slice mid-body.
// #2868 fixed three of them in component/memql; #2872 observed it "should
// probably be a shared helper rather than five copies"; #2896 measured the
// remaining ones in component/actions and asked for the extraction.
//
// # Why it lives HERE and not in component/memql
//
// component/memql imports component/actions (authoring_sandbox.go), so
// component/actions cannot import component/memql -- the "kept local so this
// package stays a leaf" comments in component/actions are describing a real
// import cycle, not a preference. This package is already imported by both
// sides and already hosts BlankComments, so it is the one address where a
// single implementation is reachable from every call site.
//
// # The three rules, and why each one is where it is
//
//  1. Header detection and brace matching run on the COMMENT-BLANKED view.
//     BlankComments preserves byte offsets, so indices map back to the original.
//     This is what makes a commented-out declaration invisible and stops a
//     brace inside a comment from unbalancing the walk.
//
//  2. The emitted slice is cut from the ORIGINAL source. Authored comments are
//     part of the declaration's text and must survive into the slice.
//
//  3. The @-attribute / comment preamble walk stays on the ORIGINAL source, and
//     this one is a trap rather than a preference. BlankComments blanks `//`
//     lines too, and the preamble walk treats a `//` line as part of the
//     preamble -- so walking the blanked view stops at such a line and strips
//     every @-attribute above it, silently dropping a declaration's
//     annotations. Pinned by TestPreambleSurvivesALineComment in
//     component/memql and TestActionSlicePreambleSurvivesALineComment in
//     component/actions.
//
// Only brace-depth-0 headers are emitted: a header nested inside another
// construct's braces is not a standalone declaration. That guard came from
// component/memql/authoring_bundle_slices.go, the most complete of the six, and
// none of the others had it.

// DeclarationSlice is one top-level declaration lifted out of a source file,
// parseable on its own.
type DeclarationSlice struct {
	Source string // slice text (preamble + body), cut from the original source
	Name   string // declaration name, from capture group 1 of the header regexp
	Start  int    // byte offset in the original source where Source begins
	End    int    // byte offset one past the closing brace
}

// ExtractDeclarationSlices returns every top-level declaration in source whose
// header matches headerRe, comment-safely.
//
// headerRe MUST capture the declaration name in group 1, and its match MUST end
// on the declaration's opening `{` -- both hold for the `^[ \t]*<keyword>[ \t]+
// (NAME)[ \t]*\{` family every call site uses.
//
// A header whose braces never close, or which sits inside another construct's
// braces, is skipped rather than guessed at.
func ExtractDeclarationSlices(source string, headerRe *regexp.Regexp) []DeclarationSlice {
	scan := BlankComments(source)
	matches := headerRe.FindAllStringSubmatchIndex(scan, -1)
	if len(matches) == 0 {
		return nil
	}

	var out []DeclarationSlice
	for _, m := range matches {
		headerStart, headerEnd := m[0], m[1]

		// Only top-level declarations. Depth is counted on the blanked view so
		// braces inside comments do not perturb it.
		if BraceDepthBefore(scan, headerStart) != 0 {
			continue
		}

		// The opening `{` is the last byte of the header match.
		closeIdx := MatchingCloseBrace(scan, headerEnd-1)
		if closeIdx < 0 {
			continue
		}

		preambleStart := PreambleStartOf(source, headerStart)

		out = append(out, DeclarationSlice{
			Source: source[preambleStart : closeIdx+1],
			Name:   source[m[2]:m[3]],
			Start:  preambleStart,
			End:    closeIdx + 1,
		})
	}
	return out
}

// ExtractTerseAutomationSlices returns every terse single-step automation in
// source, as AUTHORED:
//
//	automation NAME @trigger(schedule="0 0 2 * * *") => logic targetLogic
//
// It is a separate entry point from ExtractDeclarationSlices because that
// function's whole extent rule is "match a header ending in `{`, then find the
// matching `}`", and this form has no braces at all: the declaration ends with
// its line. Passing it a brace-less header regexp would not narrow the result,
// it would return nothing.
//
// # Why the catalog needs its own extractor rather than the loader's
//
// ExtractAutomationSlices (component/memql) already finds these, but it finds
// them by LOWERING the terse form to longhand first and then slicing the
// lowered text -- and it prepends the file's `use` declarations so the slice
// re-parses standalone. That is exactly right for a loader, whose job is to
// produce something parseable, and exactly wrong for a content hash, whose job
// is to be a function of what the author actually wrote. Hashing the lowered
// text would make the engine and the language server disagree about all ten of
// these automations (memql#3758), and reformatting the lowering would silently
// re-hash every one of them.
//
// Comment safety and the preamble walk follow the same three rules as
// ExtractDeclarationSlices: detect on the comment-blanked view so a
// commented-out declaration stays invisible, cut from the original so authored
// comments survive into the slice, and walk the preamble on the original.
func ExtractTerseAutomationSlices(source string) []DeclarationSlice {
	scan := BlankComments(source)
	matches := terseAutomationHeader.FindAllStringSubmatchIndex(scan, -1)
	if len(matches) == 0 {
		return nil
	}

	var out []DeclarationSlice
	for _, m := range matches {
		// The header regexp is `(?m)^...$`, so the match spans the whole line
		// from its indentation to just before the newline -- which IS the
		// declaration's extent for this form.
		lineStart, lineEnd := m[0], m[1]
		if BraceDepthBefore(scan, lineStart) != 0 {
			continue
		}
		preambleStart := PreambleStartOf(source, lineStart)
		// Capture group 2 is the automation's name.
		out = append(out, DeclarationSlice{
			Source: source[preambleStart:lineEnd],
			Name:   source[m[4]:m[5]],
			Start:  preambleStart,
			End:    lineEnd,
		})
	}
	return out
}

// PreambleStartOf walks backwards from a header over contiguous leading
// @-attribute and `//` comment lines and returns the offset the slice should
// start at. headerStart is a BYTE offset into source, and the return value is
// one too.
//
// Deliberately takes the ORIGINAL source, never the blanked view -- see rule 3
// in the file comment.
//
// # Why this is exported (memql#3758)
//
// It answers "where does this declaration's TEXT begin", and that answer has to
// be the same on both sides of the construct source hash -- the engine, which
// slices through ExtractDeclarationSlices below, and MemQL Sense, which locates
// constructs by walking the token stream instead. The hash function itself was
// already shared (sense.ConstructSourceHash); the SLICE it hashes was not, and
// that is the divergence that actually bites: two sides can agree perfectly
// about SHA-256 and still disagree about every construct in the tree because
// one of them starts the slice a line lower.
//
// That is not hypothetical. Sense's token scan starts a declaration at the
// first `@` of its annotation block, because comments are not tokens -- so it
// silently dropped the leading `///` doc comment that 932 constructs in dsl/
// carry above their annotations. The doc comment IS part of the declaration
// (it is the description the catalog serves, and sense.ConstructSourceHash
// deliberately keeps `///` while stripping every other comment), so those 932
// constructs hashed differently on the two sides -- which is precisely the
// "every construct reads as drifted, and it looks like a broken cluster rather
// than a hash bug" failure the issue exists to prevent.
//
// A caller in a different offset space (Sense counts RUNES, because the lexer
// scans a []rune) converts at the boundary rather than reimplementing the walk.
func PreambleStartOf(source string, headerStart int) int {
	preambleStart := headerStart
	for k := headerStart - 1; k >= 0; k-- {
		lineStart := strings.LastIndexByte(source[:k], '\n') + 1
		line := strings.TrimRight(source[lineStart:k+1], "\r\n")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "@") || strings.HasPrefix(trimmed, "//") {
			preambleStart = lineStart
			k = lineStart - 1
			continue
		}
		break
	}
	return preambleStart
}

// MatchingCloseBrace returns the index of the `}` matching the `{` at openIdx,
// or -1 if openIdx is not a `{` or the braces never balance.
//
// It is NOT block-comment aware, by design: pass the comment-blanked view (see
// BlankComments), which is what makes it block-comment safe without a second
// comment state machine here.
//
// This is a thin export over matchBraceStrAware rather than its own walk. An
// earlier version of this function WAS its own walk, and that reintroduced a
// hole the package had already closed: it tracked double-quoted literals only,
// so a brace inside a BACKTICK literal ran the depth count away and returned
// -1 -- silently dropping that declaration and every one below it, on all five
// load paths at once. matchBraceStrAware routes both quote forms through
// skipStringLiteral and does not have that hole.
//
// Pinned by TestMatchingCloseBraceHandlesBacktickLiterals. Do not re-inline a
// second walk here: one matcher, one set of escape rules, fixed in one place.
func MatchingCloseBrace(scan string, openIdx int) int {
	return matchBraceStrAware(scan, openIdx)
}

// BraceDepthBefore returns the brace nesting depth immediately before pos.
// Zero means pos is at file top level.
//
// Same contract as MatchingCloseBrace: pass the comment-blanked view.
//
// Shares skipStringLiteral with MatchingCloseBrace for the same reason -- a
// brace inside a backtick literal must not move the depth. The two must agree
// about what counts as a brace, or the top-level guard and the brace match
// disagree about the same source.
func BraceDepthBefore(scan string, pos int) int {
	if pos > len(scan) {
		pos = len(scan)
	}
	depth := 0
	for i := 0; i < pos; i++ {
		switch scan[i] {
		case '"', '`':
			i = skipStringLiteral(scan, i)
		case '{':
			depth++
		case '}':
			depth--
		}
	}
	return depth
}
