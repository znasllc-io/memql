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
