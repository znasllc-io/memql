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
	"fmt"
	"regexp"
	"strconv"
	"strings"
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

		openIdx := h[1] - 1
		closeIdx := findMatchingCloseBrace(out, openIdx)
		if closeIdx < 0 {
			return "", fmt.Errorf("%s: missing closing brace", kindLabel)
		}

		headerLine := out[h[0]:h[1]]
		nameMatch := header.FindStringSubmatch(headerLine)
		if len(nameMatch) < 2 {
			return "", fmt.Errorf("%s: could not extract name", kindLabel)
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
				return "", fmt.Errorf("%s %q: missing concept binding -- declare via the signature form `<kind> <Concept> <name> { ... }`", kindLabel, name)
			}
			conceptId = signatureConcept
		}

		body := out[openIdx+1 : closeIdx]
		preamble := precedingAnnotationBlock(out, h[0])
		rewritten, err := emit(name, conceptId, body, preamble)
		if err != nil {
			return "", fmt.Errorf("%s %q: %w", kindLabel, name, err)
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
	loc := argsBlockHeader.FindStringIndex(body)
	if loc == nil {
		return "", nil
	}
	openOffset := strings.LastIndex(body[loc[0]:loc[1]], "{")
	open := loc[0] + openOffset
	close := findMatchingCloseBrace(body, open)
	if close < 0 {
		return "", fmt.Errorf("`args { ... }` block missing closing brace")
	}
	return body[open+1 : close], nil
}

// acceptBlockHeader matches the C5 (memql#2035) `accept { ... }` block
// -- a comma/newline-separated list of public concept field NAMES a
// mutation accepts from its caller. Each name auto-binds to the
// same-named arg (`name` -> `name: args.name`), dropping the
// `insert { id: args.id, name: args.name, ... }` boilerplate.
var acceptBlockHeader = regexp.MustCompile(`(^|[\n\r])[ \t]*accept[ \t]*\{`)

// stampBlockHeader matches the C5 `stamp { ... }` block -- the
// server-set fields (key: value lines, same syntax as insert) the
// mutation writes from server context (now / actor.X / literals),
// NOT from caller args. Typically the concept's @serverSet fields
// (createdAt / createdBy / status).
var stampBlockHeader = regexp.MustCompile(`(^|[\n\r])[ \t]*stamp[ \t]*\{`)

// extractBraceBlock pulls the inner text of the first `<header> { ... }`
// block matched by re. Returns ("", false, nil) when absent.
func extractBraceBlock(body string, re *regexp.Regexp, keyword string) (string, bool, error) {
	loc := re.FindStringIndex(body)
	if loc == nil {
		return "", false, nil
	}
	open := loc[0] + strings.Index(body[loc[0]:loc[1]], "{")
	close := findMatchingCloseBrace(body, open)
	if close < 0 {
		return "", false, fmt.Errorf("`%s { ... }` block missing closing brace", keyword)
	}
	return body[open+1 : close], true, nil
}

// acceptFieldName matches a single bare identifier (a field name) in an
// `accept { ... }` list.
var acceptFieldName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseAcceptNames splits an `accept { ... }` block body into field
// names. Entries are separated by commas and/or newlines; `//` line
// comments and blank entries are ignored. Every entry must be a bare
// identifier -- an `accept` list carries field NAMES, never `key:
// value` pairs (those belong in `stamp`).
func parseAcceptNames(raw string) ([]string, error) {
	var names []string
	for _, line := range strings.Split(raw, "\n") {
		// Strip line comments.
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		for _, tok := range strings.Split(line, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			if strings.Contains(tok, ":") {
				return nil, fmt.Errorf("accept block entry %q looks like a `key: value` pair -- the `accept { ... }` list carries public field NAMES only (auto-bound from same-named args); put server-set `key: value` fields in `stamp { ... }`", tok)
			}
			if !acceptFieldName.MatchString(tok) {
				return nil, fmt.Errorf("accept block entry %q is not a valid field name", tok)
			}
			names = append(names, tok)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("`accept { ... }` block is empty -- list at least one public field name, or drop the block")
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

// legacyProceduralAuthorForm matches `func (Receiver) name(...)` at
// column 0. Pinned to the 12 receiver names the engine has ever
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
var legacyProceduralAuthorForm = regexp.MustCompile(`(?m)^func \((Query|Mutation|Logic|Spec|Automation|Builtin|Prompt|Provider|Shape|Tool|Policy|Seed)\)`)

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
	m := legacyProceduralAuthorForm.FindStringSubmatch(BlankComments(source))
	if m == nil {
		return nil
	}
	return fmt.Errorf("legacy procedural form `func (%s) ...` is retired (memql#303) -- author every construct in struct form: `<kind> <Concept> <name> { args { ... } ... }`", m[1])
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
	{"mutation", LooksLikeStructMutation, NormaliseMutationSource},
	{"logic", LooksLikeStructLogic, NormaliseLogicSource},
	// Terse single-step automation lowering (memql#2215) MUST run before
	// the struct-form automation stage: it expands `automation NAME
	// @trigger(...) => logic L` into the canonical longhand block that
	// NormaliseAutomationSource then compiles. Excluded from
	// StructFormKeywords below (it is the same `automation` keyword, not
	// a new construct).
	{"terse automation", LooksLikeTerseAutomation, NormaliseTerseAutomationSource},
	{"automation", LooksLikeStructAutomation, NormaliseAutomationSource},
	{"file-top args", LooksLikeFileTopArgs, NormaliseFileTopArgs},
}

// StructFormKeywords is the set of author-facing construct keywords the
// struct-form rewriter recognises and expands to the internal func
// form, in declaration order: query / mutation / logic / automation.
// It is the single source the #2124 drift test compares dslspec's
// "function" category constructs against. Derived from structFormSteps
// (excluding the non-construct "file-top args" stage) so the list
// cannot drift from the actual rewriter chain.
var StructFormKeywords = func() []string {
	out := make([]string, 0, len(structFormSteps))
	for _, s := range structFormSteps {
		// "file-top args" and "terse automation" are rewrite stages, not
		// author-facing top-level construct keywords: the former is the
		// bare `args { ... }` block, the latter is sugar over the same
		// `automation` keyword. Neither contributes a new keyword to the
		// recognised construct set (#2124 drift test compares this list
		// against dslspec's "function" category constructs).
		if s.name == "file-top args" || s.name == "terse automation" {
			continue
		}
		out = append(out, s.name)
	}
	return out
}()

// NormaliseAll runs every struct-form rewriter in sequence: query,
// mutation, logic, automation, file-top args. Each stage is a no-op
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
}

// UnboundedPaginateWindow is the explicit paginate window the rewriter
// injects when a query carries `@unbounded("reason")`. It signals the
// engine that the author has deliberately opted the query out of the
// implicit list-cap backstop (epic 5, issue 5.1 / memql#1965): an
// `@unbounded` query reads the full matching set, so the rewriter wraps
// it in an explicit `paginate(base, <UnboundedPaginateWindow>)`. That
// makes plan.Limit non-nil, which the engine reads as "explicit window
// requested" and therefore skips the default 50-row cap. The engine
// still clamps the realized window to MEMORY_ENGINE_MAX_WINDOW, so this
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
		return "", fmt.Errorf("`count` and `shape` are mutually exclusive on a query")
	}
	if parsed.count && (parsed.sort != "" || parsed.paginate != "") {
		return "", fmt.Errorf("`count` cannot be combined with `sort` or `paginate`")
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
		return "", err
	}
	if hasUnbounded {
		if reason == "" {
			return "", fmt.Errorf("`@unbounded` requires a non-empty reason string: @unbounded(\"why this query reads the full set\")")
		}
		if parsed.paginate != "" || parsed.sort != "" {
			return "", fmt.Errorf("`@unbounded` cannot be combined with `paginate` or `sort` -- a paginated/sorted query is already bounded; drop @unbounded or drop the directive")
		}
		if parsed.count {
			return "", fmt.Errorf("`@unbounded` cannot be combined with `count` -- count returns an aggregate, not a row set")
		}
		parsed.paginate = strconv.Itoa(UnboundedPaginateWindow)
	}

	var sb strings.Builder
	emitFuncHeader(&sb, "Query", name, parsed.argsText, "(any, error)")
	sb.WriteString("  return ")
	sb.WriteString(buildStructQueryExpr(conceptId, parsed.filter, parsed.shape, parsed.sort, parsed.paginate, parsed.asOf, parsed.count))
	sb.WriteString(", nil\n}")
	return sb.String(), nil
}

// unboundedReasonRe matches `@unbounded("reason")` and captures the
// reason string. The reason is mandatory (memql#1965): every full-set
// read must carry a one-line justification so the audit report can
// enumerate why each bypass is legitimate.
var unboundedReasonRe = regexp.MustCompile(`@unbounded\s*\(\s*"((?:[^"\\]|\\.)*)"\s*\)`)

// unboundedBareRe matches a bare `@unbounded` (no argument list), which
// is rejected -- the reason is required.
var unboundedBareRe = regexp.MustCompile(`@unbounded\b`)

// unboundedReason inspects a construct's preamble for the
// `@unbounded("reason")` annotation. Returns the captured reason, a
// presence flag, and an error if the annotation is present but
// malformed (bare, or empty/whitespace reason).
func unboundedReason(preamble string) (string, bool, error) {
	if m := unboundedReasonRe.FindStringSubmatch(preamble); m != nil {
		return strings.TrimSpace(m[1]), true, nil
	}
	if unboundedBareRe.MatchString(preamble) {
		return "", true, fmt.Errorf("`@unbounded` requires a reason string: @unbounded(\"why this query reads the full set\")")
	}
	return "", false, nil
}

func parseStructQueryBody(body string) (*structQueryBody, error) {
	out := &structQueryBody{}

	// Pull the args block out of the body if present, then iterate
	// the rest line-by-line. Stripping rather than line-skipping
	// keeps the line accounting simple.
	rest := body
	if loc := argsBlockHeader.FindStringIndex(body); loc != nil {
		openOffset := strings.LastIndex(body[loc[0]:loc[1]], "{")
		open := loc[0] + openOffset
		close := findMatchingCloseBrace(body, open)
		if close < 0 {
			return nil, fmt.Errorf("`args { ... }` block missing closing brace")
		}
		out.argsText = body[open+1 : close]
		rest = body[:loc[0]] + body[close+1:]
	}

	for _, raw := range strings.Split(rest, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "concept"):
			return nil, fmt.Errorf("inline `concept` line is no longer supported; declare the concept via a file-top `use <ns>.<concept>` directive instead")
		case strings.HasPrefix(line, "filter"):
			out.filter = strings.TrimSpace(strings.TrimPrefix(line, "filter"))
		case line == "count":
			out.count = true
		case strings.HasPrefix(line, "shape"):
			out.shape = strings.TrimSpace(strings.TrimPrefix(line, "shape"))
		case strings.HasPrefix(line, "sort"):
			out.sort = strings.TrimSpace(strings.TrimPrefix(line, "sort"))
		case strings.HasPrefix(line, "paginate"):
			out.paginate = strings.TrimSpace(strings.TrimPrefix(line, "paginate"))
		case strings.HasPrefix(line, "asOf"):
			out.asOf = strings.TrimSpace(strings.TrimPrefix(line, "asOf"))
		default:
			return nil, fmt.Errorf("unknown struct-query field on line %q", line)
		}
	}
	return out, nil
}

// buildStructQueryExpr stitches the concept / filter / shape +
// sort / paginate / asOf pieces into the runtime expression the
// engine already knows how to compile.
//
// Directive wrapping order (innermost to outermost): asOf -> sort ->
// paginate -> shape. Matches the order the runtime memql parser
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
func buildStructQueryExpr(conceptId, filter, shape, sort, paginate, asOf string, count bool) string {
	base := "concept==" + conceptId
	if filter != "" {
		base += ";" + filter
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
			return "", fmt.Errorf("%s target %q does not match the concept binding %q -- drop the restated concept and write the bare `%s { ... }` (the target comes from the `mutation %s <name>` signature)", parsed.writeKind, parsed.writeTarget, expected, parsed.writeKind, expected)
		}
	}

	idExpr, payload, err := translateInsertBody(parsed.writeBody)
	if err != nil {
		return "", fmt.Errorf("%s block: %w", parsed.writeKind, err)
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
			return "", fmt.Errorf("update block requires an `id: <expr>` line identifying the target row")
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

	// Scan a write block. The canonical form is bare `<keyword> { ... }`
	// (#986) -- the write target is the concept bound by the mutation's
	// `mutation <Concept> <name>` signature, so restating it in the body
	// is redundant. The named form `<keyword> <conceptName> { ... }` is
	// still accepted transitionally (the name is validated against the
	// signature); its tree-wide migration to the bare form + hard
	// rejection is the codemod child #988.
	scanWrite := func(keyword string) (name, raw string, found bool, err error) {
		// Named form `<keyword> <conceptName> { ... }` is retired (#988) --
		// the write target comes from the `mutation <Concept> <name>`
		// signature, so restating it in the body is rejected.
		named := regexp.MustCompile(`(^|[\n\r])[ \t]*` + keyword + `[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
		if loc := named.FindStringSubmatchIndex(body); loc != nil {
			return "", "", false, fmt.Errorf("`%s %s { ... }` is retired -- drop the restated concept and write the bare `%s { ... }` (the target comes from the `mutation <Concept> <name>` signature)", keyword, body[loc[4]:loc[5]], keyword)
		}
		// Bare form (canonical): `<keyword> { ... }`. Target derived from
		// the signature; name is left empty to signal "derive".
		bare := regexp.MustCompile(`(^|[\n\r])[ \t]*` + keyword + `[ \t]*\{`)
		if loc := bare.FindStringIndex(body); loc != nil {
			open := loc[0] + strings.Index(body[loc[0]:loc[1]], "{")
			close := findMatchingCloseBrace(body, open)
			if close < 0 {
				return "", "", false, fmt.Errorf("`%s { ... }` block missing closing brace", keyword)
			}
			return "", body[open+1 : close], true, nil
		}
		return "", "", false, nil
	}

	insertName, insertRaw, hasInsert, err := scanWrite("insert")
	if err != nil {
		return nil, err
	}
	updateName, updateRaw, hasUpdate, err := scanWrite("update")
	if err != nil {
		return nil, err
	}

	// C5 (memql#2035): the accept/stamp form. `accept { f1, f2 }` lists
	// the public concept fields the mutation accepts from its caller;
	// each auto-binds to its same-named arg (`f1` -> `f1: args.f1`).
	// `stamp { k: v, ... }` carries the server-set fields. The two
	// desugar into a synthetic `insert { ... }` body, so everything
	// downstream (id hoist, payload translation, the load-time concept
	// gate) is unchanged. The whether-a-field-is-actually-public check
	// is concept-aware and happens at load time (function_loader);
	// here we only desugar + validate the auto-bind has a matching arg.
	acceptRaw, hasAccept, err := extractBraceBlock(body, acceptBlockHeader, "accept")
	if err != nil {
		return nil, err
	}
	stampRaw, hasStamp, err := extractBraceBlock(body, stampBlockHeader, "stamp")
	if err != nil {
		return nil, err
	}

	if hasAccept || hasStamp {
		if hasInsert || hasUpdate {
			return nil, fmt.Errorf("mutation body cannot mix the accept/stamp form with an explicit `insert { ... }` / `update { ... }` block -- pick one")
		}
		argNames := argNamesFromArgsText(out.argsText)
		var lines []string
		if hasAccept {
			names, perr := parseAcceptNames(acceptRaw)
			if perr != nil {
				return nil, perr
			}
			for _, n := range names {
				if !argNames[n] {
					return nil, fmt.Errorf("accept field %q has no matching arg -- declare `%s <type>` in the `args { ... }` block so it can auto-bind to `args.%s`", n, n, n)
				}
				lines = append(lines, n+": args."+n)
			}
		}
		if hasStamp {
			if strings.TrimSpace(stampRaw) != "" {
				lines = append(lines, stampRaw)
			}
		}
		out.writeKind = "insert"
		out.writeBody = strings.Join(lines, "\n")
		return out, nil
	}

	switch {
	case hasInsert && hasUpdate:
		return nil, fmt.Errorf("mutation body cannot mix an `insert { ... }` and an `update { ... }` block")
	case hasInsert:
		out.writeKind = "insert"
		out.writeBody = insertRaw
		out.writeTarget = insertName
	case hasUpdate:
		out.writeKind = "update"
		out.writeBody = updateRaw
		out.writeTarget = updateName
	default:
		return nil, fmt.Errorf("mutation body must contain exactly one `insert { ... }` or `update { ... }` block (or the `accept { ... }` / `stamp { ... }` form)")
	}
	return out, nil
}

// idFieldMatcher matches `id: <expr>` lines inside insert/update bodies.
var idFieldMatcher = regexp.MustCompile(`^id\s*:\s*([\s\S]+)$`)

// translateInsertBody converts the struct-form insert/update payload
// from newline-separated `key: value` lines into the legacy
// object-literal form the engine's `insert()` / `update()` accept.
// The `id:` line is hoisted to a positional `id=<expr>` argument and
// dropped from the payload.
func translateInsertBody(raw string) (idExpr string, payload string, err error) {
	fields, err := splitInsertFields(raw)
	if err != nil {
		return "", "", err
	}
	var keep []string
	for _, f := range fields {
		if m := idFieldMatcher.FindStringSubmatch(f); m != nil {
			if idExpr != "" {
				return "", "", fmt.Errorf("duplicate `id:` line in insert body")
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
// Logic
// =============================================================================

var logicStructHeader = regexp.MustCompile(`(?m)^logic[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

// LooksLikeStructLogic reports whether the source declares a
// struct-form logic block.
func LooksLikeStructLogic(source string) bool {
	return logicStructHeader.MatchString(source)
}

// NormaliseLogicSource rewrites every `logic NAME { ... }` block to
// the procedural `func (Logic) NAME(_ any) (any, error) { ... }`
// form. The body keeps its own trailing `return <expr>`; the
// rewriter no longer appends `return ctx, nil`.
func NormaliseLogicSource(source string) (string, error) {
	return rewriteEachBlock(source, logicStructHeader, "struct-form logic", false, emitLogic)
}

var logicBodyBlockHeader = regexp.MustCompile(`(^|[\n\r])[ \t]*body[ \t]*\{`)

func emitLogic(name, _conceptId, body, _preamble string) (string, error) {
	argsText, err := extractArgsBlock(body)
	if err != nil {
		return "", err
	}

	loc := logicBodyBlockHeader.FindStringIndex(body)
	if loc == nil {
		// ADR Decision 5: `body { }` is mandatory on logic (always, even
		// one-liners). Its absence is a hard parse error.
		return "", fmt.Errorf("logic %q must wrap its procedural code in a `body { }` block (mandatory on logic; ADR Decision 5)", name)
	}
	openOffset := strings.LastIndex(body[loc[0]:loc[1]], "{")
	open := loc[0] + openOffset
	close := findMatchingCloseBrace(body, open)
	if close < 0 {
		return "", fmt.Errorf("`body { ... }` block missing closing brace")
	}
	bodyText := body[open+1 : close]

	var sb strings.Builder
	emitFuncHeader(&sb, "Logic", name, argsText, "(any, error)")
	sb.WriteString(bodyText)
	if !containsTrailingReturn(bodyText) {
		return "", fmt.Errorf("logic %q body must end with a `return <expr>` terminator", name)
	}
	sb.WriteString("}")
	return sb.String(), nil
}

// containsTrailingReturn reports whether the last non-blank,
// non-comment statement in `body` is a `return ...` line.
func containsTrailingReturn(body string) bool {
	lines := strings.Split(body, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "//") || line == "}" {
			continue
		}
		return strings.HasPrefix(line, "return")
	}
	return false
}

// =============================================================================
// Automation
// =============================================================================

var automationStructHeader = regexp.MustCompile(`(?m)^automation[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
var stepBlockHeader = regexp.MustCompile(`(?m)^[ \t]*step[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
var legacyAutomationHeader = regexp.MustCompile(`(?m)^func\s*\(\s*Automation\s*\)`)

// LooksLikeStructAutomation reports whether the source declares a
// struct-form automation.
func LooksLikeStructAutomation(source string) bool {
	return automationStructHeader.MatchString(source)
}

// LooksLikeLegacyAutomation reports whether the source declares the
// retired procedural `func (Automation) NAME(...)` form. Author
// files containing this form are rejected at parse time -- the
// struct form is canonical.
func LooksLikeLegacyAutomation(source string) bool {
	return legacyAutomationHeader.MatchString(source)
}

// NormaliseAutomationSource rewrites every `automation NAME { step
// <name> { ... } }` block to a procedural function whose body
// assigns each step's call to a named variable.
func NormaliseAutomationSource(source string) (string, error) {
	return rewriteEachBlock(source, automationStructHeader, "struct-form automation", false, emitAutomation)
}

// terseAutomationHeader matches the terse single-step automation form
// (ADR §2.4 / §7, memql#2215):
//
//	automation NAME @trigger(event="...")    => logic logicName
//	automation NAME @trigger(schedule="...")  => logic logicName
//
// It kills the pass-through ceremony (an automation whose entire body
// was `step run { logic X { event: event } }`) without hiding the
// reactive surface: the `@trigger` stays greppable on the declaration
// line. Group 1 is leading indentation, group 2 the automation name,
// group 3 the whole `@trigger(...)` annotation, group 4 the target
// logic name. The `@trigger` arg list carries no nested parens, so a
// `[^)]*` capture is exact.
var terseAutomationHeader = regexp.MustCompile(`(?m)^([ \t]*)automation[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+(@trigger\([^)]*\))[ \t]*=>[ \t]*logic[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*$`)

// LooksLikeTerseAutomation reports whether the source declares a terse
// single-step automation (the `=> logic X` arrow form).
func LooksLikeTerseAutomation(source string) bool {
	return terseAutomationHeader.MatchString(source)
}

// NormaliseTerseAutomationSource lowers every terse single-step
// automation to the canonical longhand struct form, hoisting the inline
// `@trigger(...)` to its own annotation line above the declaration:
//
//	@trigger(event="system.startup")
//	automation registerNode {
//	  step run {
//	    logic registerNode { event: event }
//	  }
//	}
//
// The longhand it emits is byte-for-byte the shape the four existing
// cluster pass-throughs already use, so the subsequent
// NormaliseAutomationSource stage compiles the terse and the longhand
// forms to an IDENTICAL runtime automation -- there is no separate
// terse execution path, hence no dry-run/live divergence to babysit.
//
// The synthesized step always forwards the bound trigger payload as
// `event`. A target logic that declares no args simply ignores the
// unbound value (undeclared args are dropped at invocation), so the
// "forward event when the logic takes args, empty otherwise" contract
// holds without the rewriter needing cross-file arg visibility.
func NormaliseTerseAutomationSource(source string) (string, error) {
	return terseAutomationHeader.ReplaceAllString(
		source,
		"${1}${3}\n${1}automation ${2} {\n${1}  step run {\n${1}    logic ${4} { event: event }\n${1}  }\n${1}}",
	), nil
}

// automationStep is the parsed shape of a single step.
type automationStep struct {
	name string
	call string
	// raw marks a step whose `call` is a complete top-level procedural
	// statement (e.g. a `for item := range ... { ... }` loop) that must be
	// emitted verbatim into the automation body rather than as a
	// `<name> := <call>` assignment. The forEach step (memql#2246) is the
	// only producer today; the for-range statement is not an assignment
	// expression, so parseGoStyleStep cannot consume it after `:=`.
	raw bool
}

func emitAutomation(name, _conceptId, body, _preamble string) (string, error) {
	// ADR Decision 5: `body { }` is reserved for logic; an automation is a
	// sequence of `step ...` blocks, never a procedural body block. Catch a
	// wrapping `body { ... }` before parseAutomationSteps silently reaches
	// through it to the nested steps.
	if err := rejectNonLogicBodyBlock("automation", name, body); err != nil {
		return "", err
	}
	steps, err := parseAutomationSteps(body)
	if err != nil {
		return "", err
	}
	if len(steps) == 0 {
		return "", fmt.Errorf("at least one `step` is required")
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("func (Automation) %s(_ any) {\n", name))
	for _, s := range steps {
		sb.WriteString("  ")
		if s.raw {
			// A raw step (forEach loop) is a complete top-level statement.
			sb.WriteString(s.call)
		} else {
			sb.WriteString(s.name)
			sb.WriteString(" := ")
			sb.WriteString(s.call)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("}")
	return sb.String(), nil
}

func parseAutomationSteps(body string) ([]automationStep, error) {
	var out []automationStep
	pos := 0
	for pos < len(body) {
		loc := stepBlockHeader.FindStringIndex(body[pos:])
		if loc == nil {
			break
		}
		stepStart := pos + loc[0]
		stepHeaderEnd := pos + loc[1]
		header := body[stepStart:stepHeaderEnd]
		nameMatch := stepBlockHeader.FindStringSubmatch(header)
		if len(nameMatch) < 2 {
			return nil, fmt.Errorf("step block: missing name")
		}
		stepName := nameMatch[1]
		openIdx := stepHeaderEnd - 1
		closeIdx := findMatchingCloseBrace(body, openIdx)
		if closeIdx < 0 {
			return nil, fmt.Errorf("step %q: missing closing brace", stepName)
		}
		stepBody := strings.TrimSpace(body[openIdx+1 : closeIdx])
		if stepBody == "" {
			return nil, fmt.Errorf("step %q: body is empty", stepName)
		}
		// A `forEach`/`for` step body lowers to a top-level for-range loop
		// statement (StepTypeForEach), not a `<name> := <call>` assignment
		// (memql#2246). A `switch` step body lowers to a top-level switch
		// statement (StepTypeSwitch), likewise emitted raw (epic #2212, I10
		// #2224). Everything else is a single call expression.
		lead, _ := splitLeadingIdent(stepBody)
		if lead == "forEach" || lead == "for" {
			stmt, err := translateForEachStepCall(stepName, stepBody)
			if err != nil {
				return nil, fmt.Errorf("step %q: %w", stepName, err)
			}
			out = append(out, automationStep{name: stepName, call: stmt, raw: true})
			pos = closeIdx + 1
			continue
		}
		if lead == "switch" {
			stmt, err := translateSwitchStepCall(stepName, stepBody)
			if err != nil {
				return nil, fmt.Errorf("step %q: %w", stepName, err)
			}
			out = append(out, automationStep{name: stepName, call: stmt, raw: true})
			pos = closeIdx + 1
			continue
		}
		call, err := translateStepCall(stepBody)
		if err != nil {
			return nil, fmt.Errorf("step %q: %w", stepName, err)
		}
		out = append(out, automationStep{name: stepName, call: call})
		pos = closeIdx + 1
	}
	return out, nil
}

// translateStepCall converts a step body into the legacy call
// expression. Supported shapes:
//
//	`<kind> <bareName> { <args> }` -- kind is logic / mutate /
//	  query / automation; produces `<bareName>({ args })` (post-C6 the
//	  construct name is bare; the kind keyword only selects the slot).
//	`publishEvent { <args> }`      -- builtin; `publishEvent({ args })`.
//	`<prefixedName> { <args> }`    -- direct form.
//	`<name>(<args>)`               -- already parenthesised; passthrough.
//	`if <cond> { <call> }`         -- conditional step (memql#1366): the
//	  inner call is translated recursively and the form passes through as
//	  `<step> := if <cond> { <translatedCall> }`, which the procedural
//	  parser's parseGoStyleStep already accepts (it stamps the condition
//	  on StepDef.Condition; the executor skip-gates the step on it).
//	`parallel { ... }`             -- concurrent fan-out step (memql#1368):
//	  a config-key block (`wait`, `failFast`, `branches`) whose branches
//	  are full `step <name> { ... }` blocks, each translated recursively.
//	  Emitted as `<step> := parallel { wait: ..., failFast: ...,
//	  branches: [ <name> := <call>, ... ] }`, which parseGoStyleStep
//	  parses into StepTypeParallel.
func translateStepCall(body string) (string, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", fmt.Errorf("call expression is empty")
	}
	first, rest := splitLeadingIdent(body)
	if first == "" {
		return "", fmt.Errorf("expected identifier at start of call, got %q", body)
	}

	if first == "if" {
		return translateConditionalStepCall(rest)
	}

	if first == "parallel" {
		return translateParallelStepCall(rest)
	}

	if first == "action" {
		return translateActionStepCall(rest)
	}

	if kindPrefix(first) {
		bare, after := splitLeadingIdent(strings.TrimLeft(rest, " \t\n\r"))
		if bare == "" {
			return "", fmt.Errorf("expected name after `%s` keyword", first)
		}
		// Post-C6 (memql#2036): construct names are bare. The logic / query
		// / mutate kinds resolve by bare name directly -- no prefix promotion.
		// `automation` is the exception: it keeps an internal
		// `automation<PascalCase>` marker so the compiler emits a
		// StepTypeAutomation sub-dispatch (automation_generator.go strips the
		// marker back to the bare sub-automation name).
		name := bare
		if first == "automation" {
			name = first + strings.ToUpper(bare[:1]) + bare[1:]
		}
		return finishCall(name, strings.TrimLeft(after, " \t\n\r"))
	}

	return finishCall(first, strings.TrimLeft(rest, " \t\n\r"))
}

// translateConditionalStepCall handles the `if <cond> { <call> }` step body
// (memql#1366). rest is everything after the leading `if` ident: the condition
// expression followed by the braced inner call. The condition is an
// expression (no braces of its own), so the first `{` opens the if body; the
// inner call is translated through translateStepCall recursively, which also
// permits nesting in principle though one level is the authored norm.
func translateConditionalStepCall(rest string) (string, error) {
	rest = strings.TrimLeft(rest, " \t\n\r")
	braceIdx := strings.IndexByte(rest, '{')
	if braceIdx < 0 {
		return "", fmt.Errorf("conditional step: expected `{` after the if condition")
	}
	cond := strings.TrimSpace(rest[:braceIdx])
	if cond == "" {
		return "", fmt.Errorf("conditional step: missing condition between `if` and `{`")
	}
	closeIdx := findMatchingCloseBrace(rest, braceIdx)
	if closeIdx < 0 {
		return "", fmt.Errorf("conditional step: missing closing brace for the if body")
	}
	if tail := strings.TrimSpace(rest[closeIdx+1:]); tail != "" {
		return "", fmt.Errorf("conditional step: unexpected trailing text after the if body: %q", tail)
	}
	inner := strings.TrimSpace(rest[braceIdx+1 : closeIdx])
	if inner == "" {
		return "", fmt.Errorf("conditional step: the if body is empty")
	}
	call, err := translateStepCall(inner)
	if err != nil {
		return "", fmt.Errorf("conditional step body: %w", err)
	}
	return "if " + cond + " { " + call + " }", nil
}

// translateForEachStepCall handles the per-item iteration step body
// (memql#2246). It accepts two authoring shapes inside a `step { ... }`:
//
//	forEach <var> in <expr> [where <cond>] { <call> [<call> ...] }
//	for <var> := range <expr> [if <cond>] { <call> [<call> ...] }
//
// and lowers both to the procedural top-level loop statement
//
//	for item := range <expr> [if <cond>] { <step>_do1 := <call> ... }
//
// which the procedural parser's parseForRangeStep already compiles into a
// StepTypeForEach StepDef whose Do[] children the ForEachExecutor runs once
// per item. The procedural parser pins the iteration variable to the
// canonical `item`, so the author's chosen variable is renamed to `item`
// throughout the body + filter. Each inner call is translated recursively
// through translateStepCall (so it accepts the same `<kind> <name> { ... }`
// / bare / `if cond { ... }` shapes as any other step) and is assigned a
// synthesized name so the nested parseGoStyleStep accepts it.
func translateForEachStepCall(stepName, body string) (string, error) {
	body = strings.TrimSpace(body)
	first, rest := splitLeadingIdent(body)
	isForEach := first == "forEach"
	if first != "forEach" && first != "for" {
		return "", fmt.Errorf("forEach step: expected `forEach` or `for`, got %q", first)
	}

	rest = strings.TrimLeft(rest, " \t\n\r")
	iterVar, afterVar := splitLeadingIdent(rest)
	if iterVar == "" {
		return "", fmt.Errorf("%s step: expected an iteration variable after %q", first, first)
	}
	afterVar = strings.TrimLeft(afterVar, " \t\n\r")

	// Separator between the iteration variable and the collection
	// expression: `in` for forEach, `:= range` for the go-style for.
	var afterSep string
	if isForEach {
		kw, tail := splitLeadingIdent(afterVar)
		if kw != "in" {
			return "", fmt.Errorf("forEach step: expected `in` after the iteration variable %q, got %q", iterVar, parallelSnippet(afterVar))
		}
		afterSep = strings.TrimLeft(tail, " \t\n\r")
	} else {
		if !strings.HasPrefix(afterVar, ":=") {
			return "", fmt.Errorf("for step: expected `:=` after the iteration variable %q, got %q", iterVar, parallelSnippet(afterVar))
		}
		afterVar = strings.TrimLeft(afterVar[2:], " \t\n\r")
		kw, tail := splitLeadingIdent(afterVar)
		if kw != "range" {
			return "", fmt.Errorf("for step: expected `range` after `:=`, got %q", parallelSnippet(afterVar))
		}
		afterSep = strings.TrimLeft(tail, " \t\n\r")
	}

	// The loop body opens at the first `{` (mirrors the procedural parser,
	// which reads the collection expression until `if`/`{`).
	braceIdx := strings.IndexByte(afterSep, '{')
	if braceIdx < 0 {
		return "", fmt.Errorf("%s step: expected `{` to open the loop body", first)
	}
	closeIdx := findMatchingCloseBrace(afterSep, braceIdx)
	if closeIdx < 0 {
		return "", fmt.Errorf("%s step: missing closing brace for the loop body", first)
	}
	if tail := strings.TrimSpace(afterSep[closeIdx+1:]); tail != "" {
		return "", fmt.Errorf("%s step: unexpected trailing text after the loop body: %q", first, parallelSnippet(tail))
	}

	header := strings.TrimSpace(afterSep[:braceIdx])
	inner := strings.TrimSpace(afterSep[braceIdx+1 : closeIdx])
	if inner == "" {
		return "", fmt.Errorf("%s step: the loop body is empty", first)
	}

	// Split an optional filter clause off the collection expression. The
	// filter keyword is `where` for forEach and `if` for the go-style for;
	// both lower to the procedural `if <cond>` filter clause.
	filterKw := "if"
	if isForEach {
		filterKw = "where"
	}
	collection := header
	filter := ""
	if idx := indexWord(header, filterKw); idx >= 0 {
		collection = strings.TrimSpace(header[:idx])
		filter = strings.TrimSpace(header[idx+len(filterKw):])
		if filter == "" {
			return "", fmt.Errorf("%s step: empty `%s` filter clause", first, filterKw)
		}
	}
	if collection == "" {
		return "", fmt.Errorf("%s step: missing the collection expression", first)
	}

	calls, err := parseForEachInnerCalls(inner)
	if err != nil {
		return "", err
	}

	// Rename the author's iteration variable to the canonical `item`.
	rename := func(s string) string { return renameWord(s, iterVar, "item") }

	var sb strings.Builder
	sb.WriteString("for item := range ")
	sb.WriteString(collection)
	if filter != "" {
		sb.WriteString(" if ")
		sb.WriteString(rename(filter))
	}
	sb.WriteString(" {")
	for i, c := range calls {
		sb.WriteString("\n    ")
		sb.WriteString(fmt.Sprintf("%s_do%d := ", stepName, i+1))
		sb.WriteString(rename(c))
	}
	sb.WriteString("\n  }")
	return sb.String(), nil
}

// findMatchingCloseParen returns the index of the `)` matching the `(` at
// openIdx, or -1. Same depth-counting contract as findMatchingCloseBrace.
func findMatchingCloseParen(s string, openIdx int) int {
	if openIdx < 0 || openIdx >= len(s) || s[openIdx] != '(' {
		return -1
	}
	depth := 0
	for i := openIdx; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// translateActionStepCall handles the authored action step body (epic #2212,
// I10 #2224) -- the external-capability step a deploy automation drives:
//
//	action("<id>@<ver>") { args { <k>: <v>, ... } } [on surface("<name>")]
//	action { ref: "<id>@<ver>", args: { ... }, surface: "<name>" }   (procedural)
//
// and lowers the authoring form to the procedural action-step config that the
// parser's parseActionStepConfig already accepts (StepTypeAction):
//
//	action { ref: "<id>@<ver>"[, args: { <k>: <v>, ... }][, surface: "<name>"] }
//
// rest is everything after the leading `action` ident. Actions default to the
// policy surface (cockpit/runner for the deploy pack, I13), so `on surface(...)`
// is optional. Inner `args { ... }` is a brace block (no colon) in the authoring
// form; it is rewritten to the `args: { ... }` key the action config parser reads.
func translateActionStepCall(rest string) (string, error) {
	rest = strings.TrimLeft(rest, " \t\n\r")
	if rest == "" {
		return "", fmt.Errorf("action step: expected `(\"id@ver\")` or a `{ ... }` body after `action`")
	}

	// Procedural pass-through: `action { ref: ..., args: ..., surface: ... }`.
	if rest[0] == '{' {
		closeIdx := findMatchingCloseBrace(rest, 0)
		if closeIdx < 0 {
			return "", fmt.Errorf("action step: missing closing brace for the action body")
		}
		if tail := strings.TrimSpace(rest[closeIdx+1:]); tail != "" {
			return "", fmt.Errorf("action step: unexpected trailing text after the action body: %q", parallelSnippet(tail))
		}
		return "action " + rest[:closeIdx+1], nil
	}

	// Kind-prefixed invocation form (construct-invocation ADR Decision 1+2,
	// Story 4 / memql#2328): `action <name>(<colon-args>) [on surface("x")]`.
	// The action resolves by NAME now (no `@version` suffix); the colon-named
	// args become the action config's `args: { ... }` object.
	if rest[0] != '(' && rest[0] != '"' {
		name, after := splitLeadingIdent(rest)
		if name == "" {
			return "", fmt.Errorf("action step: expected `<name>(...)` or `(\"id@ver\")` after `action`, got %q", parallelSnippet(rest))
		}
		after = strings.TrimLeft(after, " \t\n\r")
		if after == "" || after[0] != '(' {
			return "", fmt.Errorf("action step: expected `(` after action name %q", name)
		}
		pc := findMatchingCloseParen(after, 0)
		if pc < 0 {
			return "", fmt.Errorf("action step: missing closing `)` after the action args")
		}
		inner := strings.TrimSpace(after[1:pc])
		after = strings.TrimLeft(after[pc+1:], " \t\n\r")

		argsClause := ""
		if inner != "" {
			argsClause = ", args: { " + inner + " }"
		}

		surfaceClause, serr := parseTrailingOnSurface(after)
		if serr != nil {
			return "", serr
		}
		return `action { ref: "` + name + `"` + argsClause + surfaceClause + " }", nil
	}

	// Authoring form: `action("ref@1") { args { ... } } [on surface("x")]`.
	if rest[0] != '(' {
		return "", fmt.Errorf("action step: expected `(\"id@ver\")` after `action`, got %q", parallelSnippet(rest))
	}
	parenClose := findMatchingCloseParen(rest, 0)
	if parenClose < 0 {
		return "", fmt.Errorf("action step: missing closing `)` after the action ref")
	}
	ref := strings.TrimSpace(rest[1:parenClose])
	if ref == "" {
		return "", fmt.Errorf("action step: empty action ref in `action(...)`")
	}
	if ref[0] != '"' {
		ref = `"` + ref + `"`
	}
	rest = strings.TrimLeft(rest[parenClose+1:], " \t\n\r")

	argsClause := ""
	if rest != "" && rest[0] == '{' {
		bClose := findMatchingCloseBrace(rest, 0)
		if bClose < 0 {
			return "", fmt.Errorf("action step: missing closing brace for the action body")
		}
		body := strings.TrimSpace(rest[1:bClose])
		rest = strings.TrimLeft(rest[bClose+1:], " \t\n\r")
		if body != "" {
			ai, after := splitLeadingIdent(body)
			if ai != "args" {
				return "", fmt.Errorf("action step: expected `args { ... }` in the action body, got %q", parallelSnippet(body))
			}
			after = strings.TrimLeft(after, " \t\n\r")
			if after == "" || after[0] != '{' {
				return "", fmt.Errorf("action step: expected `{` after `args`")
			}
			aClose := findMatchingCloseBrace(after, 0)
			if aClose < 0 {
				return "", fmt.Errorf("action step: missing closing brace for the args block")
			}
			if tail := strings.TrimSpace(after[aClose+1:]); tail != "" {
				return "", fmt.Errorf("action step: unexpected trailing text in the action body: %q", parallelSnippet(tail))
			}
			argsClause = ", args: " + after[:aClose+1]
		}
	}

	surfaceClause, serr := parseTrailingOnSurface(rest)
	if serr != nil {
		return "", serr
	}

	return "action { ref: " + ref + argsClause + surfaceClause + " }", nil
}

// parseTrailingOnSurface parses an optional `on surface("x")` suffix on an
// action step, returning the `, surface: "x"` config clause (or "" when rest is
// empty). Any other trailing text is an error.
func parseTrailingOnSurface(rest string) (string, error) {
	rest = strings.TrimLeft(rest, " \t\n\r")
	if rest == "" {
		return "", nil
	}
	kw, afterKw := splitLeadingIdent(rest)
	if kw != "on" {
		return "", fmt.Errorf("action step: unexpected trailing text after the action body: %q", parallelSnippet(rest))
	}
	afterKw = strings.TrimLeft(afterKw, " \t\n\r")
	sw, afterSw := splitLeadingIdent(afterKw)
	if sw != "surface" {
		return "", fmt.Errorf("action step: expected `surface(\"...\")` after `on`, got %q", parallelSnippet(afterKw))
	}
	afterSw = strings.TrimLeft(afterSw, " \t\n\r")
	if afterSw == "" || afterSw[0] != '(' {
		return "", fmt.Errorf("action step: expected `(` after `surface`")
	}
	pc := findMatchingCloseParen(afterSw, 0)
	if pc < 0 {
		return "", fmt.Errorf("action step: missing closing `)` after surface")
	}
	s := strings.TrimSpace(afterSw[1:pc])
	if tail := strings.TrimSpace(afterSw[pc+1:]); tail != "" {
		return "", fmt.Errorf("action step: unexpected trailing text after `on surface(...)`: %q", parallelSnippet(tail))
	}
	if s == "" {
		return "", fmt.Errorf("action step: empty surface in `on surface(...)`")
	}
	return ", surface: " + s, nil
}

// translateSwitchStepCall handles the authored switch step body (epic #2212,
// I10 #2224) -- the single env-branch a deploy automation carries:
//
//	switch <expr> { case "v" { <call> ... } ... default { <call> ... } }
//
// and lowers it to the procedural switch statement parseSwitchStep accepts:
//
//	switch <expr> { case "v": <name> := <call> ... default: <name> := <call> ... }
//
// stepName seeds the synthesized per-call step ids. Each case body is a sequence
// of brace-bodied calls (mutation / logic / action / `if cond { ... }`), reusing
// parseForEachInnerCalls to split + translate them -- so an `action(...)` inside a
// case lowers through translateActionStepCall like anywhere else. An empty case
// body (`case "v" { }`) is allowed: it lowers to a label with no steps, which is
// how an env that takes no action in this branch is expressed.
func translateSwitchStepCall(stepName, full string) (string, error) {
	first, rest := splitLeadingIdent(full)
	if first != "switch" {
		return "", fmt.Errorf("switch step: expected `switch`, got %q", first)
	}
	rest = strings.TrimLeft(rest, " \t\n\r")
	braceIdx := strings.IndexByte(rest, '{')
	if braceIdx < 0 {
		return "", fmt.Errorf("switch step: expected `{` after the switch expression")
	}
	expr := strings.TrimSpace(rest[:braceIdx])
	if expr == "" {
		return "", fmt.Errorf("switch step: missing switch expression between `switch` and `{`")
	}
	closeIdx := findMatchingCloseBrace(rest, braceIdx)
	if closeIdx < 0 {
		return "", fmt.Errorf("switch step: missing closing brace for the switch body")
	}
	if tail := strings.TrimSpace(rest[closeIdx+1:]); tail != "" {
		return "", fmt.Errorf("switch step: unexpected trailing text after the switch body: %q", parallelSnippet(tail))
	}
	inner := rest[braceIdx+1 : closeIdx]

	var sb strings.Builder
	sb.WriteString("switch ")
	sb.WriteString(expr)
	sb.WriteString(" {")

	pos := 0
	callIdx := 0
	sawClause := false
	for {
		pos = skipParallelInsignificant(inner, pos)
		if pos >= len(inner) {
			break
		}
		kw, afterKw := splitLeadingIdent(inner[pos:])
		if kw != "case" && kw != "default" {
			return "", fmt.Errorf("switch step: expected `case` or `default`, got %q", parallelSnippet(inner[pos:]))
		}
		pos += len(inner[pos:]) - len(afterKw)

		var label string
		if kw == "case" {
			pos = skipParallelInsignificant(inner, pos)
			b := strings.IndexByte(inner[pos:], '{')
			if b < 0 {
				return "", fmt.Errorf("switch step: expected `{` after the case value")
			}
			label = strings.TrimSpace(inner[pos : pos+b])
			if label == "" {
				return "", fmt.Errorf("switch step: empty case value")
			}
			pos += b
		} else {
			pos = skipParallelInsignificant(inner, pos)
			if pos >= len(inner) || inner[pos] != '{' {
				return "", fmt.Errorf("switch step: expected `{` after `default`")
			}
		}

		bClose := findMatchingCloseBrace(inner, pos)
		if bClose < 0 {
			return "", fmt.Errorf("switch step: missing closing brace for the %s body", kw)
		}
		caseBody := strings.TrimSpace(inner[pos+1 : bClose])
		pos = bClose + 1
		sawClause = true

		if kw == "case" {
			sb.WriteString("\n    case ")
			sb.WriteString(label)
			sb.WriteString(":")
		} else {
			sb.WriteString("\n    default:")
		}
		if caseBody != "" {
			calls, err := parseForEachInnerCalls(caseBody)
			if err != nil {
				return "", fmt.Errorf("switch step %s body: %w", kw, err)
			}
			for _, c := range calls {
				callIdx++
				sb.WriteString(fmt.Sprintf(" %s_c%d := %s", stepName, callIdx, c))
			}
		}
	}
	if !sawClause {
		return "", fmt.Errorf("switch step: at least one `case`/`default` clause is required")
	}
	sb.WriteString("\n  }")
	return sb.String(), nil
}

// parseForEachInnerCalls splits a forEach loop body into its sequence of
// call statements and translates each through translateStepCall. Every call
// is a brace-bodied form -- `<kind> <name> { ... }`, bare `<name> { ... }`,
// or `if <cond> { <call> }` -- so the call span runs from the start of the
// statement through the matching close brace of its first `{`. Whitespace,
// commas, and `//` line comments between statements are skipped.
func parseForEachInnerCalls(inner string) ([]string, error) {
	var out []string
	pos := 0
	for {
		pos = skipParallelInsignificant(inner, pos)
		if pos >= len(inner) {
			break
		}
		end, err := innerCallSpanEnd(inner, pos)
		if err != nil {
			return nil, fmt.Errorf("forEach step: %w", err)
		}
		callSrc := strings.TrimSpace(inner[pos:end])
		call, err := translateStepCall(callSrc)
		if err != nil {
			return nil, fmt.Errorf("forEach step: %w", err)
		}
		out = append(out, call)
		pos = end
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("forEach step: the loop body has no calls")
	}
	return out, nil
}

// innerCallSpanEnd returns the exclusive end index of the single call statement
// beginning at start in inner. It handles every inner-call shape a forEach /
// switch-case body hosts:
//
//   - brace-bodied: `<kind> <name> { ... }`, bare `<name> { ... }`,
//     `if <cond> { <call> }` -- the call ends at the matching `}` of its body.
//   - kind-prefixed paren call (construct-invocation ADR, Story 4):
//     `action <name>(...)`, `mutation <name>(...)`, bare `<name>(...)` -- the
//     call ends at the matching `)`, then greedily absorbs a trailing
//     `{ ... }` (the legacy `action("ref") { args }` body) and/or an
//     `on surface("x")` clause.
func innerCallSpanEnd(inner string, start int) (int, error) {
	s := inner[start:]
	first, rest := splitLeadingIdent(s)
	if first == "" {
		return 0, fmt.Errorf("expected a call at %q", parallelSnippet(s))
	}

	// `if <cond> { <call> }` is always brace-bodied.
	if first == "if" {
		return braceBodyEnd(inner, start)
	}

	// Locate the opener that follows the leading head (a kind keyword + name,
	// a bare name, or `action` immediately followed by `(`).
	rest = strings.TrimLeft(rest, " \t\n\r")
	// A kind-prefixed or bare name may sit between the leading ident and the
	// opener: `action <name>(`, `mutation <name>(`, `<name> {`. Consume one
	// optional identifier so the opener check lands on `(` or `{`.
	if rest != "" && isIdentStart(rest[0]) {
		_, after := splitLeadingIdent(rest)
		rest = strings.TrimLeft(after, " \t\n\r")
	}

	if rest == "" {
		return 0, fmt.Errorf("inner call %q must have a `(...)` or `{ ... }` body", parallelSnippet(s))
	}
	switch rest[0] {
	case '{':
		return braceBodyEnd(inner, start)
	case '(':
		return parenCallEnd(inner, start)
	default:
		return 0, fmt.Errorf("inner call %q must have a `(...)` or `{ ... }` body", parallelSnippet(s))
	}
}

// braceBodyEnd spans a brace-bodied call: from start to the matching `}` of its
// first `{`.
func braceBodyEnd(inner string, start int) (int, error) {
	braceRel := strings.IndexByte(inner[start:], '{')
	if braceRel < 0 {
		return 0, fmt.Errorf("inner call %q must have a `{ ... }` body", parallelSnippet(inner[start:]))
	}
	closeIdx := findMatchingCloseBrace(inner, start+braceRel)
	if closeIdx < 0 {
		return 0, fmt.Errorf("inner call missing closing brace")
	}
	return closeIdx + 1, nil
}

// parenCallEnd spans a paren-call: from start to the matching `)` of its first
// `(`, greedily absorbing a trailing `{ ... }` body and/or `on surface(...)`.
func parenCallEnd(inner string, start int) (int, error) {
	parenRel := strings.IndexByte(inner[start:], '(')
	if parenRel < 0 {
		return 0, fmt.Errorf("inner call %q missing `(`", parallelSnippet(inner[start:]))
	}
	closeRel := findMatchingCloseParen(inner[start:], parenRel)
	if closeRel < 0 {
		return 0, fmt.Errorf("inner call missing closing `)`")
	}
	end := start + closeRel + 1

	// Greedily absorb a trailing `{ ... }` body (legacy `action("ref") { args }`).
	probe := skipParallelInsignificant(inner, end)
	if probe < len(inner) && inner[probe] == '{' {
		closeIdx := findMatchingCloseBrace(inner, probe)
		if closeIdx < 0 {
			return 0, fmt.Errorf("inner call missing closing brace for its body")
		}
		end = closeIdx + 1
		probe = skipParallelInsignificant(inner, end)
	}
	// Greedily absorb a trailing `on surface("x")` clause.
	if probe < len(inner) {
		kw, _ := splitLeadingIdent(inner[probe:])
		if kw == "on" {
			surfParenRel := strings.IndexByte(inner[probe:], '(')
			if surfParenRel >= 0 {
				pc := findMatchingCloseParen(inner[probe:], surfParenRel)
				if pc >= 0 {
					end = probe + pc + 1
				}
			}
		}
	}
	return end, nil
}

// isIdentStart reports whether c can begin an identifier.
func isIdentStart(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_'
}

// indexWord returns the byte index of the first whole-word occurrence of
// word in s, or -1. A whole word is bounded by identifier-character
// boundaries (so `in` does not match inside `info`).
func indexWord(s, word string) int {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(word) + `\b`)
	loc := re.FindStringIndex(s)
	if loc == nil {
		return -1
	}
	return loc[0]
}

// renameWord replaces every whole-word occurrence of from with to in s. Used
// to rename a forEach author's iteration variable to the canonical `item`
// (e.g. `node.id` -> `item.id`) without touching substrings (`updateNode`).
func renameWord(s, from, to string) string {
	if from == to || from == "" {
		return s
	}
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(from) + `\b`)
	return re.ReplaceAllString(s, to)
}

// translateParallelStepCall handles the `parallel { ... }` step body
// (memql#1368). rest is everything after the leading `parallel` ident: a
// braced config block carrying, in any order:
//
//	wait: "all" | "any" | "none"   (optional, default "all")
//	failFast: true | false         (optional, default false)
//	branches: [ step <name> { <body> }, ... ]   (required, non-empty)
//
// Each branch is a full step block; its body goes through translateStepCall
// recursively, so branches support every step shape -- including `if <cond>
// { <call> }` gating and nested `parallel { ... }` blocks. The output is the
// procedural form `parallel { wait: ..., failFast: ..., branches: [ <name>
// := <call>, ... ] }` that parseGoStyleStep parses into StepTypeParallel.
// Line comments (`// ...`) between config keys are skipped.
func translateParallelStepCall(rest string) (string, error) {
	rest = strings.TrimLeft(rest, " \t\n\r")
	if rest == "" || rest[0] != '{' {
		return "", fmt.Errorf("parallel step: expected `{` after `parallel`")
	}
	closeIdx := findMatchingCloseBrace(rest, 0)
	if closeIdx < 0 {
		return "", fmt.Errorf("parallel step: missing closing brace for the parallel block")
	}
	if tail := strings.TrimSpace(rest[closeIdx+1:]); tail != "" {
		return "", fmt.Errorf("parallel step: unexpected trailing text after the parallel block: %q", tail)
	}
	body := rest[1:closeIdx]

	wait := "all"
	failFast := "false"
	var branches []automationStep
	sawBranches := false

	pos := 0
	for {
		pos = skipParallelInsignificant(body, pos)
		if pos >= len(body) {
			break
		}
		key, _ := splitLeadingIdent(body[pos:])
		if key == "" {
			return "", fmt.Errorf("parallel step: expected a config key (wait, failFast, branches), got %q", parallelSnippet(body[pos:]))
		}
		pos += len(key)
		pos = skipParallelInsignificant(body, pos)
		if pos >= len(body) || body[pos] != ':' {
			return "", fmt.Errorf("parallel step: expected `:` after key %q", key)
		}
		pos++
		pos = skipParallelInsignificant(body, pos)
		switch {
		case strings.EqualFold(key, "wait"):
			val, next, err := parseParallelQuotedString(body, pos)
			if err != nil {
				return "", fmt.Errorf("parallel step: wait: %w", err)
			}
			switch val {
			case "all", "any", "none":
			default:
				return "", fmt.Errorf("parallel step: invalid wait value %q (must be \"all\", \"any\", or \"none\")", val)
			}
			wait, pos = val, next
		case strings.EqualFold(key, "failFast"):
			ident, _ := splitLeadingIdent(body[pos:])
			if ident != "true" && ident != "false" {
				return "", fmt.Errorf("parallel step: failFast must be true or false, got %q", parallelSnippet(body[pos:]))
			}
			failFast = ident
			pos += len(ident)
		case strings.EqualFold(key, "branches"):
			if pos >= len(body) || body[pos] != '[' {
				return "", fmt.Errorf("parallel step: expected `[` after `branches:`")
			}
			closeBr := findMatchingCloseBracket(body, pos)
			if closeBr < 0 {
				return "", fmt.Errorf("parallel step: missing closing `]` for the branches list")
			}
			parsed, err := parseParallelBranches(body[pos+1 : closeBr])
			if err != nil {
				return "", err
			}
			branches = parsed
			sawBranches = true
			pos = closeBr + 1
		default:
			return "", fmt.Errorf("parallel step: unknown key %q (expected wait, failFast, branches)", key)
		}
	}

	if !sawBranches {
		return "", fmt.Errorf("parallel step: a `branches: [ step <name> { ... }, ... ]` list is required")
	}
	if len(branches) == 0 {
		return "", fmt.Errorf("parallel step: branches must not be empty")
	}
	seen := make(map[string]struct{}, len(branches))
	for _, b := range branches {
		if _, dup := seen[b.name]; dup {
			return "", fmt.Errorf("parallel step: duplicate branch id %q", b.name)
		}
		seen[b.name] = struct{}{}
	}

	// NOTE: branch assignments are emitted whitespace-separated (no commas).
	// The legacy expression grammar treats `,` as logical OR, so a comma
	// after `name := call(...)` would be swallowed into the call's
	// expression; the procedural branches parser is newline/space-driven
	// like the top-level step list.
	var sb strings.Builder
	sb.WriteString("parallel { wait: \"")
	sb.WriteString(wait)
	sb.WriteString("\", failFast: ")
	sb.WriteString(failFast)
	sb.WriteString(", branches: [ ")
	for i, b := range branches {
		if i > 0 {
			sb.WriteString(" ")
		}
		sb.WriteString(b.name)
		sb.WriteString(" := ")
		sb.WriteString(b.call)
	}
	sb.WriteString(" ] }")
	return sb.String(), nil
}

// parseParallelBranches parses the contents of a `branches: [ ... ]` list:
// a comma-separated sequence of `step <name> { <body> }` blocks. Each body
// is translated through translateStepCall (so nested gating / parallel
// forms ride through).
func parseParallelBranches(list string) ([]automationStep, error) {
	var out []automationStep
	pos := 0
	for {
		pos = skipParallelInsignificant(list, pos)
		if pos >= len(list) {
			break
		}
		kw, _ := splitLeadingIdent(list[pos:])
		if kw != "step" {
			return nil, fmt.Errorf("parallel step: each branches entry must be a `step <name> { ... }` block, got %q", parallelSnippet(list[pos:]))
		}
		pos += len(kw)
		pos = skipParallelInsignificant(list, pos)
		name, _ := splitLeadingIdent(list[pos:])
		if name == "" {
			return nil, fmt.Errorf("parallel step: branch step is missing a name")
		}
		pos += len(name)
		pos = skipParallelInsignificant(list, pos)
		if pos >= len(list) || list[pos] != '{' {
			return nil, fmt.Errorf("parallel step: branch %q: expected `{` after the step name", name)
		}
		closeIdx := findMatchingCloseBrace(list, pos)
		if closeIdx < 0 {
			return nil, fmt.Errorf("parallel step: branch %q: missing closing brace", name)
		}
		inner := strings.TrimSpace(list[pos+1 : closeIdx])
		if inner == "" {
			return nil, fmt.Errorf("parallel step: branch %q: body is empty", name)
		}
		call, err := translateStepCall(inner)
		if err != nil {
			return nil, fmt.Errorf("parallel step: branch %q: %w", name, err)
		}
		out = append(out, automationStep{name: name, call: call})
		pos = closeIdx + 1
	}
	return out, nil
}

// skipParallelInsignificant advances past whitespace, commas, and `//` line
// comments inside a parallel config block.
func skipParallelInsignificant(s string, pos int) int {
	for pos < len(s) {
		c := s[pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			pos++
			continue
		}
		if c == '/' && pos+1 < len(s) && s[pos+1] == '/' {
			for pos < len(s) && s[pos] != '\n' {
				pos++
			}
			continue
		}
		break
	}
	return pos
}

// parseParallelQuotedString reads a double-quoted string literal at pos and
// returns its contents plus the index just past the closing quote.
func parseParallelQuotedString(s string, pos int) (string, int, error) {
	if pos >= len(s) || s[pos] != '"' {
		return "", 0, fmt.Errorf("expected a quoted string, got %q", parallelSnippet(s[pos:]))
	}
	end := strings.IndexByte(s[pos+1:], '"')
	if end < 0 {
		return "", 0, fmt.Errorf("unterminated string")
	}
	return s[pos+1 : pos+1+end], pos + end + 2, nil
}

// parallelSnippet truncates s for inclusion in a diagnostic.
func parallelSnippet(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 24 {
		return s[:24] + "..."
	}
	return s
}

// findMatchingCloseBracket returns the index of the `]` matching the `[` at
// openIdx, or -1. Same depth-counting contract as findMatchingCloseBrace.
func findMatchingCloseBracket(s string, openIdx int) int {
	if openIdx < 0 || openIdx >= len(s) || s[openIdx] != '[' {
		return -1
	}
	depth := 0
	for i := openIdx; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func splitLeadingIdent(s string) (string, string) {
	end := 0
	for end < len(s) {
		c := s[end]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
			end++
			continue
		}
		break
	}
	return s[:end], s[end:]
}

func kindPrefix(name string) bool {
	switch name {
	// `mutate` is the legacy step verb; `mutation` is the canonical
	// kind-prefixed invocation keyword the tree migrated to (Story 3 / #2326).
	// `builtin` lets a builtin step carry its kind prefix too. The kind is a
	// readability/validation tag here -- finishCall strips it and the bare name
	// resolves at runtime regardless of kind.
	case "logic", "mutate", "mutation", "query", "builtin", "automation":
		return true
	}
	return false
}

func finishCall(name, rest string) (string, error) {
	if rest == "" {
		return name + "()", nil
	}
	switch rest[0] {
	case '(':
		return name + rest, nil
	case '{':
		closeRel := findMatchingCloseBrace(rest, 0)
		if closeRel < 0 {
			return "", fmt.Errorf("call %q: missing closing brace", name)
		}
		args := rest[:closeRel+1]
		tail := strings.TrimSpace(rest[closeRel+1:])
		if tail != "" {
			return "", fmt.Errorf("call %q: unexpected trailing text after args block: %q", name, tail)
		}
		return name + "(" + args + ")", nil
	default:
		return "", fmt.Errorf("call %q: expected `(` or `{` after name, got %q", name, rest[:1])
	}
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
