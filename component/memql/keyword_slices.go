package memql

// keyword_slices.go provides generic per-kind slice extraction from
// consolidated .memql source files. The function-slice extractor in
// function_slices.go covers query / mutation / spec / logic /
// automation / and procedural-form `func (Kind) NAME(...)` blocks;
// this file covers the struct-form declarations that have their own
// dedicated parsers: shape, provider, prompt, tool, builtin, policy.
//
// Each kind's unified loader (unified_shapes_loader.go, etc.) calls
// ExtractKeywordSlices with the kind's keyword and feeds the
// resulting slices through the kind's existing parseXMemQL.

import (
	"regexp"
	"sort"
	"sync"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// keywordHeaderCache memoizes the per-keyword header pattern. The pattern
// depends on nothing but the keyword, and there are a dozen keywords in the
// language, so compiling one per call is pure waste -- which stopped being
// theoretical once the construct catalog (memql#3749) began slicing every file
// in the tree for every kind on a single request.
var keywordHeaderCache sync.Map // keyword string -> *regexp.Regexp

// keywordHeaderRegexp returns the compiled header pattern for one keyword.
func keywordHeaderRegexp(keyword string) *regexp.Regexp {
	if cached, ok := keywordHeaderCache.Load(keyword); ok {
		return cached.(*regexp.Regexp)
	}
	re := regexp.MustCompile(
		`(?m)^[ \t]*` + regexp.QuoteMeta(keyword) +
			`[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?([A-Za-z_][A-Za-z0-9_-]*)[ \t]*\{`,
	)
	keywordHeaderCache.Store(keyword, re)
	return re
}

// KeywordSlice is one extracted declaration of a given keyword
// (shape / provider / prompt / tool / builtin / policy).
type KeywordSlice struct {
	Source string // slice text (preamble + body), parseable in isolation
	Name   string // declaration name from the header
}

// ExtractKeywordSlices scans `source` for top-level declarations of
// the form `<keyword> NAME { ... }` at column 0 (with optional
// leading whitespace) and returns each as a self-contained slice.
// Also accepts the canonical `<keyword> CONCEPT NAME { ... }` two-
// identifier signature used by seeds / queries / mutations under
// the Form B import model; in that case NAME is the trailing
// identifier (the slice name).
//
// The NAME identifier accepts hyphens (`-`) as an internal rune so
// that kebab-case seed names like `graphic-designer` materialize
// (memql#180). The slicer is intentionally permissive about the
// name shape -- per-kind parsers downstream still enforce their
// own naming rules. The optional binder-concept identifier stays
// Go-style (no hyphens); concept names are by convention camelCase
// across the catalog.
//
// Slice extent: preamble of @-attribute and comment lines walking
// up from the header, through the matching close-brace below it.
// String + line-comment aware brace balancing. An edition-2026
// brace-less spec or trait ends with its expression instead (see
// constructDeclarationSlices).
func ExtractKeywordSlices(source, keyword string) []KeywordSlice {
	// Detect headers and balance braces on a comment-BLANKED view, so a
	// declaration existing only inside a `/* ... */` block is never extracted as
	// a live construct (memql#2868). Offsets are preserved, so the emitted slice
	// is still cut from the ORIGINAL and authored comments survive in it; the
	// preamble walk below likewise stays on the original.
	//
	// Same split ExtractFunctionSlices has made since #1074 and
	// ExtractAutomationSlices since #2866. The brace walk uses the blanked view
	// too, not just the header scan -- a `}` inside a comment would otherwise
	// close a slice early and emit a truncated construct. Only BLOCK comments
	// were affected: the header pattern is anchored at `^[ \t]*<keyword>`, so a
	// `// concept x {` line never matched.
	// One shared implementation across every offset-based slicer (memql#2896);
	// the blanked-scan / original-cut split described above lives there now.
	slices := constructDeclarationSlices(source, keyword)
	if len(slices) == 0 {
		return nil
	}

	out := make([]KeywordSlice, 0, len(slices))
	for _, s := range slices {
		out = append(out, KeywordSlice{Source: s.Source, Name: s.Name})
	}
	return out
}

// constructDeclarationSlices returns every top-level declaration of one
// keyword in source, in source order: the braced `<keyword> [CONCEPT] NAME {
// ... }` form, and -- for `spec` and `trait` -- the brace-less `spec <Bound>
// <Name> = row => ...` / `trait <Name> = row => ...` (epic memql#5363), whose
// extent is its expression rather than a brace pair.
//
// Every slicing site asks this rather than the brace slicer alone. A braced
// header regexp sees no brace-less declaration at all, so each site that used
// one -- the spec loader, the duplicate detector, the construct catalog, the
// authoring bundle splitter -- lost every spec and trait in silence: a
// construct that is not sliced is not a skip anything reports.
//
// A spec or trait still in the retired braced form (`{ return ... }`) is
// sliced too, deliberately. Its parse refuses it with the retired-form
// message and the replacement; a slicer that dropped it would turn that
// refusal into a construct that silently does not exist.
func constructDeclarationSlices(source, keyword string) []languageParser.DeclarationSlice {
	slices := languageParser.ExtractDeclarationSlices(source, keywordHeaderRegexp(keyword))
	if keyword != "spec" && keyword != "trait" {
		return slices
	}
	braceLess := languageParser.ExtractPredicateDeclarationSlices(source, keyword)
	if len(braceLess) == 0 {
		return slices
	}
	slices = append(slices, braceLess...)
	sort.SliceStable(slices, func(i, j int) bool { return slices[i].Start < slices[j].Start })
	return slices
}
