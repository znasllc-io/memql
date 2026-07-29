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

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

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
// String + line-comment aware brace balancing.
func ExtractKeywordSlices(source, keyword string) []KeywordSlice {
	headerRe := regexp.MustCompile(
		`(?m)^[ \t]*` + regexp.QuoteMeta(keyword) +
			`[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?([A-Za-z_][A-Za-z0-9_-]*)[ \t]*\{`,
	)
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
	slices := languageParser.ExtractDeclarationSlices(source, headerRe)
	if len(slices) == 0 {
		return nil
	}

	out := make([]KeywordSlice, 0, len(slices))
	for _, s := range slices {
		out = append(out, KeywordSlice{Source: s.Source, Name: s.Name})
	}
	return out
}
