// Package parser -- struct-form rewriter.
//
// The DSL author surface uses struct-form for every procedural
// construct: `query NAME { args, filter, shape }`, `mutation NAME
// { args, insert <concept> { ... } }`, `logic NAME { args, body }`,
// `automation NAME { step <name> { ... } }`, plus file-top `args { ... }`
// blocks. The general parser's grammar reads the older procedural
// form (`func (Query) NAME(ctx any) (any, error) { ... }`).
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

// translateArgsRefsToCtx used to swap `args.X` references for the
// legacy `ctx.X` envelope form during the transition window before
// the parser learned `args.X` natively. F.3 of the ctx-envelope
// purge added native `args.X` recognition to the engine parser and
// the mutation-template parser, so this translation is a no-op now
// (preserved as a function call site for the rewriter; remove the
// helper once every emit site has been audited).
func translateArgsRefsToCtx(expr string) string {
	return expr
}

// fileTopUseDecl matches a file-top `use <ns>.<concept>` declaration.
var fileTopUseDecl = regexp.MustCompile(`(?m)^[ \t]*use[ \t]+([A-Za-z_][A-Za-z0-9_.]*)[ \t]*(?://.*)?$`)

// useConceptBinding matches a single-name `@useConcept(<name>)`
// annotation used as the concept binding for a struct-form
// query/mutation.
var useConceptBinding = regexp.MustCompile(`@useConcept\(\s*([A-Za-z_][A-Za-z0-9_]*)\s*\)`)

// extractStructConceptBinding inspects the source for the concept
// binding driving a struct-form query/mutation. Two forms are
// accepted (exactly one must be present):
//
//   - File-top `use <ns>.<concept>` directive -- emits the fully-
//     qualified canonical id (`v1:<ns>:<concept>`).
//   - `@useConcept(<name>)` annotation -- emits the BARE name. The
//     post-parse concept resolver fully-qualifies it.
//
// A leading `v1` version segment in the `use` path is accepted and
// elided.
func extractStructConceptBinding(source string) (string, error) {
	useMatches := fileTopUseDecl.FindStringSubmatch(source)
	annotMatches := useConceptBinding.FindStringSubmatch(source)
	switch {
	case useMatches != nil && annotMatches != nil:
		return "", fmt.Errorf("conflicting concept binding: both file-top `use <ns>.<concept>` directive and `@useConcept(...)` annotation present; pick one")
	case useMatches != nil:
		path := useMatches[1]
		parts := strings.Split(path, ".")
		if len(parts) < 2 {
			return "", fmt.Errorf("invalid `use` path %q: expected `<ns>.<concept>` or `v1.<ns>.<concept>`", path)
		}
		if parts[0] == "v1" {
			parts = parts[1:]
		}
		if len(parts) < 2 {
			return "", fmt.Errorf("invalid `use` path %q: expected at least two segments after the version", path)
		}
		return "v1:" + strings.Join(parts, ":"), nil
	case annotMatches != nil:
		return annotMatches[1], nil
	default:
		return "", fmt.Errorf("missing concept binding; declare via `@useConcept(<name>)` annotation")
	}
}

// useConceptInBlockRe finds @useConcept annotations in a construct's
// preamble (the annotations declared above the block header).
var useConceptInBlockRe = regexp.MustCompile(`(?m)^[ \t]*@useConcept\(([^)]+)\)`)

// extractConceptBindingForBlock returns the concept binding (bare
// name or fully-qualified id) for the construct whose header starts
// at `blockStart`. Looks for the nearest `@useConcept(<name>)` in
// the preamble; falls back to the file-level resolver for
// single-construct legacy files.
func extractConceptBindingForBlock(source string, blockStart int) (string, error) {
	pre := source[:blockStart]
	matches := useConceptInBlockRe.FindAllStringSubmatch(pre, -1)
	if len(matches) > 0 {
		last := matches[len(matches)-1]
		first := strings.TrimSpace(strings.Split(last[1], ",")[0])
		return first, nil
	}
	return extractStructConceptBinding(source)
}

// rewriteEachBlock walks every struct-form construct in `source`
// matching `header`, calls `emit` on each, and splices the result
// back in place. Processes matches in reverse so byte offsets stay
// stable across splices. When `needsConcept` is true, the concept
// binding is resolved (per-block via extractConceptBindingForBlock)
// and passed to emit; otherwise emit receives an empty conceptId.
//
// `kindLabel` is used in error messages ("struct-form query", etc).
// `emit` returns the procedural source that replaces the matched
// block.
func rewriteEachBlock(
	source string,
	header *regexp.Regexp,
	kindLabel string,
	needsConcept bool,
	emit func(name, conceptId, body string) (string, error),
) (string, error) {
	matches := header.FindAllStringIndex(source, -1)
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
			if signatureConcept != "" {
				conceptId = signatureConcept
			} else {
				cid, err := extractConceptBindingForBlock(out, h[0])
				if err != nil {
					return "", fmt.Errorf("%s: %w", kindLabel, err)
				}
				conceptId = cid
			}
		}

		body := out[openIdx+1 : closeIdx]
		rewritten, err := emit(name, conceptId, body)
		if err != nil {
			return "", fmt.Errorf("%s %q: %w", kindLabel, name, err)
		}
		out = out[:h[0]] + rewritten + out[closeIdx+1:]
	}
	return out, nil
}

// emitFuncHeader writes the procedural function preamble: optional
// file-top `args { ... }` block, then the receiver-style function
// signature. Returns the parameter name (`ctx` if args are present,
// otherwise `_`).
func emitFuncHeader(sb *strings.Builder, receiver, name, argsText, returns string) string {
	hasArgs := strings.TrimSpace(argsText) != ""
	if hasArgs {
		sb.WriteString("args {\n")
		sb.WriteString(argsText)
		if !strings.HasSuffix(strings.TrimRight(argsText, " \t"), "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("}\n")
	}
	param := "_"
	if hasArgs {
		param = "ctx"
	}
	sb.WriteString(fmt.Sprintf("func (%s) %s(%s any)", receiver, name, param))
	if returns != "" {
		sb.WriteString(" ")
		sb.WriteString(returns)
	}
	sb.WriteString(" {\n")
	return param
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

// =============================================================================
// Public chain: NormaliseAll
// =============================================================================

// NormaliseAll runs every struct-form rewriter in sequence: query,
// mutation, logic, automation, file-top args. Each stage is a no-op
// when the source doesn't match its detector. Errors from any
// stage are wrapped with the stage name and returned immediately.
func NormaliseAll(source string) (string, error) {
	steps := []struct {
		name   string
		detect func(string) bool
		apply  func(string) (string, error)
	}{
		{"query", LooksLikeStructQuery, NormaliseQuerySource},
		{"mutation", LooksLikeStructMutation, NormaliseMutationSource},
		{"logic", LooksLikeStructLogic, NormaliseLogicSource},
		{"automation", LooksLikeStructAutomation, NormaliseAutomationSource},
		{"file-top args", LooksLikeFileTopArgs, NormaliseFileTopArgs},
	}
	for _, step := range steps {
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

// queryStructHeader matches both the legacy and the canonical
// post-migration shapes:
//
//	query queryFoo { ... }                  -- legacy (concept via @useConcept)
//	query participant queryFoo { ... }      -- canonical (concept-in-signature)
//
// Group 1 is the optional concept name; group 2 is the construct name.
var queryStructHeader = regexp.MustCompile(`(?m)^[ \t]*query[ \t]+(?:([A-Za-z_][A-Za-z0-9_]*)[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

// LooksLikeStructQuery reports whether the source declares a
// struct-form query.
func LooksLikeStructQuery(source string) bool {
	return queryStructHeader.MatchString(source)
}

// NormaliseQuerySource rewrites every `query NAME { ... }` block in
// the source to the procedural `func (Query) NAME(ctx any) (any,
// error) { ctx.output = <expr>; return ctx, nil }` form.
func NormaliseQuerySource(source string) (string, error) {
	return rewriteEachBlock(source, queryStructHeader, "struct-form query", true, emitQuery)
}

// structQueryBody is the parsed shape of a query body.
type structQueryBody struct {
	filter   string
	shape    string
	argsText string
	sort     string
	paginate string
	asOf     string
}

func emitQuery(name, conceptId, body string) (string, error) {
	parsed, err := parseStructQueryBody(body)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	emitFuncHeader(&sb, "Query", name, parsed.argsText, "(any, error)")
	sb.WriteString("  return ")
	sb.WriteString(buildStructQueryExpr(conceptId, parsed.filter, parsed.shape))
	sb.WriteString(", nil\n}")
	return sb.String(), nil
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
			out.filter = translateArgsRefsToCtx(strings.TrimSpace(strings.TrimPrefix(line, "filter")))
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

// buildStructQueryExpr stitches the concept / filter / shape pieces
// into the runtime expression the engine already knows how to
// compile.
func buildStructQueryExpr(conceptId, filter, shape string) string {
	base := "concept==" + conceptId
	if filter != "" {
		base += ";" + filter
	}
	if shape != "" {
		return fmt.Sprintf("shape(%s, %q)", base, shape)
	}
	return base
}

// =============================================================================
// Mutation
// =============================================================================

// mutationStructHeader matches both the legacy and the canonical
// post-migration shapes; mirrors queryStructHeader.
var mutationStructHeader = regexp.MustCompile(`(?m)^[ \t]*mutation[ \t]+(?:([A-Za-z_][A-Za-z0-9_]*)[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

// LooksLikeStructMutation reports whether the source declares a
// struct-form mutation.
func LooksLikeStructMutation(source string) bool {
	return mutationStructHeader.MatchString(source)
}

// NormaliseMutationSource rewrites every `mutation NAME { ... }`
// block to the procedural form.
func NormaliseMutationSource(source string) (string, error) {
	return rewriteEachBlock(source, mutationStructHeader, "struct-form mutation", true, emitMutation)
}

// structMutationBody is the parsed shape of a mutation body.
type structMutationBody struct {
	argsText    string
	writeKind   string // "insert" or "update"
	writeBody   string // raw block contents, args.X translated to ctx.X
	writeTarget string // bare concept name from `<kind> <name> { ... }`
}

func emitMutation(name, conceptId, body string) (string, error) {
	parsed, err := parseStructMutationBody(body)
	if err != nil {
		return "", err
	}

	// The write-target's bare name must match the bare concept name
	// inferred from the file's binding (last colon-separated segment
	// of the canonical id).
	expected := conceptId
	if idx := strings.LastIndex(conceptId, ":"); idx >= 0 {
		expected = conceptId[idx+1:]
	}
	if parsed.writeTarget != expected {
		return "", fmt.Errorf("%s target %q does not match the concept binding %q -- write it as `%s %s { ... }`", parsed.writeKind, parsed.writeTarget, expected, parsed.writeKind, expected)
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
		// (mirroring insert) so the declared-usage validator finds
		// the implicit `@useConcept(X)` reference in the body. The
		// runtime doesn't strictly need it -- update looks up by id
		// -- but emitting it keeps the post-rewrite shape symmetric
		// with insert and prevents `function not found` at call
		// time from a silently-dropped load.
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

	// Scan a write block: `<insert|update> <bareConceptName> { ... }`.
	scanWrite := func(keyword string) (name, raw string, found bool, err error) {
		re := regexp.MustCompile(`(^|[\n\r])[ \t]*` + keyword + `[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
		loc := re.FindStringSubmatchIndex(body)
		if loc == nil {
			// Detect the legacy bare form `insert { ... }` so the
			// error message names the missing concept binding.
			bareRe := regexp.MustCompile(`(^|[\n\r])[ \t]*` + keyword + `[ \t]*\{`)
			if bareRe.FindStringIndex(body) != nil {
				return "", "", false, fmt.Errorf("`%s { ... }` is retired -- use `%s <conceptName> { ... }` so the write target is visible at the block header (the concept name must match the file's `@useConcept(...)` binding)", keyword, keyword)
			}
			return "", "", false, nil
		}
		name = body[loc[4]:loc[5]]
		openOffset := strings.LastIndex(body[loc[0]:loc[1]], "{")
		open := loc[0] + openOffset
		close := findMatchingCloseBrace(body, open)
		if close < 0 {
			return "", "", false, fmt.Errorf("`%s %s { ... }` block missing closing brace", keyword, name)
		}
		return name, body[open+1 : close], true, nil
	}

	insertName, insertRaw, hasInsert, err := scanWrite("insert")
	if err != nil {
		return nil, err
	}
	updateName, updateRaw, hasUpdate, err := scanWrite("update")
	if err != nil {
		return nil, err
	}
	switch {
	case hasInsert && hasUpdate:
		return nil, fmt.Errorf("mutation body cannot mix `insert <name> { ... }` and `update <name> { ... }`")
	case hasInsert:
		out.writeKind = "insert"
		out.writeBody = translateArgsRefsToCtx(insertRaw)
		out.writeTarget = insertName
	case hasUpdate:
		out.writeKind = "update"
		out.writeBody = translateArgsRefsToCtx(updateRaw)
		out.writeTarget = updateName
	default:
		return nil, fmt.Errorf("mutation body must contain exactly one `insert <conceptName> { ... }` or `update <conceptName> { ... }` block")
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
// the procedural `func (Logic) NAME(ctx any) (any, error) { ... }`
// form.
func NormaliseLogicSource(source string) (string, error) {
	return rewriteEachBlock(source, logicStructHeader, "struct-form logic", false, emitLogic)
}

var logicBodyBlockHeader = regexp.MustCompile(`(^|[\n\r])[ \t]*body[ \t]*\{`)

func emitLogic(name, _conceptId, body string) (string, error) {
	argsText, err := extractArgsBlock(body)
	if err != nil {
		return "", err
	}

	loc := logicBodyBlockHeader.FindStringIndex(body)
	if loc == nil {
		return "", fmt.Errorf("logic body must contain a `body { ... }` block")
	}
	openOffset := strings.LastIndex(body[loc[0]:loc[1]], "{")
	open := loc[0] + openOffset
	close := findMatchingCloseBrace(body, open)
	if close < 0 {
		return "", fmt.Errorf("`body { ... }` block missing closing brace")
	}
	bodyText := translateArgsRefsToCtx(body[open+1 : close])

	var sb strings.Builder
	emitFuncHeader(&sb, "Logic", name, argsText, "(any, error)")
	sb.WriteString(bodyText)
	if !containsTrailingReturn(bodyText) {
		sb.WriteString("\n  return ctx, nil\n")
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

// automationStep is the parsed shape of a single step.
type automationStep struct {
	name string
	call string
}

func emitAutomation(name, _conceptId, body string) (string, error) {
	steps, err := parseAutomationSteps(body)
	if err != nil {
		return "", err
	}
	if len(steps) == 0 {
		return "", fmt.Errorf("at least one `step` is required")
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("func (Automation) %s(ctx any) {\n", name))
	for _, s := range steps {
		sb.WriteString("  ")
		sb.WriteString(s.name)
		sb.WriteString(" := ")
		sb.WriteString(s.call)
		sb.WriteString("\n")
	}
	sb.WriteString("  return ctx, nil\n}")
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
		stepBody = translateEventRefsToCtxInput(stepBody)
		call, err := translateStepCall(stepBody)
		if err != nil {
			return nil, fmt.Errorf("step %q: %w", stepName, err)
		}
		out = append(out, automationStep{name: stepName, call: call})
		pos = closeIdx + 1
	}
	return out, nil
}

// eventRefMatcher matches whole-word `event` tokens for translation
// to the legacy `ctx.input` envelope path.
var eventRefMatcher = regexp.MustCompile(`\bevent\b`)

func translateEventRefsToCtxInput(expr string) string {
	return eventRefMatcher.ReplaceAllString(expr, "ctx.input")
}

// translateStepCall converts a step body into the legacy call
// expression. Supported shapes:
//
//   `<kind> <bareName> { <args> }` -- kind is logic / mutation /
//     query / automation; produces `<kind><PascalCase(bare)>({ args })`.
//   `publishEvent { <args> }`      -- builtin; `publishEvent({ args })`.
//   `<prefixedName> { <args> }`    -- direct form.
//   `<name>(<args>)`               -- already parenthesised; passthrough.
func translateStepCall(body string) (string, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", fmt.Errorf("call expression is empty")
	}
	first, rest := splitLeadingIdent(body)
	if first == "" {
		return "", fmt.Errorf("expected identifier at start of call, got %q", body)
	}

	if kindPrefix(first) {
		bare, after := splitLeadingIdent(strings.TrimLeft(rest, " \t\n\r"))
		if bare == "" {
			return "", fmt.Errorf("expected name after `%s` keyword", first)
		}
		fullName := first + strings.ToUpper(bare[:1]) + bare[1:]
		return finishCall(fullName, strings.TrimLeft(after, " \t\n\r"))
	}

	return finishCall(first, strings.TrimLeft(rest, " \t\n\r"))
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
	case "logic", "mutation", "query", "automation":
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

// NormaliseFileTopArgs translates `args.X` references to `ctx.X`
// inside the function body that follows each file-top `args { ... }`
// block. The args block itself stays in the source -- the parser
// ingests it natively and attaches it to the next FunctionDef.
func NormaliseFileTopArgs(source string) (string, error) {
	cursor := 0
	for {
		loc := fileTopArgsHeader.FindStringIndex(source[cursor:])
		if loc == nil {
			return source, nil
		}
		absStart := cursor + loc[0]
		if isInsideStructConstructHeader(source, absStart) {
			cursor = absStart + loc[1] - loc[0]
			continue
		}

		openIdx := cursor + loc[1] - 1
		closeIdx := findMatchingCloseBrace(source, openIdx)
		if closeIdx < 0 {
			return "", fmt.Errorf("file-top `args { ... }` block: missing closing brace")
		}

		after := source[closeIdx+1:]
		defStart := findNextDefinitionIndex(after)
		if defStart < 0 {
			cursor = closeIdx + 1
			continue
		}
		defOpenBrace := strings.Index(after[defStart:], "{")
		if defOpenBrace < 0 {
			cursor = closeIdx + 1
			continue
		}
		defOpen := defStart + defOpenBrace
		defClose := findMatchingCloseBrace(after, defOpen)
		if defClose < 0 {
			cursor = closeIdx + 1
			continue
		}
		bodyAbsStart := closeIdx + 1 + defStart
		bodyAbsEnd := closeIdx + 1 + defClose + 1
		bodyTranslated := translateArgsRefsToCtx(source[bodyAbsStart:bodyAbsEnd])
		source = source[:bodyAbsStart] + bodyTranslated + source[bodyAbsEnd:]

		cursor = closeIdx + 1
	}
}

// definitionLeadingTokens matches the start of any procedural /
// declarative construct, so the file-top args translator can find
// the function body it should rewrite into.
var definitionLeadingTokens = regexp.MustCompile(`(?m)^[ \t]*(func|builtin|tool|prompt|provider|shape|spec|query|mutation|automation|policy)\b`)

func findNextDefinitionIndex(source string) int {
	loc := definitionLeadingTokens.FindStringIndex(source)
	if loc == nil {
		return -1
	}
	insert := loc[0]
	for insert > 0 {
		prevNL := strings.LastIndex(source[:insert-1], "\n")
		lineStart := prevNL + 1
		line := strings.TrimSpace(source[lineStart : insert-1])
		if line == "" || strings.HasPrefix(line, "@") || strings.HasPrefix(line, "//") {
			insert = lineStart
			continue
		}
		break
	}
	return insert
}

// isInsideStructConstructHeader returns true when `argsLoc` falls
// inside the body of a struct-form construct.
var structConstructHeaderForArgs = regexp.MustCompile(`(?m)^[ \t]*(query|mutation)[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{`)

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
