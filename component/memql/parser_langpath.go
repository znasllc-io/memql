package memql

import (
	"errors"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// errLangparserUnsupported is the sentinel sentinel returned by
// parseViaLangparser when the runtime query uses a shape the opt-in
// langparser path doesn't (yet) cover. e.Parse catches this
// specific error and falls back to the memql/parser.go path, so a
// genuine parse error from the langparser path (anything OTHER
// than this sentinel) still propagates to the caller unchanged.
//
// The set of "unsupported" shapes is the small tail that the
// runtime grammar handles via post-parse processing in
// component/memql/parser.go: trailing `@timestamp` / `@latest`
// suffixes, inline spec definitions (`name := expr`), and raw
// `insert(...)` mutations. (The last is moot here -- e.Parse
// already short-circuits insert() before reaching either parser.)
// The opt-in path can grow to cover these in a follow-up; for
// #248 the contract is "function-invocation + filter-expression
// + accessor queries", which covers the dominant SDK-generated
// query shape AND the hand-written `concept==X` queries.
var errLangparserUnsupported = errors.New("langparser runtime path: query shape not yet supported")

// parseViaLangparser is the opt-in alternative to
// (memql/parser.go).tokenize+newParser+parse: it parses a runtime
// query string via the SAME language parser the .memql loader uses,
// then converts the produced langparser AST into the engine's
// ExpressionNode family via the existing ASTConverter.
//
// The two parsers were the duplication that caused #216 / #221 /
// #239 to require matching fixes in both. #244 (shared accessor
// classifier) + #242 (shared token-enum) closed the correctness
// arc; this is the first slice of the broader retirement
// (#248 / 3a; followed by #249 default-flip and #250 deletion).
// Default behaviour is unchanged -- see (*MemQLEngine).UseLangparserRuntime
// and the soak-period plan in #249.
//
// Returns errLangparserUnsupported (sentinel) for query shapes
// outside the opt-in's scope so e.Parse can fall back to the
// established path; any other error is a real parse failure and
// propagates unchanged.
func parseViaLangparser(query string) (ExpressionNode, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return nil, ErrEmptyQuery
	}
	if langparserPathUnsupported(trimmed) {
		return nil, errLangparserUnsupported
	}
	parsed, err := langparser.ParseExpression(trimmed)
	if err != nil {
		// #249 soak posture: any langparser parse error falls back
		// to the memql parser so the canonical error message +
		// sentinel chain (ErrInvalidArgument, ErrInvalidSyntax,
		// etc.) reaches the caller via the memql path. Without this
		// conversion, langparser's bare error text doesn't wrap the
		// sentinels callers grep for via errors.Is(), and tests like
		// TestParseErrors break on the wrapping check. Lose nothing
		// in correctness: memql also fails -> caller sees the right
		// error; memql succeeds -> the AST we get is the right one.
		// Once langparser error-wrapping reaches parity (separate
		// follow-up under epic #218) this branch can return `err`
		// directly again.
		return nil, errLangparserUnsupported
	}
	converted, err := NewASTConverter().ConvertExpression(parsed)
	if err != nil {
		return nil, err
	}
	return flattenRuntimeFunctionArgs(converted), nil
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
// The detector is intentionally conservative -- it returns true
// for `@` / `:=` ANYWHERE in the source after the first character,
// not just at the structural positions where those tokens act as
// directives. Strings / identifiers / args that happen to contain
// these characters inside quoted values are protected because the
// detector skips quoted regions. False positives only cost a
// fallback to the memql parser; false negatives would surface as
// a real parse error from langparser, which is fine -- the user
// gets a clear error either way.
//
// Directive function names (paginate / sort / select / asOf /
// withDepth / shape) used to fall back here too because the
// langparser produced a generic *FunctionCallExpr for the modern
// single-paren form. That gap was closed in #254 (the langparser
// now emits *PaginateExpr / *SortExpr / *SelectExpr / *TimestampExpr
// / *DepthExpr / *ShapeExpr directly), so the directive-name guard
// is gone and these queries flow through the opt-in path.
func langparserPathUnsupported(query string) bool {
	// #249: targeted fallbacks for runtime forms whose langparser
	// parity is known incomplete. Each entry has a follow-up under
	// epic #218 to widen langparser support; while the gap exists,
	// the memql parser remains canonical for the shape.
	//
	//   shape( ... )  -- langparser shape grammar rejects array
	//      templates + executes some nested-object templates
	//      differently. (TestShapeDirective* tests.)
	//   select( ... ) -- langparser produces a correct AST but
	//      doesn't populate Plan.Fields / Plan.Metadata the way
	//      the memql parser's select handler does. (TestParseSelectDirective.)
	//   concept==memql:version -- memql parser rewrites this
	//      filter shape to a memqlVersion BuiltinFunctionExpression;
	//      langparser produces a regular ComparisonExpression.
	//      (TestParseConceptMemqlVersionCompatibility.)
	if containsRuntimeCall(query, "shape") ||
		containsRuntimeCall(query, "select") {
		return true
	}
	if containsConceptMemqlVersionShape(query) {
		return true
	}
	// `<expr> in (...)` collection operator: langparser doesn't
	// recognise the parenthesised-list form on the RHS. Memql
	// parser supports it. (TestParseCollectionOperators.)
	if containsInOperator(query) {
		return true
	}
	// Trailing comma: langparser silently absorbs it, memql parser
	// raises ErrInvalidQuerySyntax. Until langparser strict-mode
	// catches this, the memql parser is canonical so callers see
	// the right syntax-error sentinel. (TestParseErrors/UnexpectedComma.)
	if t := strings.TrimRightFunc(query, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n'
	}); strings.HasSuffix(t, ",") {
		return true
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
		// `@` outside a string: timestamp suffix (`@latest` /
		// `@"2026-01-01T..."`). e.Parse handles it post-parse on
		// the memql path; the langparser path doesn't yet.
		if c == '@' {
			return true
		}
		// `:=` outside a string: inline spec definition. Same.
		if c == ':' && i+1 < len(query) && query[i+1] == '=' {
			return true
		}
	}
	return false
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
