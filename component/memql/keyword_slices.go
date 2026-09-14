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
	"strings"
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
// String + line-comment aware brace balancing.
func ExtractKeywordSlices(source, keyword string) []KeywordSlice {
	headerRe := keywordHeaderRegexp(keyword)
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
	if keyword == "spec" || keyword == "trait" {
		slices = append(slices, extractLambdaDeclarationSlices(source, keyword)...)
		sort.SliceStable(slices, func(i, j int) bool { return slices[i].Start < slices[j].Start })
	}
	if len(slices) == 0 {
		return nil
	}

	out := make([]KeywordSlice, 0, len(slices))
	for _, s := range slices {
		out = append(out, KeywordSlice{Source: s.Source, Name: s.Name})
	}
	return out
}

// lambdaHeaderCache memoizes the brace-less header pattern per keyword.
var lambdaHeaderCache sync.Map // keyword string -> *regexp.Regexp

// lambdaDeclarationHeaderRegexp matches the header of an edition-2026 spec or
// trait (memql#5366): `spec <bound> <name> =` / `trait <name> =`, the `=` NOT
// followed by `=` or `>` -- so neither `==` nor `=>` can read as one.
func lambdaDeclarationHeaderRegexp(keyword string) *regexp.Regexp {
	if cached, ok := lambdaHeaderCache.Load(keyword); ok {
		return cached.(*regexp.Regexp)
	}
	re := regexp.MustCompile(
		`(?m)^[ \t]*` + regexp.QuoteMeta(keyword) +
			`[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?([A-Za-z_][A-Za-z0-9_-]*)[ \t]*=(?:[ \t]|$)`,
	)
	lambdaHeaderCache.Store(keyword, re)
	return re
}

// extractLambdaDeclarationSlices returns every edition-2026 spec or trait in
// source: `spec ticket isOpen = row => row.status == "open"`.
//
// # Why the brace slicer cannot find these
//
// ExtractDeclarationSlices' whole extent rule is "a header ending in `{`, then
// the matching `}`", and this form has no braces: its body is a lambda, and a
// lambda's body "extends as far as it can" (the precedence table's last row).
// Without this function every `=` spec and trait in a tree would be invisible
// to the loader -- not refused, not skipped, ABSENT -- and every query applying
// one would fail as if the predicate had never been declared.
//
// # The extent
//
// From the header to the last non-blank byte before the next top-level
// declaration: a line, at bracket depth zero, that opens with `@` (an
// annotation preamble) or a declaration keyword. A continuation line opens
// with anything else -- `&&`, `||`, `?`, `:`, a name -- so a body broken
// across lines stays one slice. Scanned on the comment-blanked view, string
// and bracket aware, like every other slicer here.
func extractLambdaDeclarationSlices(source, keyword string) []languageParser.DeclarationSlice {
	scan := languageParser.BlankComments(source)
	matches := lambdaDeclarationHeaderRegexp(keyword).FindAllStringSubmatchIndex(scan, -1)
	if len(matches) == 0 {
		return nil
	}
	var out []languageParser.DeclarationSlice
	for _, m := range matches {
		headerStart, headerEnd := m[0], m[1]
		if languageParser.BraceDepthBefore(scan, headerStart) != 0 {
			continue
		}
		end := lambdaDeclarationEnd(scan, headerEnd)
		preambleStart := languageParser.PreambleStartOf(source, headerStart)
		out = append(out, languageParser.DeclarationSlice{
			Source: source[preambleStart:end],
			Name:   source[m[2]:m[3]],
			Start:  preambleStart,
			End:    end,
		})
	}
	return out
}

// lambdaDeclarationKeywords are the words a top-level declaration line opens
// with; a line opening with one ends the body above it.
var lambdaDeclarationKeywords = map[string]bool{
	"spec": true, "trait": true, "use": true, "query": true, "mutate": true, "mutation": true,
	"logic": true, "automation": true, "shape": true, "concept": true, "builtin": true,
	"tool": true, "prompt": true, "provider": true, "policy": true, "rule": true,
	"seed": true, "action": true, "capability": true, "func": true,
}

// lambdaDeclarationEnd returns the end (exclusive) of a brace-less body that
// begins at from, over the comment-blanked view scan.
func lambdaDeclarationEnd(scan string, from int) int {
	depth := 0
	inString := byte(0)
	escaped := false
	last := from - 1
	for i := from; i < len(scan); i++ {
		c := scan[i]
		if inString != 0 {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == inString:
				inString = 0
			}
			last = i
			continue
		}
		switch c {
		case '"', '`':
			inString = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				// An unbalanced close belongs to something around this
				// declaration, never to it.
				return last + 1
			}
			depth--
		case '\n':
			if depth == 0 && nextLineStartsDeclaration(scan[i+1:]) {
				return last + 1
			}
		}
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			last = i
		}
	}
	return last + 1
}

// nextLineStartsDeclaration reports whether the next non-blank line of rest
// opens a new top-level declaration (or rest has no such line: end of file).
func nextLineStartsDeclaration(rest string) bool {
	for _, line := range strings.Split(rest, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "@") {
			return true
		}
		word := trimmed
		if i := strings.IndexFunc(trimmed, func(r rune) bool {
			return !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
		}); i >= 0 {
			word = trimmed[:i]
		}
		return lambdaDeclarationKeywords[word] && len(trimmed) > len(word)
	}
	return true
}
