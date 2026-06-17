package memql

import (
	"fmt"
	"strings"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// #250: the soak-period fallback to the legacy memql parser is
// retired (sub-epic #286 migrated the runtime callsites). Each
// detector branch that previously short-circuited to
// errLangparserUnsupported now returns a typed ErrUnsupportedQueryShape
// with a shape-specific message + workaround. Callers writing these
// shapes get an actionable error pointing at the parity follow-up
// instead of the now-gone legacy-parser handling.

// langpathResult carries the parsed runtime query plus the two inline
// modifiers the langparser grammar gap previously rejected (#1558):
// the trailing `@timestamp`/`@latest` asOf modifier and a leading
// inline-spec definition (`name := expr`). Both are zero-valued for the
// common case (a plain filter expression) so callers consume them
// unconditionally.
type langpathResult struct {
	Root        ExpressionNode
	Timestamp   *time.Time
	UseLatest   bool
	InlineSpecs map[string]*Spec
}

// parseViaLangparser parses a runtime query string via the language
// parser + ASTConverter into the engine's ExpressionNode family.
// Post-#250 this is the SOLE runtime parser path.
//
// Returns a typed ErrUnsupportedQueryShape (wrapping a shape-
// specific message) for the shapes the langparser doesn't handle;
// genuine parser errors propagate raw.
//
// #1558: when allowInline is set (the MCP Tier-3 path, gated server-
// side to the inline tier + owner/developer BEFORE this is reached),
// the two inline shapes the pre-flight detector lifts are now PARSED
// here rather than handed to the langparser raw (which cannot express
// them and would error `unexpected token after expression`):
//   - a trailing `@<rfc3339-timestamp>` / `@latest` asOf modifier on
//     the whole query, peeled off and surfaced as Timestamp/UseLatest;
//   - a leading inline-spec definition `name := expr`, materialised
//     into an InlineSpecs entry whose reference becomes the filter root.
func parseViaLangparser(query string, allowInline bool) (langpathResult, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return langpathResult{}, ErrEmptyQuery
	}
	if err := unsupportedQueryShape(trimmed, allowInline); err != nil {
		return langpathResult{}, err
	}

	var result langpathResult

	// #1558 piece 1: peel a trailing @timestamp / @latest asOf modifier
	// off the WHOLE query (inline tier only). The langparser grammar
	// can't express the trailing form, and the bare RFC3339 literal
	// fragments under the lexer, so the suffix is split at the source
	// level and the body is parsed clean.
	if allowInline {
		body, ts, useLatest, hadSuffix, err := splitTrailingAsOf(trimmed)
		if err != nil {
			return langpathResult{}, err
		}
		if hadSuffix {
			result.Timestamp = ts
			result.UseLatest = useLatest
			trimmed = strings.TrimSpace(body)
			if trimmed == "" {
				return langpathResult{}, fmt.Errorf("query is empty before the @asOf modifier")
			}
		}
	}

	// #1558 piece 2: peel a leading inline-spec definition `name := expr`
	// (inline tier only). The named predicate is materialised into an
	// inline *Spec and the filter root becomes a reference to it, so the
	// existing resolvePlanSpecs inline path expands it at compile time.
	if allowInline {
		if name, bodyExpr, ok := splitInlineSpecDefinition(trimmed); ok {
			spec, err := buildInlineSpec(name, bodyExpr)
			if err != nil {
				return langpathResult{}, err
			}
			result.InlineSpecs = map[string]*Spec{name: spec}
			result.Root = &SpecReferenceExpression{Name: name}
			return result, nil
		}
	}

	parsed, err := langparser.ParseExpression(trimmed)
	if err != nil {
		return langpathResult{}, err
	}
	converted, err := NewASTConverter().ConvertExpression(parsed)
	if err != nil {
		return langpathResult{}, err
	}
	result.Root = flattenRuntimeFunctionArgs(converted)
	return result, nil
}

// splitTrailingAsOf detects and peels a trailing `@<rfc3339>` / `@latest`
// asOf modifier off a runtime query string. It scans for a TOP-LEVEL `@`
// (outside any string literal) whose suffix runs to end-of-query, so the
// `@` inside an annotation-shaped string literal or a payload value never
// trips it. Returns the query body (everything before the `@`), the parsed
// timestamp (nil for @latest), useLatest, and whether a suffix was found.
//
// The suffix value is either the bare identifier `latest` or an RFC3339 /
// RFC3339Nano timestamp -- consumed as the raw run after `@` since the
// lexer would otherwise fragment a bare timestamp into number/colon/ident
// tokens.
func splitTrailingAsOf(query string) (body string, ts *time.Time, useLatest bool, found bool, err error) {
	at := lastTopLevelAt(query)
	if at < 0 {
		return query, nil, false, false, nil
	}
	body = strings.TrimRight(query[:at], " \t\n")
	suffix := strings.TrimSpace(query[at+1:])
	if suffix == "" {
		return "", nil, false, false, fmt.Errorf("trailing @ asOf modifier requires a value (@<rfc3339-timestamp> or @latest)")
	}
	if strings.EqualFold(suffix, "latest") {
		return body, nil, true, true, nil
	}
	parsed, perr := time.Parse(time.RFC3339Nano, suffix)
	if perr != nil {
		return "", nil, false, false, fmt.Errorf("invalid @asOf timestamp %q: expected an RFC3339 timestamp or @latest", suffix)
	}
	return body, &parsed, false, true, nil
}

// lastTopLevelAt returns the byte index of the last `@` that sits outside
// any string literal, or -1 if none. The trailing-asOf modifier attaches
// to the whole query, so the relevant `@` is the last one at top level.
func lastTopLevelAt(query string) int {
	idx := -1
	inStr := false
	var quote byte
	for i := 0; i < len(query); i++ {
		c := query[i]
		if inStr {
			if c == '\\' && i+1 < len(query) {
				i++
				continue
			}
			if c == quote {
				inStr = false
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			inStr = true
			quote = c
		case '@':
			idx = i
		}
	}
	return idx
}

// splitInlineSpecDefinition detects a leading inline-spec definition of the
// form `name := expr` and returns the spec name + the source of its body
// expression. The `:=` must appear at TOP LEVEL (outside any string literal)
// and the head must be a single bare identifier, so a `:=` buried in a value
// or a comparison never trips it. ok is false for any non-definition query.
func splitInlineSpecDefinition(query string) (name string, bodyExpr string, ok bool) {
	define := topLevelDefineIndex(query)
	if define < 0 {
		return "", "", false
	}
	head := strings.TrimSpace(query[:define])
	if head == "" || !isBareIdentifier(head) {
		return "", "", false
	}
	body := strings.TrimSpace(query[define+2:])
	if body == "" {
		return "", "", false
	}
	return head, body, true
}

// topLevelDefineIndex returns the byte index of the first `:=` outside any
// string literal, or -1. (`:=` is the inline-spec binding operator.)
func topLevelDefineIndex(query string) int {
	inStr := false
	var quote byte
	for i := 0; i+1 < len(query); i++ {
		c := query[i]
		if inStr {
			if c == '\\' && i+1 < len(query) {
				i++
				continue
			}
			if c == quote {
				inStr = false
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			inStr = true
			quote = c
		case ':':
			if query[i+1] == '=' {
				return i
			}
		}
	}
	return -1
}

// isBareIdentifier reports whether s is a single MemQL identifier (the head
// of an inline-spec definition). Rejects dotted paths, whitespace runs, and
// any non-identifier character so only `name := ...` (not `payload.x := ...`)
// is treated as a definition.
func isBareIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			continue
		}
		if i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

// buildInlineSpec materialises an inline-spec definition's body source into
// a *Spec the engine's inline-spec resolution path consumes. It runs the
// same convert -> normalize -> boolean-check -> classify pipeline that
// specDeclToSpec runs for a DSL `spec` construct, so an inline predicate
// behaves identically to a declared one.
func buildInlineSpec(name, bodyExpr string) (*Spec, error) {
	if err := validateSpecName(name); err != nil {
		return nil, fmt.Errorf("inline spec %q: %w", name, err)
	}
	parsed, err := langparser.ParseExpression(bodyExpr)
	if err != nil {
		return nil, fmt.Errorf("inline spec %q: %w", name, err)
	}
	expr, err := NewASTConverter().ConvertExpression(parsed)
	if err != nil {
		return nil, fmt.Errorf("inline spec %q: convert body: %w", name, err)
	}
	expr = flattenRuntimeFunctionArgs(expr)
	expr, err = normalizeSpecCallsToReferences(expr)
	if err != nil {
		return nil, fmt.Errorf("inline spec %q: normalize body: %w", name, err)
	}
	if err := ensureBooleanExpression(expr); err != nil {
		return nil, fmt.Errorf("inline spec %q body must be boolean: %w", name, err)
	}
	kind, err := classifySpecKind(expr)
	if err != nil {
		return nil, fmt.Errorf("inline spec %q: %w", name, err)
	}
	return &Spec{
		Name:       name,
		ExprSource: canonicalExpression(expr),
		Expr:       expr,
		Kind:       kind,
		UsesSI:     detectSIUsage(expr),
		Origin:     "inline",
	}, nil
}

// flattenRuntimeFunctionArgs reconciles a semantic mismatch between
// the langparser's grammar (where `foo({k: v})` is a single
// POSITIONAL argument that happens to be a map) and the runtime
// grammar's convention (where `foo({k: v})` is a NAMED-args call
// with the inner map's keys promoted to top-level Args). The engine
// + downstream tool-call paths consume the runtime grammar's flat
// shape; without this normalisation a langparser-routed function
// invocation would surface as `Args["0"][k]` rather than `Args[k]`.
//
// The pass walks the produced engine AST and, for every
// *FunctionCallExpression whose Args has exactly one entry keyed
// "0" with a map value, replaces Args with that inner map. Calls
// that already match the runtime convention (or were emitted by
// the memql parser, which builds the flat shape directly) pass
// through untouched. Logical / comparison nodes recurse so nested
// function calls are normalised too.
func flattenRuntimeFunctionArgs(node ExpressionNode) ExpressionNode {
	switch n := node.(type) {
	case *FunctionCallExpression:
		if n != nil && len(n.Args) == 1 {
			if inner, ok := n.Args["0"].(map[string]any); ok {
				n.Args = inner
			}
		}
		return n
	case *LogicalExpression:
		if n != nil {
			n.Left = flattenRuntimeFunctionArgs(n.Left)
			n.Right = flattenRuntimeFunctionArgs(n.Right)
		}
		return n
	}
	return node
}

// langparserPathUnsupported is a cheap upfront detector for the
// trailing-feature shapes the opt-in path doesn't handle yet. We
// run it BEFORE invoking langparser.ParseExpression so the
// fall-back path returns a clean errLangparserUnsupported sentinel
// rather than a parser error the caller would have to fingerprint.
//
// unsupportedQueryShape is the pre-flight detector for runtime
// query shapes the langparser path doesn't handle. Returns a typed
// error wrapping ErrUnsupportedQueryShape with a shape-specific
// message + workaround; nil if the query is fine.
//
// Post-#250: each shape's wrapper points at the DSL-side workaround
// (the parity follow-ups under epic #218). Pre-#250 these branches
// short-circuited to a fallback sentinel that e.Parse routed to the
// legacy memql parser; the parser is gone now, so callers writing
// these shapes get an actionable error.
func unsupportedQueryShape(query string, allowInline bool) error {
	if containsRuntimeCall(query, "shape") {
		return fmt.Errorf("%w: shape(...) runtime usage -- use a DSL-defined query with a @shape(...) projection instead (see sub-epic #286 for the cognition migration pattern)",
			ErrUnsupportedQueryShape)
	}
	if containsRuntimeCall(query, "select") {
		return fmt.Errorf("%w: select(...) runtime usage -- langparser path doesn't populate Plan.Fields/Plan.Metadata; declare the projection in the DSL query instead",
			ErrUnsupportedQueryShape)
	}
	if containsConceptMemqlVersionShape(query) {
		return fmt.Errorf("%w: concept==memql:version compat shape -- call memqlVersion() directly (handled by the meta-command shim)",
			ErrUnsupportedQueryShape)
	}
	if containsInOperator(query) {
		return fmt.Errorf("%w: <expr> in (...) collection operator -- rewrite as a disjunction (x==\"a\" || x==\"b\" || ...)",
			ErrUnsupportedQueryShape)
	}
	if t := strings.TrimRightFunc(query, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n'
	}); strings.HasSuffix(t, ",") {
		return fmt.Errorf("%w: trailing comma in query string -- remove the comma",
			ErrUnsupportedQueryShape)
	}
	inStr := false
	var quote byte
	for i := 0; i < len(query); i++ {
		c := query[i]
		if inStr {
			if c == '\\' && i+1 < len(query) {
				i++
				continue
			}
			if c == quote {
				inStr = false
			}
			continue
		}
		if c == '"' || c == '\'' || c == '`' {
			inStr = true
			quote = c
			continue
		}
		if c == '@' {
			// MCP Tier-3 (#1535): the inline tier + owner/developer lifts the
			// pre-rejection so the langparser is given the chance to parse the
			// shape. (The langparser grammar for trailing @timestamp/@latest is a
			// tracked follow-up; until then the caller gets the parser's own
			// error rather than this blanket pre-rejection.)
			if allowInline {
				continue
			}
			return fmt.Errorf("%w: trailing @timestamp / @latest suffix on a runtime query -- pin the timestamp at the DSL definition site instead (lift via the MCP inline tier)",
				ErrUnsupportedQueryShape)
		}
		if c == ':' && i+1 < len(query) && query[i+1] == '=' {
			if allowInline {
				continue
			}
			return fmt.Errorf("%w: inline spec definition (`name := expr`) in a runtime query -- define the spec in the DSL and reference it by name (lift via the MCP inline tier)",
				ErrUnsupportedQueryShape)
		}
	}
	return nil
}

// containsRuntimeCall reports whether `query` contains a call to
// `funcName(...)` outside of any string literal. Conservative
// matcher: requires a word boundary before the name + the
// immediate `(` after so substrings (e.g. `payload.shapeId` or a
// literal "shape(" inside a quoted string) don't trip. Used by
// langparserPathUnsupported to short-circuit the langparser path
// for runtime directives whose langparser support is known
// incomplete.
func containsRuntimeCall(query, funcName string) bool {
	inStr := false
	var quote byte
	nameLen := len(funcName)
	for i := 0; i < len(query); i++ {
		c := query[i]
		if inStr {
			if c == '\\' && i+1 < len(query) {
				i++
				continue
			}
			if c == quote {
				inStr = false
			}
			continue
		}
		if c == '"' || c == '\'' || c == '`' {
			inStr = true
			quote = c
			continue
		}
		// Try to match funcName at position i. Require word
		// boundary on the left (i==0 OR previous char is not a
		// word char) so `payload.shape` / `myShape` don't match.
		if i+nameLen+1 > len(query) {
			continue
		}
		if query[i:i+nameLen] != funcName {
			continue
		}
		if query[i+nameLen] != '(' {
			continue
		}
		if i > 0 {
			prev := query[i-1]
			if isIdentChar(prev) {
				continue
			}
		}
		return true
	}
	return false
}

// isIdentChar reports whether c is part of a memql identifier.
// Used by containsRuntimeCall to enforce the word boundary.
func isIdentChar(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '_'
}

// containsConceptMemqlVersionShape reports whether `query` contains
// the literal `concept==memql:version` filter shape (outside any
// string literal). The memql parser rewrites this to a memqlVersion
// BuiltinFunctionExpression; the langparser produces a regular
// ComparisonExpression. Until that rewrite migrates to the
// meta-command dispatch (or the langparser learns it), this exact
// shape falls back. Whitespace-tolerant around `==`.
func containsConceptMemqlVersionShape(query string) bool {
	// Cheap pre-filter: must contain both halves.
	if !strings.Contains(query, "concept") || !strings.Contains(query, "memql:version") {
		return false
	}
	// Verify the literal pattern (whitespace-tolerant around ==)
	// outside any string literal.
	inStr := false
	var quote byte
	for i := 0; i < len(query); i++ {
		c := query[i]
		if inStr {
			if c == '\\' && i+1 < len(query) {
				i++
				continue
			}
			if c == quote {
				inStr = false
			}
			continue
		}
		if c == '"' || c == '\'' || c == '`' {
			inStr = true
			quote = c
			continue
		}
		// Try to match `concept` at i.
		const needle = "concept"
		if i+len(needle) > len(query) || query[i:i+len(needle)] != needle {
			continue
		}
		// Word boundary on the left.
		if i > 0 && isIdentChar(query[i-1]) {
			continue
		}
		// Skip whitespace; expect ==.
		j := i + len(needle)
		for j < len(query) && (query[j] == ' ' || query[j] == '\t') {
			j++
		}
		if j+1 >= len(query) || query[j] != '=' || query[j+1] != '=' {
			continue
		}
		j += 2
		for j < len(query) && (query[j] == ' ' || query[j] == '\t') {
			j++
		}
		const target = "memql:version"
		if j+len(target) > len(query) {
			continue
		}
		if query[j:j+len(target)] != target {
			continue
		}
		// Right-boundary check: the literal `memql:version` must end
		// at end-of-query OR followed by a non-ident char (whitespace,
		// `)`, `;`, etc.). Otherwise `concept==memql:versionfoo` would
		// false-positive when both parsers would produce equivalent
		// generic ComparisonExpression for that input.
		end := j + len(target)
		if end < len(query) && isIdentChar(query[end]) {
			continue
		}
		return true
	}
	return false
}

// containsInOperator reports whether `query` contains the
// collection-form `in (...)` operator outside of any string literal.
// Pattern: an identifier char run, optional whitespace, the literal
// `in`, optional whitespace, `(`. Catches `payload.stage in ("a","b")`
// without false-positiving on `paginate(...)` or words containing
// `in` (e.g. `inside`, `integer`) thanks to the word boundaries.
func containsInOperator(query string) bool {
	inStr := false
	var quote byte
	for i := 0; i+3 < len(query); i++ {
		c := query[i]
		if inStr {
			if c == '\\' && i+1 < len(query) {
				i++
				continue
			}
			if c == quote {
				inStr = false
			}
			continue
		}
		if c == '"' || c == '\'' || c == '`' {
			inStr = true
			quote = c
			continue
		}
		// Look for "in" preceded by a word boundary AND followed
		// by optional whitespace + `(`.
		if c != 'i' || query[i+1] != 'n' {
			continue
		}
		// Left boundary: must be at start OR non-ident char before.
		if i > 0 && isIdentChar(query[i-1]) {
			continue
		}
		// Right side: `in` must be followed by space-then-paren or
		// directly by `(`. (Operator form, not function-call form.)
		j := i + 2
		// Reject identifier continuation (`inside`, `integer`).
		if j < len(query) && isIdentChar(query[j]) {
			continue
		}
		for j < len(query) && (query[j] == ' ' || query[j] == '\t') {
			j++
		}
		if j < len(query) && query[j] == '(' {
			return true
		}
	}
	return false
}
