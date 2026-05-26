package memql

import (
	"fmt"
	"strings"
)

// parseStandaloneExpression parses a single MemQL expression string
// through the language parser + AST converter. Returns
// `ErrEmptyQuery` for blank input, `ErrUnsupportedQueryShape`
// (wrapped) for the 8 shapes the langparser doesn't handle, or a
// raw langparser error otherwise.
//
// Pre-#328 this routed through the legacy in-package recursive-
// descent parser (`tokenize` + `newParser`); both were retired in
// #328's parser.go trim. The body is now a thin wrapper around
// `parseViaLangparser` (parser_langpath.go) -- the same path
// `MemQLEngine.Parse` uses for the full runtime query surface.
//
// Kept as its own function purely so its one external caller
// (mutation_functions_test.go's
// `TestParseStandaloneExpression_BareSpecRejected`) doesn't need
// to reach into parser_langpath.go.
func parseStandaloneExpression(input string) (ExpressionNode, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return nil, fmt.Errorf("expression is empty")
	}
	return parseViaLangparser(trimmed)
}

// detectSIUsage walks an ExpressionNode tree and reports whether
// any node in the tree is an *SIExpression. Used by spec_parser.go
// (still alive pending #329's Stage 1C migration) at registration
// time to populate `Spec.UsesSI`, which downstream callers (the
// planner's SI budget gate, observability) read to decide whether
// to wrap a spec evaluation in the SI-call envelope.
func detectSIUsage(expr ExpressionNode) bool {
	if expr == nil {
		return false
	}
	switch node := expr.(type) {
	case *SIExpression:
		return true
	case *LogicalExpression:
		return detectSIUsage(node.Left) || detectSIUsage(node.Right)
	case *RelationshipExpression:
		return detectSIUsage(node.Target)
	default:
		_ = node
		return false
	}
}
