package parser

import (
	"errors"
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
	src := `logic logicReadsAsOf {
  return asOf(concept == "v1:cluster:node", latest)
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
	src := `automation autoReadsAsOf {
  x := asOf(concept == "v1:cluster:node", latest)
}`
	_, err := ParseFile(src)
	if err == nil {
		t.Fatalf("expected a parse error for asOf in an automation body, got nil")
	}
	if !strings.Contains(err.Error(), "query-only") {
		t.Fatalf("expected query-only error, got: %v", err)
	}
}

// TestAsOfRejectedInSpecAndTraitBodies: a spec or trait body is a predicate
// over one row, so an `asOf(...)` in its lambda is refused with the
// query-only message a logic body gets, at the `asOf` the author wrote -- in
// a whole file, in the one-declaration slice the engine's spec loader parses
// (anchored to the file's line), and inside a nested lambda. Unrefused, the
// engine read it as a predicate applied to two arguments.
func TestAsOfRejectedInSpecAndTraitBodies(t *testing.T) {
	file := "use probe.concepts.{ thing }\n\n" +
		"spec thing readsAsOf = row => row.active == true && asOf(row.id != \"\", latest)\n"
	trait := "trait readsAsOf = row => asOf(row.active == true, latest)\n"
	nested := "spec thing readsAsOf = row => row.items.any(i => asOf(i.ok == true, latest))\n"
	slice := "spec thing readsAsOf = row => asOf(row.active == true, latest)"
	for _, tc := range []struct {
		name, src, kind string
		line            int // the file line the source starts on
		parse           func(string) error
	}{
		{"a spec in a file", file, "spec", 1, func(s string) error { _, err := ParseFile(s); return err }},
		{"a trait in a file", trait, "trait", 1, func(s string) error { _, err := ParseFile(s); return err }},
		{"inside a nested lambda", nested, "spec", 1, func(s string) error { _, err := ParseFile(s); return err }},
		{"a spec slice anchored at its file line", slice, "spec", 7, func(s string) error { _, err := ParseSpecDecl(AnchorSource(s, 7)); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.parse(tc.src)
			want := "`asOf` is a query-only clause and cannot appear in a " + tc.kind + " body"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("want the refusal %q, got %v", want, err)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("the refusal carries no position: %v", err)
			}
			wantLine, wantCol := authoredAt(t, tc.src, "asOf", 1)
			wantLine += tc.line - 1
			if line, col := pe.Position(); line != wantLine || col != wantCol {
				t.Errorf("refused at %d:%d, want %d:%d, the author's `asOf`", line, col, wantLine, wantCol)
			}
		})
	}

	// The same lambda parsed context-free -- the engine's own fixtures build
	// one this way -- is not a spec body, and stays permissive, as a
	// standalone expression does: the rule is the declaration's.
	if _, err := ParseV1Lambda("row => asOf(row.active == true, latest)"); err != nil {
		t.Errorf("a context-free lambda was refused: %v", err)
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
