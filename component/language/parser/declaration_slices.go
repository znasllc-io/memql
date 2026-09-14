package parser

import (
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/dslclause"
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

// predicateDeclHeaderRe matches the header of an edition-2026 BRACE-LESS
// predicate declaration, up to and including its `=`: `spec <Bound> <Name> =`
// or `trait <Name> =` (memql#5364). Group 1 is the keyword, group 2 the name.
var predicateDeclHeaderRe = regexp.MustCompile(`(?m)^[ \t]*(spec|trait)[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?([A-Za-z_][A-Za-z0-9_-]*)[ \t]*=`)

// ExtractPredicateDeclarationSlices returns every top-level edition-2026
// brace-less `spec` or `trait` declaration of the given keyword in source:
//
//	spec agent isAssistant = row => row.role == "assistant"
//	trait isActiveRecord = row => row.active == true
//
// It is a separate entry point from ExtractDeclarationSlices because that
// function's extent rule is "match a header ending in `{`, then find the
// matching `}`", and this form has no braces. Its extent is its EXPRESSION -- the header line plus every
// continuation line dslclause.ClauseExtent folds into it, which is how a spec
// the codemod wrapped at its top-level `&&` stays one declaration -- and the
// slice ends at the expression's last character, trailing comment excluded,
// which is where MemQL Sense's token scan ends the same declaration (the two
// must agree byte for byte, or the construct source hash reads as drifted).
//
// A slicer that required the `{` saw none of these: every spec and trait of a
// migrated tree was silently absent from the loader, the duplicate detector,
// the construct catalog and the authoring bundle splitter at once.
//
// Comment safety and the preamble walk follow ExtractDeclarationSlices' three
// rules. Only top-level headers are emitted, and `==` is never a header's `=`.
func ExtractPredicateDeclarationSlices(source, keyword string) []DeclarationSlice {
	scan := BlankComments(source)
	matches := predicateDeclHeaderRe.FindAllStringSubmatchIndex(scan, -1)
	if len(matches) == 0 {
		return nil
	}
	lines := strings.Split(scan, "\n")
	lineStarts := make([]int, len(lines))
	for i, off := 0, 0; i < len(lines); i++ {
		lineStarts[i] = off
		off += len(lines[i]) + 1
	}

	var out []DeclarationSlice
	for _, m := range matches {
		headerStart, headerEnd := m[0], m[1]
		if scan[m[2]:m[3]] != keyword {
			continue
		}
		if headerEnd < len(scan) && scan[headerEnd] == '=' {
			continue // `==`: a comparison, not a declaration
		}
		if BraceDepthBefore(scan, headerStart) != 0 {
			continue
		}
		first := sort.SearchInts(lineStarts, headerStart+1) - 1
		last := dslclause.ClauseExtent(lines, first)
		end := lineStarts[last] + len(strings.TrimRight(lines[last], " \t\r"))
		preambleStart := PreambleStartOf(source, headerStart)
		out = append(out, DeclarationSlice{
			Source: source[preambleStart:end],
			Name:   source[m[4]:m[5]],
			Start:  preambleStart,
			End:    end,
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
