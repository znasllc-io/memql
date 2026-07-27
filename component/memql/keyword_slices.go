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
	"strings"

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
	scan := languageParser.BlankComments(source)
	matches := headerRe.FindAllStringSubmatchIndex(scan, -1)
	if len(matches) == 0 {
		return nil
	}

	var out []KeywordSlice
	for _, m := range matches {
		headerStart := m[0]
		headerEnd := m[1]
		nameStart := m[2]
		nameEnd := m[3]
		name := source[nameStart:nameEnd]

		openIdx := headerEnd - 1 // index of `{`
		closeIdx := findMatchingCloseBraceRune(scan, openIdx)
		if closeIdx < 0 {
			continue
		}

		// Walk backwards for the @-attribute + comment preamble.
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
			if trimmed == "" {
				break
			}
			break
		}

		out = append(out, KeywordSlice{
			Source: source[preambleStart : closeIdx+1],
			Name:   name,
		})
	}
	return out
}
