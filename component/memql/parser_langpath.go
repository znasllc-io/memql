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
		return nil, err
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

// directiveFunctionNames is the set of function names the runtime
// grammar treats as DIRECTIVES -- query-shape wrappers that the
// memql parser specialises into dedicated *PaginateExpression /
// *SortExpression / *SelectExpression / *TimestampExpression /
// *DepthExpression / *ShapeExpression AST nodes (see
// component/memql/parser.go around lines 1119/1201/1271/1330/1371/1415
// for the build-sites). The langparser's modern single-paren form
// produces a generic *FunctionCallExpr for the same syntax (its
// wrapDirective fires only for the legacy double-paren form), so
// the two paths diverge at the AST level for any composite query
// containing one of these names. e.Parse's applyDirectiveWrappers
// only walks the specialised types; without the divergence guard
// here, a langparser-routed `paginate(...)` would silently skip
// extracting Limit/Offset into the plan.
//
// Until the langparser learns to wrap modern directive calls
// natively (filed as a follow-up; tracked under epic #218 with the
// remaining #250 deletion), these shapes fall back to the memql
// parser through langparserPathUnsupported. Membership lookup is
// O(1) string compare against the known set.
var directiveFunctionNames = map[string]struct{}{
	"paginate":  {},
	"sort":      {},
	"select":    {},
	"asof":      {},
	"withdepth": {},
	"shape":     {},
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
// It ALSO falls back on any source where a directive function name
// (`paginate`, `sort`, `select`, `asOf`, `withDepth`, `shape`)
// appears as the head of a call -- the langparser produces a
// generic *FunctionCallExpr for those, but the engine expects the
// specialised directive expression. See directiveFunctionNames
// above for the rationale.
func langparserPathUnsupported(query string) bool {
	inStr := false
	var quote byte
	identStart := -1 // index of the first byte of an in-progress identifier, -1 if none
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
			identStart = -1
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
		// Track identifier boundaries so the directive check fires
		// on `paginate(` but not on `mutationPaginateFoo({...})` or
		// `payload.paginate=="x"`. An identifier extends while the
		// byte is an ASCII letter / digit / underscore; the first
		// non-identifier byte ends it.
		if isAccessorIdentByte(c) {
			if identStart < 0 {
				identStart = i
			}
			continue
		}
		if identStart >= 0 {
			// Identifier just ended at i-1. If the next non-space
			// byte is `(`, this is a function call -- check if the
			// name is a directive.
			ident := strings.ToLower(query[identStart:i])
			identStart = -1
			j := i
			for j < len(query) && (query[j] == ' ' || query[j] == '\t' || query[j] == '\n' || query[j] == '\r') {
				j++
			}
			if j < len(query) && query[j] == '(' {
				if _, ok := directiveFunctionNames[ident]; ok {
					return true
				}
			}
		}
	}
	// EOF while inside an identifier -- can't be a call (no `(`
	// follows), so no directive match.
	return false
}

// isAccessorIdentByte returns true if c can be part of a memql
// identifier. Matches the language parser's lexer convention --
// ASCII letter / digit / underscore. (The dotted `actor.X` form
// is one identifier token at the lexer level; the dot is treated
// as part of the identifier elsewhere, but for the directive-call
// boundary check we want the bare-name boundary so `foo.shape("x")`
// is NOT treated as a `shape(` call.)
func isAccessorIdentByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}
