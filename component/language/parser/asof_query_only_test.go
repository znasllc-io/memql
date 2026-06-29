package parser

import (
	"strings"
	"testing"
)

// asof_query_only_test.go pins the temporal-access (`asOf`) visibility
// rule from the core-builtins ADR §2.3 (story memql#2305): `asOf` is a
// query-only clause. It is rejected in logic / automation / mutation
// bodies and allowed in queries; a standalone expression (runtime query
// string) stays permissive.

// TestAsOfRejectedInLogicBody asserts a logic body that calls `asOf`
// fails to parse with the query-only migration message.
func TestAsOfRejectedInLogicBody(t *testing.T) {
	src := `func (Logic) logicReadsAsOf(ctx any) (any, error) {
  return asOf(concept==v1:cluster:node, latest)
}`
	_, err := ParseFile(src)
	if err == nil {
		t.Fatalf("expected a parse error for asOf in a logic body, got nil")
	}
	if !strings.Contains(err.Error(), "query-only") {
		t.Fatalf("expected query-only error, got: %v", err)
	}
}

// TestAsOfRejectedInAutomationBody asserts an automation body that calls
// `asOf` fails to parse.
func TestAsOfRejectedInAutomationBody(t *testing.T) {
	src := `func (Automation) autoReadsAsOf(ctx any) (any, error) {
  x := asOf(concept==v1:cluster:node, latest)
  return x
}`
	_, err := ParseFile(src)
	if err == nil {
		t.Fatalf("expected a parse error for asOf in an automation body, got nil")
	}
	if !strings.Contains(err.Error(), "query-only") {
		t.Fatalf("expected query-only error, got: %v", err)
	}
}

// TestAsOfAllowedInQueryBody asserts the same clause parses cleanly in a
// query body (the only legal home for `asOf`). The procedural query form
// returns the `(value, error)` pair the struct-form rewriter emits.
func TestAsOfAllowedInQueryBody(t *testing.T) {
	src := `func (Query) queryReadsAsOf(ctx any) (any, error) {
  return asOf(concept==v1:cluster:node, latest), nil
}`
	if _, err := ParseFile(src); err != nil {
		t.Fatalf("asOf in a query body must parse, got: %v", err)
	}
}

// TestAsOfAllowedInStandaloneExpression asserts a context-free
// expression (a runtime / handwritten query string) keeps working --
// the gate only fires for an explicit non-query receiver.
func TestAsOfAllowedInStandaloneExpression(t *testing.T) {
	if _, err := ParseExpression(`asOf(concept==v1:cluster:node, latest)`); err != nil {
		t.Fatalf("standalone asOf expression must parse, got: %v", err)
	}
}
