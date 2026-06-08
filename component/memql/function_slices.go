package memql

// function_slices.go provides text-based extraction of function
// declarations (struct-form and procedural) from the new
// consolidated DSL tree. Each slice carries one function +
// its @-attribute preamble, ready to feed into the legacy
// per-function parse pipeline (tryParseNewFunctionSyntax).
//
// This is the function-loader companion to concepts_only_extractor.go:
// instead of refactoring the legacy function loader to walk the
// unified tree end-to-end, we slice each function out of its
// consolidated file and feed the existing single-function pipeline.
// The result is the same -- a *Function ready to register -- and
// the legacy validation + concept resolution still runs.

import (
	"regexp"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// FunctionSlice is one extracted function declaration with its
// preamble and inferred kind / name.
type FunctionSlice struct {
	Source string                      // slice text (preamble + declaration body)
	Kind   languageParser.FunctionType // Query / Mutation / Logic / Automation / Spec / Shape / Tool / etc.
	Name   string                      // function name from the header
}

// functionDeclHeader matches every top-level function-style header
// at column 0 we care about:
//
//   - struct-form: `query NAME {`, `mutation NAME {`, `logic NAME {`
//   - procedural: `func (Query) NAME`, `func (Mutation) NAME`, ...
//
// Captures the (keyword, name) for the slice. We don't match
// `concept` -- those go through ExtractConceptDecls.
//
// Excludes `spec` and `trait` -- those have their own dedicated
// parser (component/memql/spec_parser.go) and don't flow through
// the function-slicer pipeline. Including them here would just
// produce debug-level parse failures on every load.
//
// Excludes `automation` -- automations are NOT function-registry
// constructs. They are event-triggered side-effects loaded by the
// dedicated runtime loader (component/automations/unified_loader.go),
// nothing imports them via `use`, and the function type switch has no
// automation case (an `*ast.AutomationDef` body would be rejected).
// Slicing them here only produced "body is not an expression node"
// skips on every load (issue #212). The inner `logic NAME { ... }`
// invocations that live inside an automation's `step` blocks are
// likewise NOT top-level declarations -- the brace-depth guard in
// ExtractFunctionSlices skips them so they aren't mis-extracted as
// standalone logic slices.
var functionDeclHeader = regexp.MustCompile(
	`(?m)^[ \t]*(?:` +
		// struct-form: `<kind> [<concept>] <name> {`. The optional
		// `<concept>` identifier is the post-migration signature-bound
		// concept on queries / mutations / shapes / seeds (PR #48 / #49);
		// logic never binds a concept in the signature, so for it the
		// optional segment will always be absent in authored sources.
		`(query|mutation|logic)[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{` +
		`|` +
		// procedural: `func (Kind) NAME(`
		`func[ \t]*\([ \t]*(Query|Mutation|Logic|Shape|Tool|Builtin|Prompt|Provider|Policy)[ \t]*\)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\(` +
		`)`,
)

// kindFromString maps a parser keyword to the FunctionType enum.
// Spec / Trait / Automation omitted -- dedicated paths.
var kindFromString = map[string]languageParser.FunctionType{
	"query":    languageParser.FunctionTypeQuery,
	"mutation": languageParser.FunctionTypeMutation,
	"logic":    languageParser.FunctionTypeLogic,
	"Query":    languageParser.FunctionTypeQuery,
	"Mutation": languageParser.FunctionTypeMutation,
	"Logic":    languageParser.FunctionTypeLogic,
	"Shape":    languageParser.FunctionTypeShape,
	"Tool":     languageParser.FunctionTypeTool,
	"Builtin":  languageParser.FunctionTypeBuiltin,
	"Prompt":   languageParser.FunctionTypePrompt,
	"Provider": languageParser.FunctionTypeProvider,
	"Policy":   languageParser.FunctionTypePolicy,
}

// ExtractFunctionSlices scans `source` for top-level function
// declarations and returns each as a self-contained slice ready
// to feed through the legacy single-function parser pipeline.
//
// Slice extents:
//   - START: the first line of the @-attribute preamble immediately
//     above the function header (walking upward; blank lines stop).
//   - END: the matching close-brace of the function body.
//
// Concepts are explicitly NOT extracted by this function -- use
// ExtractConceptDecls for those. Builtins are extracted (they use
// `func (Builtin) NAME` shape in the legacy tree) so the unified
// loader can later wire them.
//
// File-top `use` declarations are extracted up-front and prepended to
// every emitted slice. Without this, the per-slice parser sees an
// empty Uses list and the function-loader's concept-binding logic
// (the path that turns the bare `concept` keyword in procedural
// queries into a real comparison) can't determine which concept the
// function targets.
func ExtractFunctionSlices(source string) []FunctionSlice {
	// Detect headers on a comment-blanked view so a `func (Receiver)
	// ...` or `<kind> <name> {` token that only appears inside a `//`
	// or `/* */` comment is never extracted as a standalone construct
	// (memql#1074). BlankComments preserves byte offsets, so every
	// capture-group index below maps 1:1 onto the original `source`,
	// which is what the keyword/name/body slices read from -- keeping
	// authored comments in the emitted slice text.
	scan := languageParser.BlankComments(source)
	matches := functionDeclHeader.FindAllStringSubmatchIndex(scan, -1)
	if len(matches) == 0 {
		return nil
	}

	// Collect file-top `use ... .{ ... }` blocks once so each slice
	// inherits them.
	usePreamble := extractUseDeclarations(source)

	var slices []FunctionSlice
	for _, m := range matches {
		headerStart := m[0]
		headerEnd := m[1]

		// Only extract top-level (brace-depth-0) declarations. A header
		// that sits inside another construct's braces is not a standalone
		// declaration -- e.g. a `logic NAME { ... }` invocation inside an
		// automation `step` block, or a nested construct in a logic body.
		// Without this guard those get mis-extracted as bogus slices that
		// then fail to parse (issue #212). `use ...{ ... }` preambles have
		// balanced braces so they net to depth 0 and don't shift it.
		if braceDepthBefore(scan, headerStart) != 0 {
			continue
		}

		// Pull the keyword + name from the capture groups. Group
		// indices are (in order):
		//   m[2..3] = struct-form keyword
		//   m[4..5] = struct-form name
		//   m[6..7] = procedural receiver
		//   m[8..9] = procedural name
		var kindStr, name string
		switch {
		case m[2] >= 0:
			kindStr = source[m[2]:m[3]]
			name = source[m[4]:m[5]]
		case m[6] >= 0:
			kindStr = source[m[6]:m[7]]
			name = source[m[8]:m[9]]
		default:
			continue
		}
		kind, ok := kindFromString[kindStr]
		if !ok {
			continue
		}

		// Find the matching close brace. For struct-form the
		// opening `{` is at headerEnd-1. For procedural the body
		// opens after the (...) signature -- find the first `{` at
		// or after headerEnd.
		// Brace location + matching runs on the comment-blanked `scan`
		// (offset-identical to `source`) so braces inside a comment in
		// the body don't perturb the depth count (memql#1074).
		openIdx := -1
		if kindStr == "query" || kindStr == "mutation" || kindStr == "logic" {
			openIdx = headerEnd - 1
		} else {
			// Procedural: scan from headerEnd for the first `{`.
			rel := strings.IndexByte(scan[headerEnd:], '{')
			if rel < 0 {
				continue
			}
			openIdx = headerEnd + rel
		}
		closeIdx := findMatchingCloseBraceRune(scan, openIdx)
		if closeIdx < 0 {
			continue
		}

		// Walk backwards for the @-attribute preamble.
		preambleStart := headerStart
		for k := headerStart - 1; k >= 0; k-- {
			lineStart := strings.LastIndexByte(source[:k], '\n') + 1
			line := strings.TrimRight(source[lineStart:k+1], "\r\n")
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "@") {
				preambleStart = lineStart
				k = lineStart - 1
				continue
			}
			if strings.HasPrefix(trimmed, "//") {
				preambleStart = lineStart
				k = lineStart - 1
				continue
			}
			if trimmed == "" {
				break
			}
			break
		}

		body := source[preambleStart : closeIdx+1]
		if usePreamble != "" {
			body = usePreamble + body
		}
		slices = append(slices, FunctionSlice{
			Source: body,
			Kind:   kind,
			Name:   name,
		})
	}

	return slices
}

// extractUseDeclarations returns every file-top `use ... .{ ... }`
// declaration concatenated with newlines + a trailing blank line.
// Used by ExtractFunctionSlices to prepend the file's import context
// to each per-function slice so the per-slice parser sees the same
// Uses list the full-file parser would. Returns an empty string when
// the source has no use declarations.
func extractUseDeclarations(source string) string {
	matches := useBlockRE.FindAllString(source, -1)
	if len(matches) == 0 {
		return ""
	}
	var b strings.Builder
	for _, m := range matches {
		b.WriteString(m)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}

// useBlockRE matches `use <path>.{ <names> }` declarations including
// multi-line bodies. The path may contain dots; the body may span
// multiple lines.
var useBlockRE = regexp.MustCompile(`(?m)^[ \t]*use[ \t]+[A-Za-z_][A-Za-z0-9_.]*\.\{[^}]*\}`)

// braceDepthBefore returns the net number of unclosed `{` in
// source[0:pos]. Used to decide whether a construct header is
// top-level (depth 0) or nested inside another construct (depth > 0).
// Mirrors the string + line-comment handling of
// findMatchingCloseBraceRune so braces inside string literals and
// `// ...` comments don't perturb the count.
func braceDepthBefore(source string, pos int) int {
	if pos > len(source) {
		pos = len(source)
	}
	depth := 0
	inString := false
	for i := 0; i < pos; i++ {
		c := source[i]
		if inString {
			if c == '\\' && i+1 < pos {
				i++
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '/':
			if i+1 < len(source) && source[i+1] == '/' {
				nl := strings.IndexByte(source[i:], '\n')
				if nl < 0 {
					return depth
				}
				i += nl
			}
		case '{':
			depth++
		case '}':
			depth--
		}
	}
	return depth
}

// findMatchingCloseBraceRune walks source from openIdx (position of
// `{`) and returns the index of the matching `}`. Strings + chars
// + line comments are tolerated. Returns -1 when unbalanced.
func findMatchingCloseBraceRune(source string, openIdx int) int {
	if openIdx < 0 || openIdx >= len(source) || source[openIdx] != '{' {
		return -1
	}
	depth := 0
	inString := false
	for i := openIdx; i < len(source); i++ {
		c := source[i]
		if inString {
			if c == '\\' && i+1 < len(source) {
				i++
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '/':
			if i+1 < len(source) && source[i+1] == '/' {
				// Skip to end of line.
				nl := strings.IndexByte(source[i:], '\n')
				if nl < 0 {
					return -1
				}
				i += nl
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
