// Package parser -- struct-form rewriter.
//
// The DSL author surface uses struct-form for every procedural
// construct: `query NAME { args, filter, shape }`, `mutation NAME
// { args, insert <concept> { ... } }`, `logic NAME { args, body }`,
// `automation NAME { step <name> { ... } }`, plus file-top `args { ... }`
// blocks. The general parser's grammar reads the older procedural
// form (`func (Query) NAME(_ any) (any, error) { return <expr>, nil }`).
//
// Per issue #93, the rewriter's procedural output uses `_ any` as
// the parameter (purely a placeholder slot -- bodies reference
// args via `args.X`, resolved against the args block) and does
// not emit `return ctx, nil` trailers. The engine parser still
// recognises both `args.X` and `ctx.X` for backwards-compatibility
// with non-rewriter call sites (policy evaluator); that recognition
// is a separate cleanup.
//
// This file is the bridge: every struct-form input gets translated
// to the equivalent procedural source string before the parser
// proper sees it. The five per-construct rewriters used to live in
// query_rewrite.go / mutation_rewrite.go / logic_rewrite.go /
// automation_rewrite.go / args_rewrite.go, plus normalise_all.go --
// 1306 LOC across six files with the same iteration skeleton
// duplicated four times. They are consolidated here.
//
// Public surface (the only entry points external packages use):
//
//   NormaliseAll(source) -- the five-stage chain, used by every
//                           DSL-loading path. Each stage is a no-op
//                           when its detector doesn't match.
//   NormaliseLogicSource, NormaliseAutomationSource
//   LooksLikeStructLogic, LooksLikeStructAutomation,
//   LooksLikeLegacyAutomation -- called by automations/loader.go
//                                to drive its own per-slice rewrite
//                                between parse and compile.

package parser

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/znasllc-io/memql/component/language/dslclause"
)

// =============================================================================
// Shared infrastructure
// =============================================================================

// findMatchingCloseBrace scans `s` starting at `openIdx` (which must
// point at `{`) and returns the index of the matching `}`. Returns
// -1 if not found. Doesn't try to be clever about strings or
// comments -- sufficient for the struct-form bodies which don't
// embed unmatched braces in string literals.
func findMatchingCloseBrace(s string, openIdx int) int {
	if openIdx < 0 || openIdx >= len(s) || s[openIdx] != '{' {
		return -1
	}
	depth := 0
	for i := openIdx; i < len(s); i++ {
		switch s[i] {
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

// skipStringLiteral returns the index of the closing quote of the string
// literal that starts at s[i] (which must be `"` or a backtick). A double-
// quoted literal honours `\` escapes; a backtick literal is raw. An
// unterminated literal returns len(s)-1 so the caller advances to EOF.
func skipStringLiteral(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		if q == '"' && s[j] == '\\' {
			j++ // skip the escaped byte
			continue
		}
		if s[j] == q {
			return j
		}
	}
	return len(s) - 1
}

// matchBraceStrAware is findMatchingCloseBrace made string-aware: braces inside
// a "..." or `...` literal are not counted. It expects a COMMENT-BLANKED view
// (BlankComments), so comment braces are already spaces. Together they let a
// stamp value like `id: "a}b"` or a `// }` comment sit inside a block without
// throwing off the framing.
func matchBraceStrAware(s string, openIdx int) int {
	if openIdx < 0 || openIdx >= len(s) || s[openIdx] != '{' {
		return -1
	}
	depth := 0
	for i := openIdx; i < len(s); i++ {
		switch s[i] {
		case '"', '`':
			i = skipStringLiteral(s, i)
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

// matchBraceInBody finds the `}` matching the `{` at openIdx in a RAW body
// (comments and strings intact), string- and comment-aware. BlankComments is
// offset-preserving, so the returned index still indexes `body`. The inner
// block extractors (args / logic body / automation step) use this so a `}`
// inside a string value or comment does not truncate the block, keeping them
// consistent with the outer frame (rewriteEachBlock) and scanMutationBlocks.
func matchBraceInBody(body string, openIdx int) int {
	return matchBraceStrAware(BlankComments(body), openIdx)
}

// mutBodyBlock is a top-level `<keyword> { ... }` block located by
// scanMutationBlocks. `inner` is the raw content between the braces (comments
// and strings intact -- taken from the original body, not the blanked view).
type mutBodyBlock struct {
	keyword string // the identifier before `{` (args/insert/update/accept/stamp/...)
	named   string // a second header identifier if present (the retired `insert Concept {` form)
	inner   string
	at      int // offset of keyword in the scanned text, for a refusal to name
	innerAt int // offset of inner in the scanned text
}

// scanMutationBlocks is the SINGLE source of block framing for a mutation body
// (or a write block's inner). It walks a comment-blanked, string- and paren-
// aware view once and returns, in source order:
//
//   - blocks: every top-level `<keyword> { ... }` (or `<keyword> <name> { ... }`)
//   - hasFields: whether any depth-0 `key: value` FIELD content exists (which is
//     a legacy write body when inside insert/update, or a stray at the mutation
//     top level)
//   - fieldSample: the first such field line, for error messages
//
// Because every reader (write-kind detection, nested accept/stamp, stray
// guards) consumes THIS result rather than re-matching the text with its own
// regex, they can never disagree about which blocks exist -- which is what
// retires the same-line / boundary-anchoring class of bugs (a block is found
// identically whether it sits on its own line, shares a line with another
// block's `}`, or contains a brace inside a string literal). A `{` in value
// position (after a `:` or inside `(...)`) is an object literal, matched and
// skipped, never mistaken for a block.
//
// It also returns strayLine: the first depth-0 line that carries words but is
// neither a `key: value` field nor a block header -- a line no reader above
// consumes (`filter x == 1` at the mutation top level). The top-level caller
// refuses it; inside a write block a field's value may continue onto the next
// line, so the write block's caller does not.
func scanMutationBlocks(body string) (blocks []mutBodyBlock, hasFields bool, fieldSample string, strayLine string, err error) {
	blanked := BlankComments(body)
	n := len(blanked)
	var idents []string
	var identAt []int
	identStart := -1
	lineFirst := -1 // where the current line's first word starts
	inValue := false
	parenDepth := 0
	isIdent := func(c byte) bool {
		return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
	}
	endIdent := func(at int) {
		if identStart >= 0 {
			idents = append(idents, blanked[identStart:at])
			identAt = append(identAt, identStart)
			identStart = -1
		}
	}
	// noteStray records the current line as stray when it carried words that
	// were neither a field key nor a block header.
	noteStray := func() {
		if strayLine == "" && !inValue && parenDepth == 0 && len(idents) > 0 && lineFirst >= 0 {
			strayLine = lineOfIndex(body, blanked, lineFirst)
		}
	}
	lineOf := func(at int) string {
		start := at
		for start > 0 && blanked[start-1] != '\n' {
			start--
		}
		end := at
		for end < n && blanked[end] != '\n' {
			end++
		}
		return strings.TrimSpace(body[start:end])
	}
	for i := 0; i < n; {
		c := blanked[i]
		switch {
		case c == '"' || c == '`':
			endIdent(i)
			i = skipStringLiteral(blanked, i) + 1
		case c == ' ' || c == '\t':
			endIdent(i)
			i++
		case c == '\n' || c == '\r':
			endIdent(i)
			if parenDepth == 0 {
				noteStray()
				inValue = false
				idents, identAt = nil, nil
				lineFirst = -1
			}
			i++
		case c == '(':
			endIdent(i)
			parenDepth++
			i++
		case c == ')':
			endIdent(i)
			if parenDepth > 0 {
				parenDepth--
			}
			i++
		case c == ',':
			endIdent(i)
			if parenDepth == 0 {
				inValue = false
				idents, identAt = nil, nil
			}
			i++
		case c == ':':
			endIdent(i)
			if parenDepth == 0 {
				inValue = true
				if !hasFields {
					hasFields = true
					fieldSample = lineOf(i)
				}
			}
			i++
		case c == '{':
			endIdent(i)
			closeIdx := matchBraceStrAware(blanked, i)
			if closeIdx < 0 {
				return nil, false, "", "", refuseAtBody(i, 1, fmt.Errorf("mutation body: `{` has no matching `}`"))
			}
			if inValue || parenDepth > 0 {
				// Object-literal value (`field: { ... }`), not a block.
				i = closeIdx + 1
			} else {
				switch len(idents) {
				case 1:
					blocks = append(blocks, mutBodyBlock{keyword: idents[0], inner: body[i+1 : closeIdx], at: identAt[0], innerAt: i + 1})
				case 2:
					blocks = append(blocks, mutBodyBlock{keyword: idents[0], named: idents[1], inner: body[i+1 : closeIdx], at: identAt[0], innerAt: i + 1})
				case 0:
					return nil, false, "", "", refuseAtBody(i, 1, fmt.Errorf("mutation body: `{ ... }` block with no keyword"))
				default:
					return nil, false, "", "", refuseAtBody(identAt[0], len(idents[0]), fmt.Errorf("mutation body: block header `%s ...` has too many words", idents[0]))
				}
				i = closeIdx + 1
			}
			idents, identAt = nil, nil
			inValue = false
			lineFirst = -1
		case isIdent(c):
			if identStart < 0 {
				identStart = i
				if lineFirst < 0 {
					lineFirst = i
				}
			}
			i++
		default:
			// Value punctuation (`.`, `=`, `-`, digits are idents already, etc.).
			endIdent(i)
			i++
		}
	}
	endIdent(n)
	noteStray()
	return blocks, hasFields, fieldSample, strayLine, nil
}

// lineOfIndex returns the trimmed line of body containing offset at, located
// on the blanked view (comments are blank there, so a comment is never taken
// for a line's content) and sliced from the original.
func lineOfIndex(body, blanked string, at int) string {
	start := at
	for start > 0 && blanked[start-1] != '\n' {
		start--
	}
	end := at
	for end < len(blanked) && blanked[end] != '\n' {
		end++
	}
	return strings.TrimSpace(body[start:end])
}

// rewriteEachBlock walks every struct-form construct in `source`
// matching `header`, calls `emit` on each, and splices the result
// back in place. Processes matches in reverse so byte offsets stay
// stable across splices. When `needsConcept` is true, the concept
// binding MUST come from the construct's signature
// (`<kind> <Concept> <name> { ... }`); the annotation + file-top
// directive fallbacks the rewriter used to honour were retired in
// memql#314.
//
// `kindLabel` is used in error messages ("struct-form query", etc).
// `emit` returns the procedural source that replaces the matched
// block.
func rewriteEachBlock(
	source string,
	header *regexp.Regexp,
	kindLabel string,
	needsConcept bool,
	emit func(name, conceptId, body, preamble string) (string, error),
) (string, error) {
	// Detect headers against a comment-blanked view so a `<kind> <name>
	// {` (or `func (Receiver) ...`) token inside a `//` / `/* */`
	// comment is never matched as a real construct header (memql#1074).
	// BlankComments preserves byte offsets, so every index below maps
	// 1:1 onto the original `source`, which is what we splice into.
	scan := BlankComments(source)
	matches := header.FindAllStringIndex(scan, -1)
	if len(matches) == 0 {
		return source, nil
	}
	out := source
	for i := len(matches) - 1; i >= 0; i-- {
		h := matches[i]

		// A refusal names the author's text it refuses (rewrite_errors.go):
		// the construct's name unless the emitter says which text. The
		// offsets index source, which `out` still equals up to this
		// construct's closing brace.
		refuse := func(err error, start, end int) error {
			return &RewriteError{err: err, input: source, start: start, end: end}
		}
		nameExtent := func() (int, int) {
			if sub := header.FindStringSubmatchIndex(scan[h[0]:h[1]]); len(sub) >= 4 && sub[len(sub)-2] >= 0 {
				return h[0] + sub[len(sub)-2], h[0] + sub[len(sub)-1]
			}
			return h[0], h[1]
		}

		openIdx := h[1] - 1
		// Match the construct's closing brace on the comment-blanked, string-
		// aware view (offsets map 1:1 to `out` for this not-yet-spliced region,
		// since matches are processed in reverse). This keeps a `}` inside a
		// string value or a `// }` comment from ending the construct early --
		// findMatchingCloseBrace was brace-only and truncated such bodies.
		closeIdx := matchBraceStrAware(scan, openIdx)
		if closeIdx < 0 {
			return "", refuse(fmt.Errorf("%s: missing closing brace", kindLabel), openIdx, openIdx+1)
		}

		// From `scan`, not `out`: the header was LOCATED on the blanked view and
		// this regex's whitespace runs can match blanked comment bytes, so
		// re-matching against raw text makes the two passes disagree whenever a
		// comment sits in the header region (memql#2906). Name bytes are
		// non-space and identical in both views.
		headerLine := scan[h[0]:h[1]]
		nameMatch := header.FindStringSubmatch(headerLine)
		if len(nameMatch) < 2 {
			return "", refuse(fmt.Errorf("%s: could not extract name", kindLabel), h[0], h[1])
		}
		// Headers that carry a signature-bound concept expose it as
		// the second-to-last submatch (capture group 1); the name is
		// always the trailing group. Headers that don't bind a concept
		// in the signature have only the name capture.
		signatureConcept := ""
		var name string
		if len(nameMatch) >= 3 {
			signatureConcept = nameMatch[1]
			name = nameMatch[2]
		} else {
			name = nameMatch[1]
		}

		var conceptId string
		if needsConcept {
			if signatureConcept == "" {
				start, end := nameExtent()
				return "", refuse(fmt.Errorf("%s %q: missing concept binding -- declare via the signature form `<kind> <Concept> <name> { ... }`", kindLabel, name), start, end)
			}
			conceptId = signatureConcept
		}

		body := out[openIdx+1 : closeIdx]
		preamble := precedingAnnotationBlock(out, h[0])
		rewritten, err := emit(name, conceptId, body, preamble)
		if err != nil {
			start, end := nameExtent()
			var r *refusal
			if errors.As(err, &r) {
				pre := h[0] - len(preamble)
				if s, e, ok := r.locate(scan[pre:closeIdx+1], openIdx+1-pre); ok {
					start, end = pre+s, pre+e
				}
			}
			return "", refuse(fmt.Errorf("%s %q: %w", kindLabel, name, err), start, end)
		}
		out = out[:h[0]] + rewritten + out[closeIdx+1:]
	}
	return out, nil
}

// precedingAnnotationBlock returns the contiguous run of annotation
// (`@...`) and comment (`//`) lines immediately preceding the
// construct header at headerStart. A blank line or any non-annotation/
// non-comment line terminates the block. The returned text lets a
// per-construct emitter inspect the construct's annotations (e.g.
// `@unbounded("reason")`) without re-parsing the whole file.
func precedingAnnotationBlock(src string, headerStart int) string {
	blockStart := headerStart
	for k := headerStart - 1; k >= 0; {
		lineStart := strings.LastIndexByte(src[:k], '\n') + 1
		line := strings.TrimSpace(strings.TrimRight(src[lineStart:k+1], "\r\n"))
		if strings.HasPrefix(line, "@") || strings.HasPrefix(line, "//") {
			blockStart = lineStart
			k = lineStart - 1
			continue
		}
		break
	}
	return src[blockStart:headerStart]
}

// emitFuncHeader writes the procedural function preamble: optional
// file-top `args { ... }` block, then the receiver-style function
// signature. The parameter is always `_` -- the body references
// args via `args.X` (resolved against the args block), so the
// function parameter itself is purely a placeholder slot.
func emitFuncHeader(sb *strings.Builder, receiver, name, argsText, returns string) {
	if strings.TrimSpace(argsText) != "" {
		sb.WriteString("args {\n")
		sb.WriteString(argsText)
		if !strings.HasSuffix(strings.TrimRight(argsText, " \t"), "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("}\n")
	}
	sb.WriteString(fmt.Sprintf("func (%s) %s(_ any)", receiver, name))
	if returns != "" {
		sb.WriteString(" ")
		sb.WriteString(returns)
	}
	sb.WriteString(" {\n")
}

// extractArgsBlock pulls the `args { ... }` block out of a struct
// body (if present) and returns its raw inner text. Empty when no
// args block is found.
var argsBlockHeader = regexp.MustCompile(`(^|[\n\r])[ \t]*args[ \t]*\{`)

func extractArgsBlock(body string) (string, error) {
	// Located on the BLANKED view so a commented-out `args {` header is not
	// matched (memql#2906). matchBraceInBody below already blanks, so
	// matching the header on raw text made the two disagree: the header was
	// found in a comment, its brace was sought in a blanked view where that
	// `{` is a space, and the load failed with "missing closing brace" naming
	// a block that does not exist. Offsets are preserved, so every slice
	// below still indexes `body`.
	loc := argsBlockHeader.FindStringIndex(BlankComments(body))
	if loc == nil {
		return "", nil
	}
	openOffset := strings.LastIndex(body[loc[0]:loc[1]], "{")
	open := loc[0] + openOffset
	close := matchBraceInBody(body, open)
	if close < 0 {
		kw, width := firstWordAt(body, loc[0])
		return "", refuseAtBody(kw, width, fmt.Errorf("`args { ... }` block missing closing brace"))
	}
	return body[open+1 : close], nil
}

// The C5 (memql#2035) accept/stamp blocks -- `accept { f1, f2 }` (public field
// names auto-binding to same-named args) and `stamp { k: v }` (server-set
// fields) -- are located by scanMutationBlocks, the single comment/string-aware
// framing scan, rather than by per-reader regexes (which kept disagreeing about
// same-line and boundary-adjacent blocks).

// acceptFieldName matches a single bare identifier (a field name) in an
// `accept { ... }` list.
var acceptFieldName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseAcceptNames splits an `accept { ... }` block body into field
// names. Entries are separated by commas and/or newlines; `//` line
// comments and blank entries are ignored. Every entry must be a bare
// identifier -- an `accept` list carries field NAMES, never `key:
// value` pairs (those belong in `stamp`).
//
// A refusal of an entry names it at its offset in raw (rewrite_errors.go);
// the caller shifts that to the mutation body.
func parseAcceptNames(raw string) ([]string, error) {
	var names []string
	lineStart := 0
	for _, line := range strings.Split(raw, "\n") {
		here := lineStart
		lineStart += len(line) + 1
		// Strip line comments.
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		entryStart := here
		for _, entry := range strings.Split(line, ",") {
			at := entryStart + len(entry) - len(strings.TrimLeftFunc(entry, unicode.IsSpace))
			entryStart += len(entry) + 1
			tok := strings.TrimSpace(entry)
			if tok == "" {
				continue
			}
			if strings.Contains(tok, ":") {
				return nil, refuseAtBody(at, len(tok), fmt.Errorf("accept block entry %q looks like a `key: value` pair -- the `accept { ... }` list carries public field NAMES only (auto-bound from same-named args); put server-set `key: value` fields in `stamp { ... }`", tok))
			}
			if !acceptFieldName.MatchString(tok) {
				return nil, refuseAtBody(at, len(tok), fmt.Errorf("accept block entry %q is not a valid field name", tok))
			}
			names = append(names, tok)
		}
	}
	if len(names) == 0 {
		return nil, refuseClause("accept", fmt.Errorf("`accept { ... }` block is empty -- list at least one public field name, or drop the block"))
	}
	return names, nil
}

// argsFieldName matches the leading identifier of an args-block field
// declaration line (e.g. `name` in `name string @required`).
var argsFieldName = regexp.MustCompile(`^[ \t]*([A-Za-z_][A-Za-z0-9_]*)\b`)

// argNamesFromArgsText returns the set of top-level field names
// declared in an args block's inner text. Used to validate that every
// `accept` name has a same-named arg to auto-bind to.
func argNamesFromArgsText(argsText string) map[string]bool {
	out := map[string]bool{}
	depth := 0
	for _, line := range strings.Split(argsText, "\n") {
		trimmed := strings.TrimSpace(line)
		// Track brace depth so nested object fields don't register as
		// top-level args.
		if depth == 0 && trimmed != "" && !strings.HasPrefix(trimmed, "//") {
			if m := argsFieldName.FindStringSubmatch(line); m != nil {
				out[m[1]] = true
			}
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if depth < 0 {
			depth = 0
		}
	}
	return out
}

// =============================================================================
// Public chain: NormaliseAll
// =============================================================================

// legacyProceduralAuthorForm matches `func (Receiver) name(...)`.
// Pinned to the 12 receiver names the engine has ever
// accepted as author-written procedural form. memql#303 retired the
// author-facing procedural surface: the DSL-load entry point
// (component/memql.tryParseNewFunctionSyntax) calls
// RejectLegacyProceduralAuthorForm before NormaliseAll runs, so any
// author-written .memql slice in that shape fails fast with a
// migration hint.
//
// The check is NOT applied inside NormaliseAll itself because the
// per-construct rewriters below synthesise procedural source as an
// internal IR (e.g. `func (Query) NAME(_ any) (any, error) { return
// X, nil }`) -- gating NormaliseAll directly would block the
// rewriter's own output from re-entering the parser. The compiler
// API (component/language/compiler.CompileSource) likewise calls
// NormaliseAll on whatever source the caller hands it; its unit
// tests historically use procedural source as compiler-IR fixtures,
// which remains supported.
// The whitespace here must be AT LEAST as permissive as the slicer that
// actually extracts these declarations (component/memql's
// functionSliceHeader: `func[ \t]*\([ \t]*(Query|...)[ \t]*\)`). It was not.
// This pattern required column 0, exactly one space after `func`, and no space
// inside the parens, while the slicer allowed leading indentation and flexible
// spacing. Every spelling in that gap -- `  func (Query) queryFoo(`,
// `func  (Query) ...`, `func ( Query ) ...` -- sailed past the rejection,
// got sliced, parsed and REGISTERED. A retired author form kept loading, and
// because the naming gate has no `func` arm either, a kind-prefixed construct
// could ship with every test green (memql#2853 round-3 review).
//
// A rejection gate narrower than the thing it guards is not a gate. This is
// the same defect as #2 in test/dslconformance/naming_conventions_test.go's header (`^` pinned
// to column 0 while the real matcher accepts leading whitespace), one layer
// down.
var legacyProceduralAuthorForm = regexp.MustCompile(`(?m)^[ \t]*func[ \t]*\([ \t]*(Query|Mutation|Logic|Spec|Automation|Builtin|Prompt|Provider|Shape|Tool|Policy|Seed)[ \t]*\)`)

// RejectLegacyProceduralAuthorForm returns an error when the source
// contains author-written procedural form (`func (Receiver) name(ctx
// any) ...`) for any of the 12 retired receiver names. Called by
// the DSL load-time entry point (memql#303); see the comment on
// legacyProceduralAuthorForm for why this isn't wired into
// NormaliseAll directly.
func RejectLegacyProceduralAuthorForm(source string) error {
	// Scan a comment-blanked view so a `func (Receiver) ...` token that
	// only appears inside a `//` or `/* */` comment never trips the
	// rejection gate (memql#1074).
	m := legacyProceduralAuthorForm.FindStringSubmatchIndex(BlankComments(source))
	if m == nil {
		return nil
	}
	// A *RewriteError over source, naming the `func` that opens the form
	// (rewrite_errors.go); PositionRewriteError places it.
	kw, width := firstWordAt(source, m[0])
	return &RewriteError{
		err:   fmt.Errorf("legacy procedural form `func (%s) ...` is retired (memql#303) -- author every construct in struct form: `<kind> <Concept> <name> { args { ... } ... }`", source[m[2]:m[3]]),
		input: source, start: kw, end: kw + width,
	}
}

// structFormStep is one stage of the struct-form rewriter chain: a
// keyword-named detector + applier pair.
type structFormStep struct {
	name   string
	detect func(string) bool
	apply  func(string) (string, error)
}

// structFormSteps is the authoritative ordered struct-form rewriter
// chain. NormaliseAll iterates it, and StructFormKeywords is derived
// from it, so the rewriter's recognised construct set has exactly one
// definition. "file-top args" is intentionally present as a rewrite
// stage but excluded from StructFormKeywords because it is not an
// author-facing top-level construct keyword -- it is the bare
// `args { ... }` block.
var structFormSteps = []structFormStep{
	{"query", LooksLikeStructQuery, NormaliseQuerySource},
	// The author keyword is `mutate` (mutationStructHeader matches `mutate`,
	// C1/memql#2041); `mutation` is the invocation-step prefix only. The step
	// name IS the author-facing keyword StructFormKeywords reports, so it must
	// be `mutate` -- the #2124 drift test pins dslspec's construct keyword to it.
	{"mutate", LooksLikeStructMutation, NormaliseMutationSource},
	{"file-top args", LooksLikeFileTopArgs, NormaliseFileTopArgs},
}

// statementConstructKeywords are the construct keywords the parser reads as
// written, their statement bodies included (epic memql#5370): no rewriter
// stage expands them.
var statementConstructKeywords = []string{"logic", "automation"}

// StructFormKeywords is the set of author-facing construct keywords of the
// struct form, in declaration order: query / mutate, which the rewriter
// expands to the internal func form, then logic / automation, which the
// parser reads as written. It is the single source the #2124 drift test
// compares dslspec's "function" category constructs against. Derived from
// structFormSteps (excluding the non-construct "file-top args" stage) and
// statementConstructKeywords, so the list cannot drift from the parser.
var StructFormKeywords = func() []string {
	out := make([]string, 0, len(structFormSteps)+len(statementConstructKeywords))
	for _, s := range structFormSteps {
		// "file-top args" is a rewrite stage, not an author-facing top-level
		// construct keyword: it is the bare `args { ... }` block.
		if s.name == "file-top args" {
			continue
		}
		out = append(out, s.name)
	}
	out = append(out, statementConstructKeywords...)
	return out
}()

// NormaliseAll runs every struct-form rewriter in sequence: query,
// mutate, logic, automation, file-top args. Each stage is a no-op
// when the source doesn't match its detector. Errors from any
// stage are wrapped with the stage name and returned immediately.
func NormaliseAll(source string) (string, error) {
	for _, step := range structFormSteps {
		if !step.detect(source) {
			continue
		}
		rewritten, err := step.apply(source)
		if err != nil {
			return "", fmt.Errorf("%s rewrite: %w", step.name, err)
		}
		source = rewritten
	}
	return source, nil
}

// =============================================================================
// Query
// =============================================================================

// queryStructHeader matches the canonical concept-in-signature shape:
//
//	query participant queryFoo { ... }      -- concept binding from signature
//
// Group 1 is the concept name; group 2 is the construct name. The
// legacy single-identifier shape `query queryFoo { ... }` was retired
// in memql#314 -- the regex's optional concept group is preserved so
// a legacy header still gets matched + handed to rewriteEachBlock for
// a clean "missing concept binding" error rather than a silent no-op.
var queryStructHeader = regexp.MustCompile(`(?m)^[ \t]*query[ \t]+(?:([A-Za-z_][A-Za-z0-9_]*)[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

// LooksLikeStructQuery reports whether the source declares a
// struct-form query.
func LooksLikeStructQuery(source string) bool {
	return queryStructHeader.MatchString(source)
}

// NormaliseQuerySource rewrites every `query NAME { ... }` block in
// the source to the procedural `func (Query) NAME(_ any) (any,
// error) { return <expr>, nil }` form.
func NormaliseQuerySource(source string) (string, error) {
	return rewriteEachBlock(source, queryStructHeader, "struct-form query", true, emitQuery)
}

// structQueryBody is the parsed shape of a query body.
type structQueryBody struct {
	filter   string
	shape    string
	count    bool
	argsText string
	sort     string
	paginate string
	asOf     string
	// refine is the lambda of a `refine <lambda>` clause (memql#5364): a
	// predicate evaluated in process over the page paginate reads.
	refine string
	// at is where each clause starts in the body, keyed by its keyword: the
	// author's text a refusal of the clause names (rewrite_errors.go).
	at map[string]int
}

// refuseClauseAt marks err as a refusal of the query's clause kw.
func (q *structQueryBody) refuseClauseAt(kw string, err error) error {
	at, ok := q.at[kw]
	if !ok {
		return refuseClause(kw, err)
	}
	return refuseAtBody(at, len(kw), err)
}

// UnboundedPaginateWindow is the explicit paginate window the rewriter
// injects when a query carries `@unbounded("reason")`. It signals the
// engine that the author has deliberately opted the query out of the
// implicit list-cap backstop (epic 5, issue 5.1 / memql#1965): an
// `@unbounded` query reads the full matching set, so the rewriter wraps
// it in an explicit `paginate(base, <UnboundedPaginateWindow>)`. That
// makes plan.Limit non-nil, which the engine reads as "explicit window
// requested" and therefore skips the default 50-row cap. The engine
// still clamps the realized window to MEMQL_MEMORY_ENGINE_MAX_WINDOW, so this
// is a "give me everything up to the hard ceiling" request, not a way
// to bypass the absolute window ceiling.
const UnboundedPaginateWindow = 1000000

func emitQuery(name, conceptId, body, preamble string) (string, error) {
	// ADR Decision 5: `body { }` is reserved for logic; a query is
	// declarative clauses, never a procedural body block.
	if err := rejectNonLogicBodyBlock("query", name, body); err != nil {
		return "", err
	}
	parsed, err := parseStructQueryBody(body)
	if err != nil {
		return "", err
	}
	if parsed.count && parsed.shape != "" {
		return "", parsed.refuseClauseAt("count", fmt.Errorf("`count` and `shape` are mutually exclusive on a query"))
	}
	if parsed.count && (parsed.sort != "" || parsed.paginate != "") {
		return "", parsed.refuseClauseAt("count", fmt.Errorf("`count` cannot be combined with `sort` or `paginate`"))
	}
	if err := checkRefineClause(parsed); err != nil {
		return "", err
	}

	// `@unbounded("reason")` opt-out (memql#1965). The author has
	// deliberately marked this query as a legitimate full-set read.
	// It is mutually exclusive with the bounding directives -- a query
	// that already paginates / sorts is bounded, not unbounded -- and
	// with count (an aggregate, never a row set). When present we inject
	// an explicit paginate window so the engine's runtime list-cap
	// backstop treats the query as explicitly windowed and does not clamp
	// it to the implicit 50-row default.
	reason, hasUnbounded, err := unboundedReason(preamble)
	if err != nil {
		return "", refuseText("@unbounded", err)
	}
	if hasUnbounded {
		if reason == "" {
			return "", refuseText("@unbounded", fmt.Errorf("`@unbounded` requires a non-empty reason string: @unbounded(\"why this query reads the full set\")"))
		}
		if parsed.paginate != "" || parsed.sort != "" {
			return "", refuseText("@unbounded", fmt.Errorf("`@unbounded` cannot be combined with `paginate` or `sort` -- a paginated/sorted query is already bounded; drop @unbounded or drop the directive"))
		}
		if parsed.count {
			return "", refuseText("@unbounded", fmt.Errorf("`@unbounded` cannot be combined with `count` -- count returns an aggregate, not a row set"))
		}
		parsed.paginate = strconv.Itoa(UnboundedPaginateWindow)
	}

	var sb strings.Builder
	emitFuncHeader(&sb, "Query", name, parsed.argsText, "(any, error)")
	sb.WriteString("  return ")
	sb.WriteString(buildStructQueryExpr(conceptId, parsed.filter, parsed.shape, parsed.sort, parsed.paginate, parsed.asOf, parsed.refine, parsed.count))
	sb.WriteString(", nil\n}")
	return sb.String(), nil
}

// unboundedReasonRe matches `@unbounded("reason")` and captures the
// reason string. The reason is mandatory (memql#1965): every full-set
// read must carry a one-line justification so the audit report can
// enumerate why each bypass is legitimate.
var unboundedReasonRe = regexp.MustCompile(`@unbounded\s*\(\s*"((?:[^"\\]|\\.)*)"\s*\)`)

// unboundedReason inspects a construct's preamble for the
// `@unbounded("reason")` annotation. Returns the captured reason and a
// presence flag.
//
// An @unbounded written in any other form -- bare, a number, a list -- is
// reported ABSENT here, not refused: the annotation line stays in the source,
// and the parser's annotation check refuses its form against the registry
// (memql#5359), which says what @unbounded takes and shows the example. A
// second, text-scanning form check here answered first with an uncoded
// message, so the registry was not the one gate for this annotation.
func unboundedReason(preamble string) (string, bool, error) {
	if m := unboundedReasonRe.FindStringSubmatch(preamble); m != nil {
		return strings.TrimSpace(m[1]), true, nil
	}
	return "", false, nil
}

// checkRefineClause validates a `refine <lambda>` clause (memql#5364), before
// @unbounded can inject a window of its own. refine evaluates in process over
// the page paginate reads, so it needs an AUTHORED paginate: without one the
// "page" is the whole matching set, which is the silent client-side scan the
// pushdown tier exists to refuse. It never combines with count, which reads no
// page at all.
//
// That the clause IS a lambda is the parser's to say (parseRefineFunction):
// the text passes through to it verbatim, and a refusal there lands on the
// author's token, where one here could only name the construct.
func checkRefineClause(q *structQueryBody) error {
	if q.refine == "" {
		return nil
	}
	if q.count {
		return q.refuseClauseAt("refine", fmt.Errorf("`refine` cannot be combined with `count` -- count aggregates in SQL and reads no page for refine to run over"))
	}
	if q.paginate == "" {
		return q.refuseClauseAt("refine", fmt.Errorf("`refine` requires `paginate`: it runs in process over the page paginate reads, and without one that page is the whole matching set -- add `paginate <n>` (refine may then return fewer than n rows)"))
	}
	return nil
}

// joinStructQueryContinuations folds a struct-query body's physical lines
// into logical ones, so a field value may span lines.
//
// The scan below is line-based: it reads the first token of each line as a
// field keyword. That made a multi-line boolean impossible -- this
//
//	filter  when(args.ownerId) { ownerId==args.ownerId } &&
//	        when(args.status) { status==args.status }
//
// failed with `unknown struct-query field on line "when(args.status) { ...`
// while the byte-identical expression on ONE line loaded fine (memql#4123).
// The failure named a "field", so it read as a typo in the author's field
// name rather than as "multi-line values are not supported", which is the
// part that cost time.
//
// A line continues its predecessor when any of three things is true:
//
//   - the accumulated value has an unclosed `(` or `{` -- a `when(...) {`
//     guard or a parenthesised group split across lines;
//   - the accumulated value ends on a dangling binary operator or opener,
//     so it cannot be a complete expression (`... &&`);
//   - the next line OPENS with a binary operator (`&& when(...) { ... }`),
//     which is the other conventional way to break a long boolean.
//
// Anything else starts a new field. A field keyword can therefore never be
// swallowed: `shape spaceFull` neither leaves a delimiter open nor ends on
// an operator, so the `sort` line after it starts fresh.
//
// The rule itself is dslclause.ContinuesClause, and it lives there rather than
// here so every gate that reads a clause as text folds lines exactly as this
// does (epic memql#5363): a gate reading only a filter's first line is blind to
// every conjunct the codemod wrapped onto the lines below it.
//
// Each clause keeps where its first line starts in text, so a refusal of the
// clause can name the author's line and column (rewrite_errors.go).
func joinStructQueryContinuations(text string) []structQueryClause {
	var out []structQueryClause
	for start := 0; start <= len(text); {
		end := len(text)
		if nl := strings.IndexByte(text[start:], '\n'); nl >= 0 {
			end = start + nl
		}
		raw := text[start:end]
		if line := strings.TrimSpace(raw); line != "" {
			if n := len(out); n > 0 && dslclause.ContinuesClause(out[n-1].text, line) {
				out[n-1].text += " " + line
			} else {
				lead := len(raw) - len(strings.TrimLeftFunc(raw, unicode.IsSpace))
				out = append(out, structQueryClause{text: line, at: start + lead})
			}
		}
		if end == len(text) {
			break
		}
		start = end + 1
	}
	return out
}

// structQueryClause is one clause of a struct-query body: its lines joined,
// and the byte offset of its first character in the body.
type structQueryClause struct {
	text string
	at   int
}

// blankKeepingLines is s with every byte but a line break turned to a space:
// text cut out of a body without moving what follows it.
func blankKeepingLines(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c != '\n' && c != '\r' {
			b[i] = ' '
		}
	}
	return string(b)
}

func parseStructQueryBody(body string) (*structQueryBody, error) {
	out := &structQueryBody{at: map[string]int{}}

	// Pull the args block out of the body if present, then iterate
	// the rest line-by-line. Stripping rather than line-skipping
	// keeps the line accounting simple.
	// Locate on the COMMENT-BLANKED view, slice from the original (memql#2948).
	// The header regexp ran on raw source while matchBraceInBody matches on the
	// blanked view, and the two can never agree about a header inside a
	// comment: the header IS in the raw text, its `{` is NOT in the blanked
	// view. So a commented-out `args {` refused the load while naming a block
	// the file does not have. BlankComments is byte- and newline-preserving, so
	// every offset below indexes `body` correctly. #2906 fixed this same shape
	// in the helpers every lane shares; this lane kept its own locator.
	scan := BlankComments(body)

	restScan := scan
	if loc := argsBlockHeader.FindStringIndex(scan); loc != nil {
		openOffset := strings.LastIndex(scan[loc[0]:loc[1]], "{")
		open := loc[0] + openOffset
		close := matchBraceInBody(body, open)
		if close < 0 {
			kw, width := firstWordAt(scan, loc[0])
			return nil, refuseAtBody(kw, width, fmt.Errorf("`args { ... }` block missing closing brace"))
		}
		out.argsText = body[open+1 : close]
		// Blanked, not cut: every clause after the block keeps its offset.
		restScan = scan[:loc[0]] + blankKeepingLines(scan[loc[0]:close+1]) + scan[close+1:]
	}

	// Iterate the BLANKED view. The default arm below rejects any line it does
	// not recognise, and a comment is not a recognised field -- so one
	// explanatory sentence anywhere in a struct-form query failed the load with
	// `unknown struct-query field on line "/*"`. That is a SEPARATE defect from
	// the locator above, and it hits ordinary prose rather than only
	// commented-out constructs.
	//
	// Reading field values from the blanked view rather than the raw line is
	// deliberate and safe: BlankComments is string- and backtick-aware, so a
	// `//` inside a string literal survives untouched, while a trailing comment
	// on a real field (`filter id==args.id // note`) becomes trailing
	// whitespace that TrimSpace removes -- which is what the author meant.
	for _, clause := range joinStructQueryContinuations(restScan) {
		line := clause.text
		kw := ""
		switch {
		case strings.HasPrefix(line, "concept"):
			return nil, refuseAtBody(clause.at, len("concept"), fmt.Errorf("inline `concept` line is no longer supported; declare the concept via a file-top `use <ns>.<concept>` directive instead"))
		case strings.HasPrefix(line, "filter"):
			kw = "filter"
			out.filter = strings.TrimSpace(strings.TrimPrefix(line, "filter"))
		case line == "count":
			kw = "count"
			out.count = true
		case strings.HasPrefix(line, "shape"):
			kw = "shape"
			out.shape = strings.TrimSpace(strings.TrimPrefix(line, "shape"))
		case strings.HasPrefix(line, "sort"):
			kw = "sort"
			out.sort = strings.TrimSpace(strings.TrimPrefix(line, "sort"))
		case strings.HasPrefix(line, "paginate"):
			kw = "paginate"
			out.paginate = strings.TrimSpace(strings.TrimPrefix(line, "paginate"))
		case strings.HasPrefix(line, "asOf"):
			kw = "asOf"
			out.asOf = strings.TrimSpace(strings.TrimPrefix(line, "asOf"))
		case strings.HasPrefix(line, "refine"):
			kw = "refine"
			out.refine = strings.TrimSpace(strings.TrimPrefix(line, "refine"))
		default:
			at, width := firstWordAt(restScan, clause.at)
			return nil, refuseAtBody(at, width, fmt.Errorf("unknown struct-query field on line %q", line))
		}
		out.at[kw] = clause.at
	}
	return out, nil
}

// buildStructQueryExpr stitches the concept / filter / shape +
// sort / paginate / asOf pieces into the runtime expression the
// engine already knows how to compile.
//
// Directive wrapping order (innermost to outermost): asOf -> sort ->
// paginate -> refine -> shape. Matches the order the runtime memql parser
// applies them when these are written as nested function calls in
// a handwritten query string. Each directive's argument is passed
// through VERBATIM -- the author writes the same arg list they
// would use in the runtime form (`sort "createdAt", "desc"` ->
// `sort(base, "createdAt", "desc")`), so the rewriter needs zero
// new arg parsing logic.
//
// memql#294: pre-#294 the sort / paginate / asOf fields on
// structQueryBody were parsed but silently dropped here. That left
// every "give me the latest N rows" query forced into the
// handwritten `shape(paginate(sort(...)))` runtime form, which
// blocked memql#286's migration of cognition's space-context
// callsites away from runtime shape() (memql#288, memql#290).
func buildStructQueryExpr(conceptId, filter, shape, sort, paginate, asOf, refine string, count bool) string {
	base := "concept==" + conceptId
	if filter != "" {
		// Joined with `&&` and parenthesised (memql#5364). The join used to be
		// `;`, which binds at `&&` level, so a filter whose own top level was
		// an `||` split around it -- `concept==X; a || b` is
		// `(concept==X && a) || b`, and every row matching b escaped the
		// concept. The corpus parenthesises its one such filter; the
		// parentheses here make that unnecessary. A v1 filter is a lambda
		// (`row => ...`), and the parentheses are what keep its body, which
		// extends as far as it can, inside the join.
		base += " && (" + filter + ")"
	}
	if asOf != "" {
		base = fmt.Sprintf("asOf(%s, %s)", base, asOf)
	}
	if sort != "" {
		base = fmt.Sprintf("sort(%s, %s)", base, sort)
	}
	if paginate != "" {
		base = fmt.Sprintf("paginate(%s, %s)", base, paginate)
	}
	// refine runs in process over the page paginate read, so it wraps the
	// page and sits inside shape (memql#5364).
	if refine != "" {
		base = fmt.Sprintf("refine(%s, %s)", base, refine)
	}
	// count is the outermost wrapper and mutually exclusive with shape
	// (enforced in emitQuery). It aggregates the matching set to a
	// numeric {count: N} envelope.
	if count {
		return fmt.Sprintf("count(%s)", base)
	}
	if shape != "" {
		return fmt.Sprintf("shape(%s, %q)", base, shape)
	}
	return base
}

// =============================================================================
// Mutation
// =============================================================================

// mutationStructHeader matches the canonical struct-form mutation
// header; mirrors queryStructHeader.
//
// The declaration keyword is `mutate` (the descriptive verb introduced
// by C1 / memql#2041 of the grammar redesign, epic #2031), which maps to
// the internal ReceiverMutation kind. The transitional `mutation` noun
// alias was dropped by C6 (memql#2036) once the .memql tree was swept to
// `mutate`.
var mutationStructHeader = regexp.MustCompile(`(?m)^[ \t]*mutate[ \t]+(?:([A-Za-z_][A-Za-z0-9_]*)[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

// LooksLikeStructMutation reports whether the source declares a
// struct-form mutation.
func LooksLikeStructMutation(source string) bool {
	return mutationStructHeader.MatchString(source)
}

// NormaliseMutationSource rewrites every `mutate NAME { ... }`
// block to the procedural form.
func NormaliseMutationSource(source string) (string, error) {
	return rewriteEachBlock(source, mutationStructHeader, "struct-form mutation", true, emitMutation)
}

// structMutationBody is the parsed shape of a mutation body.
type structMutationBody struct {
	argsText    string
	writeKind   string // "insert" or "update"
	writeBody   string // raw block contents (args.X references pass through verbatim)
	writeTarget string // bare concept name from `<kind> <name> { ... }`
}

func emitMutation(name, conceptId, body, _preamble string) (string, error) {
	// ADR Decision 5: `body { }` is reserved for logic; a mutation is a
	// declarative `insert { ... }` / `update { ... }` block, never a
	// procedural body block.
	if err := rejectNonLogicBodyBlock("mutation", name, body); err != nil {
		return "", err
	}
	parsed, err := parseStructMutationBody(body)
	if err != nil {
		return "", err
	}

	// The bare form (writeTarget == "") derives its target from the
	// signature-bound concept directly. The transitional named form
	// restates the concept's bare name; validate it still matches the
	// binding (last colon-separated segment of the canonical id).
	if parsed.writeTarget != "" {
		expected := conceptId
		if idx := strings.LastIndex(conceptId, ":"); idx >= 0 {
			expected = conceptId[idx+1:]
		}
		if parsed.writeTarget != expected {
			return "", refuseClause(parsed.writeKind, fmt.Errorf("%s target %q does not match the concept binding %q -- drop the restated concept and write the bare `%s { ... }` (the target comes from the `mutate %s <name>` signature)", parsed.writeKind, parsed.writeTarget, expected, parsed.writeKind, expected))
		}
	}

	idExpr, payload, err := translateInsertBody(parsed.writeBody)
	if err != nil {
		err = fmt.Errorf("%s block: %w", parsed.writeKind, err)
		var r *refusal
		if !errors.As(err, &r) {
			err = refuseClause(parsed.writeKind, err)
		}
		return "", err
	}

	var sb strings.Builder
	emitFuncHeader(&sb, "Mutation", name, parsed.argsText, "error")
	switch parsed.writeKind {
	case "insert":
		if idExpr != "" {
			sb.WriteString(fmt.Sprintf("  return insert(%s, id=%s, payload=%s)\n", conceptId, idExpr, payload))
		} else {
			sb.WriteString(fmt.Sprintf("  return insert(%s, %s)\n", conceptId, payload))
		}
	case "update":
		if idExpr == "" {
			return "", refuseClause("update", fmt.Errorf("update block requires an `id: <expr>` line identifying the target row"))
		}
		// Emit the bare concept name as the first positional arg
		// (mirroring insert). The runtime doesn't strictly need it --
		// update looks up by id -- but emitting it keeps the
		// post-rewrite shape symmetric with insert and prevents
		// `function not found` at call time from a silently-dropped
		// load.
		sb.WriteString(fmt.Sprintf("  return update(%s, id=%s, payload=%s)\n", conceptId, idExpr, payload))
	}
	sb.WriteString("}")
	return sb.String(), nil
}

func parseStructMutationBody(body string) (*structMutationBody, error) {
	out := &structMutationBody{}

	argsText, err := extractArgsBlock(body)
	if err != nil {
		return nil, err
	}
	out.argsText = argsText

	// Single-source the block framing (scanMutationBlocks): every decision
	// below reads THIS one comment/string-aware scan, so no two readers can
	// disagree about which blocks exist -- the property that retires the
	// same-line / boundary-anchoring class of bugs the regex approach kept
	// reintroducing.
	blocks, topFields, topFieldSample, topStray, err := scanMutationBlocks(body)
	if err != nil {
		return nil, err
	}

	// C5 (memql#2035) + #2592: the accept/stamp form. `accept { f1, f2 }`
	// lists the public concept fields the mutation accepts from its caller;
	// each auto-binds to its same-named arg (`f1` -> `f1: args.f1`).
	// `stamp { k: v, ... }` carries the server-set fields. Two spellings,
	// differing only in where the WRITE KIND comes from:
	//
	//   bare (#2035)    accept/stamp at the top level -> spells no kind, means
	//                   insert (the original C5 semantics).
	//   nested (#2592)  accept/stamp inside an `insert`/`update` block -> the
	//                   kind is spelled by the enclosing block, which is what
	//                   lets an update adopt the sugar. There is no input where
	//                   the author names a write kind and a different one is
	//                   emitted.
	var writeKind, writeInner string
	writeCount := 0
	var topAccept, topStamp *mutBodyBlock
	var writeBlk *mutBodyBlock
	for i := range blocks {
		b := &blocks[i]
		if b.named != "" {
			// The named form `<kind> <Concept> { ... }` is retired (#988): the
			// write target comes from the `mutate <Concept> <name>` signature.
			return nil, refuseAtBody(b.at, len(b.keyword), fmt.Errorf("`%s %s { ... }` is retired -- drop the restated concept and write the bare `%s { ... }` (the target comes from the `mutate <Concept> <name>` signature)", b.keyword, b.named, b.keyword))
		}
		switch b.keyword {
		case "args":
			// argsText already extracted above.
		case "insert", "update":
			writeCount++
			if writeCount > 1 {
				return nil, refuseAtBody(b.at, len(b.keyword), fmt.Errorf("mutation body must contain exactly one write block -- one write per mutation (authoring-rules rule 1)"))
			}
			writeKind, writeInner, writeBlk = b.keyword, b.inner, b
		case "accept":
			if topAccept != nil {
				return nil, refuseAtBody(b.at, len(b.keyword), fmt.Errorf("mutation body has more than one top-level `accept { ... }` block"))
			}
			topAccept = b
		case "stamp":
			if topStamp != nil {
				return nil, refuseAtBody(b.at, len(b.keyword), fmt.Errorf("mutation body has more than one top-level `stamp { ... }` block"))
			}
			topStamp = b
		default:
			return nil, refuseAtBody(b.at, len(b.keyword), fmt.Errorf("mutation body: unexpected `%s { ... }` block", b.keyword))
		}
	}
	haveWrite := writeCount == 1

	// The mutation top level is blocks only. A bare `key: value` there would be
	// silently dropped by any desugar (and the legacy write form keeps its
	// fields INSIDE the insert/update block), so it is always an error.
	if topFields {
		return nil, refuseText(topFieldSample, fmt.Errorf("mutation body: unexpected field %q at the top level -- put write fields inside the `insert`/`update` block (or a `stamp { ... }` block)", topFieldSample))
	}
	// ...and neither is a line of words: until memql#5359 a top-level
	// `filter x == 1` (or any clause a mutation does not take) was dropped
	// without a word.
	if topStray != "" {
		return nil, fmt.Errorf("mutation body: unexpected %q at the top level -- a mutation body takes %s", topStray, clauseSpellings("mutate"))
	}

	// Nested accept/stamp live inside the write block; scan its inner with the
	// same single source.
	var nestAccept, nestStamp *mutBodyBlock
	if haveWrite {
		wblocks, wFields, wFieldSample, _, werr := scanMutationBlocks(writeInner)
		if werr != nil {
			return nil, shiftRefusal(werr, writeBlk.innerAt)
		}
		for i := range wblocks {
			b := &wblocks[i]
			b.at += writeBlk.innerAt // offsets index the mutation body from here on
			b.innerAt += writeBlk.innerAt
			if b.named != "" {
				return nil, refuseAtBody(b.at, len(b.keyword), fmt.Errorf("`%s { ... }` has a `%s %s { ... }` block -- accept/stamp take no name", writeKind, b.keyword, b.named))
			}
			switch b.keyword {
			case "accept":
				if nestAccept != nil {
					return nil, refuseAtBody(b.at, len(b.keyword), fmt.Errorf("`%s { ... }` has more than one nested `accept { ... }` block", writeKind))
				}
				nestAccept = b
			case "stamp":
				if nestStamp != nil {
					return nil, refuseAtBody(b.at, len(b.keyword), fmt.Errorf("`%s { ... }` has more than one nested `stamp { ... }` block", writeKind))
				}
				nestStamp = b
			default:
				return nil, refuseAtBody(b.at, len(b.keyword), fmt.Errorf("`%s { ... }` may not contain a nested `%s { ... }` block", writeKind, b.keyword))
			}
		}
		// A write block mixing accept/stamp with loose fields would drop those
		// fields on desugar (the body is rebuilt from the blocks alone).
		if (nestAccept != nil || nestStamp != nil) && wFields {
			return nil, refuseText(wFieldSample, fmt.Errorf("`%s { ... }` carries the field %q beside a nested `accept`/`stamp` block -- move server-set fields into `stamp { ... }` (a field left here would be dropped)", writeKind, wFieldSample))
		}
	}

	hasNested := nestAccept != nil || nestStamp != nil
	hasTop := topAccept != nil || topStamp != nil

	top := topAccept
	if top == nil {
		top = topStamp
	}
	switch {
	case hasTop && hasNested:
		// accept/stamp split across the write-block boundary -- one inside, one
		// outside. The desugar reads only the nested pair, so the outer block
		// would be silently dropped (worst case: an empty payload).
		return nil, refuseAtBody(top.at, len(top.keyword), fmt.Errorf("mutation body cannot mix a top-level `accept`/`stamp` with a nested one -- put both inside the `%s { ... }` block", writeKind))
	case hasTop && haveWrite && !hasNested:
		// bare accept/stamp sitting BESIDE an explicit write block.
		return nil, refuseAtBody(top.at, len(top.keyword), fmt.Errorf("mutation body cannot mix the accept/stamp form with an explicit `%s { ... }` block -- nest `accept`/`stamp` inside the write block, or drop the block", writeKind))
	}

	// Resolve the accept/stamp blocks in play and the write kind they emit.
	acceptBlk, stampBlk := topAccept, topStamp
	if hasNested {
		acceptBlk, stampBlk = nestAccept, nestStamp // writeKind stays the enclosing block's
	} else if hasTop {
		writeKind = "insert" // bare form spells no kind -> insert (#2035)
	}

	if acceptBlk != nil || stampBlk != nil {
		argNames := argNamesFromArgsText(out.argsText)
		var lines []string
		if acceptBlk != nil {
			names, perr := parseAcceptNames(acceptBlk.inner)
			if perr != nil {
				return nil, shiftRefusal(perr, acceptBlk.innerAt)
			}
			for _, nm := range names {
				if !argNames[nm] {
					return nil, refuseTextAfter("accept", nm, fmt.Errorf("accept field %q has no matching arg -- declare `%s <type>` in the `args { ... }` block so it can auto-bind to `args.%s`", nm, nm, nm))
				}
				lines = append(lines, nm+": args."+nm)
			}
		}
		if stampBlk != nil {
			if s := strings.TrimSpace(stampBlk.inner); s != "" {
				lines = append(lines, s)
			}
		}
		out.writeKind = writeKind
		out.writeBody = strings.Join(lines, "\n")
		return out, nil
	}

	// No accept/stamp -> the legacy explicit write form, body used verbatim.
	if haveWrite {
		out.writeKind = writeKind
		out.writeBody = writeInner
		return out, nil
	}

	return nil, fmt.Errorf("mutation body must contain exactly one `insert { ... }` or `update { ... }` block (or the `accept { ... }` / `stamp { ... }` form)")
}

// idFieldMatcher matches `id: <expr>` lines inside insert/update bodies.
var idFieldMatcher = regexp.MustCompile(`^id\s*:\s*([\s\S]+)$`)

// idFieldLine finds an `id:` field line in a construct, for a refusal of a
// second one to name it.
var idFieldLine = regexp.MustCompile(`(?m)^[ \t]*(id)[ \t]*:`)

// translateInsertBody converts the struct-form insert/update payload
// from newline-separated `key: value` lines into the legacy
// object-literal form the engine's `insert()` / `update()` accept.
// The `id:` line is hoisted to a positional `id=<expr>` argument and
// dropped from the payload.
//
// Every field it emits is an explicit `key: value` entry: the bare-mirror
// shorthand is expanded here (expandBareMirror), so the payload is a map
// literal in both grammars -- the edition-2026 map refuses a key-less entry.
func translateInsertBody(raw string) (idExpr string, payload string, err error) {
	fields, err := splitInsertFields(raw)
	if err != nil {
		return "", "", err
	}
	var keep []string
	for _, f := range fields {
		if f, err = expandBareMirror(f); err != nil {
			return "", "", err
		}
		if m := idFieldMatcher.FindStringSubmatch(f); m != nil {
			if idExpr != "" {
				return "", "", &refusal{err: fmt.Errorf("duplicate `id:` line in insert body"), at: -1, pattern: idFieldLine, nth: 1}
			}
			idExpr = strings.TrimSpace(m[1])
			continue
		}
		keep = append(keep, f)
	}
	if len(keep) == 0 {
		return idExpr, "{}", nil
	}
	return idExpr, "{ " + strings.Join(keep, ", ") + " }", nil
}

// bareArgsPathRe matches a key-less write-block field that is a dotted
// `args.` path of any depth; bareMirrorRe (acceptstamp_migrate.go) is its
// one-segment case, the only one authoring rule 15 admits.
var bareArgsPathRe = regexp.MustCompile(`^args(?:\.[A-Za-z_][A-Za-z0-9_]*)+$`)

// expandBareMirror expands authoring rule 15's bare-mirror shorthand: a
// write-block line that is only `args.name` means `name: args.name`.
//
// The shorthand is write-block SYNTAX, not an expression, so it is resolved
// here, where the block is still a list of lines, and never reaches an
// expression parser: the edition-2026 map literal has no key-less entry, and
// a key cannot be dotted, so the line would be refused there. Expanding it
// here gives the payload the explicit entry, and gives rule 15's constraint a
// message instead of a vague map-literal failure: a multi-segment path has no
// single key to infer, so `args.user.id` is refused, naming the explicit
// spelling. Any other field passes through untouched.
func expandBareMirror(field string) (string, error) {
	if m := bareMirrorRe.FindStringSubmatch(field); m != nil {
		return m[1] + ": " + field, nil
	}
	if bareArgsPathRe.MatchString(field) {
		key := field[strings.LastIndex(field, ".")+1:]
		return "", refuseText(field, fmt.Errorf("`%s` has no key, and the bare-mirror shorthand takes a single-segment arg (authoring rule 15): write `%s: %s`", field, key, field))
	}
	return field, nil
}

// splitInsertFields walks the raw body and returns each
// `<key>: <value>` field as a single string. Fields separated by
// newlines at brace/paren depth 0; trailing commas tolerated;
// multi-line nested expressions stay glued to their key.
func splitInsertFields(raw string) ([]string, error) {
	var fields []string
	var cur strings.Builder
	depth := 0
	flush := func() {
		s := strings.TrimSpace(cur.String())
		s = strings.TrimSuffix(s, ",")
		if s != "" && !strings.HasPrefix(s, "//") {
			fields = append(fields, s)
		}
		cur.Reset()
	}
	i := 0
	for i < len(raw) {
		c := raw[i]
		switch c {
		case '"', '`':
			// Copy a string literal verbatim: a `//`, `,`, `}` or brace inside
			// it must never be read as a comment, separator, or nesting -- e.g.
			// `source: "https://cdn/x"` must not truncate at the `//` and drop
			// the fields after it.
			end := skipStringLiteral(raw, i)
			cur.WriteString(raw[i : end+1])
			i = end + 1
			continue
		case '{', '[', '(':
			depth++
			cur.WriteByte(c)
		case '}', ']', ')':
			depth--
			cur.WriteByte(c)
		case '/':
			if i+1 < len(raw) && raw[i+1] == '/' && depth == 0 {
				for i < len(raw) && raw[i] != '\n' {
					i++
				}
				continue
			}
			cur.WriteByte(c)
		case '\n':
			if depth == 0 {
				flush()
			} else {
				cur.WriteByte(' ')
			}
		case ',':
			if depth == 0 {
				flush()
			} else {
				cur.WriteByte(c)
			}
		default:
			cur.WriteByte(c)
		}
		i++
	}
	flush()
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced braces / parens in insert body")
	}
	return fields, nil
}

// =============================================================================
// Clause spellings
// =============================================================================

// clauseSpellings lists a construct's clauses the way they are written:
// "`args { }`, `insert { }` and `update { }`".
func clauseSpellings(keyword string) string {
	clauses := BodyClauses(keyword)
	out := make([]string, 0, len(clauses))
	for _, c := range clauses {
		if IsNamedBlock(c) {
			out = append(out, "`"+c+" <name> { }`")
		} else {
			out = append(out, "`"+c+" { }`")
		}
	}
	if len(out) <= 1 {
		return strings.Join(out, "")
	}
	return strings.Join(out[:len(out)-1], ", ") + " and " + out[len(out)-1]
}

// =============================================================================
// File-top args
// =============================================================================

// fileTopArgsHeader matches a file-level `args { ... }` declaration.
var fileTopArgsHeader = regexp.MustCompile(`(?m)^[ \t]*args[ \t]*\{`)

// LooksLikeFileTopArgs reports whether the source declares a
// file-level `args { ... }` block (outside any struct construct).
func LooksLikeFileTopArgs(source string) bool {
	loc := fileTopArgsHeader.FindStringIndex(source)
	if loc == nil {
		return false
	}
	if isInsideStructConstructHeader(source, loc[0]) {
		return false
	}
	return true
}

// NormaliseFileTopArgs is a no-op. It once translated `args.X`
// references to `ctx.X` inside the function body that followed each
// file-top `args { ... }` block; the engine parser learned `args.X`
// natively in F.3 of the ctx-envelope purge. The function stays in
// the NormaliseAll chain only to preserve the structural-detection
// callsite shape (LooksLikeFileTopArgs gates whether the chain runs)
// without churning callers.
func NormaliseFileTopArgs(source string) (string, error) {
	return source, nil
}

// isInsideStructConstructHeader returns true when `argsLoc` falls
// inside the body of a struct-form construct.
// Accepts both `<kind> <name> {` and the post-migration
// `<kind> <Concept> <name> {` (signature-bound concept; PR #48/49).
// `mutate` is the C1 (memql#2041) verb alias for `mutation`.
var structConstructHeaderForArgs = regexp.MustCompile(`(?m)^[ \t]*(query|mutate)[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?[A-Za-z_][A-Za-z0-9_]*[ \t]*\{`)

func isInsideStructConstructHeader(source string, argsLoc int) bool {
	for _, m := range structConstructHeaderForArgs.FindAllStringIndex(source[:argsLoc], -1) {
		openBrace := m[1] - 1
		close := findMatchingCloseBrace(source, openBrace)
		if close < 0 {
			continue
		}
		if close > argsLoc {
			return true
		}
	}
	return false
}
