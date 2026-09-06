package parser

import (
	"strings"
	"testing"
)

// render_test.go -- RenderCall's own contract. The end-to-end proof that the
// output is a form the engine accepts lives beside each caller
// (component/worker/render_parses_test.go); these are the properties the
// renderer owes on its own.

// TestRenderCallRoundTripsThroughTheParser is the one that matters: every
// value shape a caller passes must come back out as a statement this same
// package will parse. `name({...})` -- the form memql#5004 shipped in eight
// places -- cannot survive this.
func TestRenderCallRoundTripsThroughTheParser(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"noArgs", nil},
		{"emptyMap", map[string]any{}},
		{"scalars", map[string]any{
			"s": "plain", "i": 42, "f": 1.5, "b": true, "nul": nil,
		}},
		{"awkwardString", map[string]any{
			"text": "O'Brien's \"box\" <lab> & co \\ tab\there\nnewline é 🙂",
		}},
		{"nestedObject", map[string]any{
			"usage": map[string]any{"inputTokens": 12, "known": true},
		}},
		{"listOfObjects", map[string]any{
			"apps": []any{map[string]any{"id": "claude-code", "allowed": true}},
		}},
		{"emptyCollections", map[string]any{
			"list": []any{}, "obj": map[string]any{},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stmt, err := RenderCall("someMutation", c.args)
			if err != nil {
				t.Fatalf("RenderCall: %v", err)
			}
			if _, err := ParseExpression(stmt); err != nil {
				t.Fatalf("the parser refused what RenderCall produced:\n  %s\n  --> %v", stmt, err)
			}
		})
	}
}

// TestRenderCallEmptyArgsIsNotTheEmptyWrapper pins the case that reads as a
// detail and is not: `name({})` is rejected by the same rule as `name({...})`.
func TestRenderCallEmptyArgsIsNotTheEmptyWrapper(t *testing.T) {
	for _, args := range []map[string]any{nil, {}} {
		got, err := RenderCall("noop", args)
		if err != nil {
			t.Fatalf("RenderCall: %v", err)
		}
		if got != "noop()" {
			t.Errorf("empty args rendered %q, want %q", got, "noop()")
		}
	}
}

// TestRenderCallSortsKeys -- a caller's map iteration order is random, so an
// unsorted statement differs run to run and cannot be asserted on, diffed in a
// log, or hashed.
func TestRenderCallSortsKeys(t *testing.T) {
	args := map[string]any{"zulu": 1, "alpha": 2, "mike": 3}
	want := `f(alpha: 2, mike: 3, zulu: 1)`
	for i := 0; i < 32; i++ {
		got, err := RenderCall("f", args)
		if err != nil {
			t.Fatalf("RenderCall: %v", err)
		}
		if got != want {
			t.Fatalf("iteration %d rendered %q, want %q", i, got, want)
		}
	}
}

// TestRenderCallDoesNotHTMLEscape -- encoding/json escapes <, > and & by
// default, which has no meaning in a MemQL statement and makes every logged
// statement and every test failure unreadable. QuoteString made the same
// choice; they must not disagree.
func TestRenderCallDoesNotHTMLEscape(t *testing.T) {
	got, err := RenderCall("f", map[string]any{"t": "<lab> & co"})
	if err != nil {
		t.Fatalf("RenderCall: %v", err)
	}
	for _, esc := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if strings.Contains(got, esc) {
			t.Errorf("RenderCall HTML-escaped its value (%s): %s", esc, got)
		}
	}
	if want := `f(t: "<lab> & co")`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if q := QuoteString("<lab> & co"); !strings.Contains(got, q) {
		t.Errorf("RenderCall and QuoteString disagree: RenderCall gave %s, QuoteString gave %s", got, q)
	}
}

// TestRenderCallRefusesABlankName -- a blank name renders `()`, which parses
// as something else entirely rather than failing where the mistake is.
func TestRenderCallRefusesABlankName(t *testing.T) {
	for _, name := range []string{"", "   "} {
		if _, err := RenderCall(name, map[string]any{"a": 1}); err == nil {
			t.Errorf("RenderCall(%q, ...) returned no error", name)
		}
	}
}

// TestRenderCallReportsAnUnencodableValue rather than emitting a fragment that
// would corrupt the statement around it.
func TestRenderCallReportsAnUnencodableValue(t *testing.T) {
	_, err := RenderCall("f", map[string]any{"ch": make(chan int)})
	if err == nil {
		t.Fatal("RenderCall accepted an unencodable value")
	}
	if !strings.Contains(err.Error(), `"ch"`) {
		t.Errorf("the error does not name the offending argument: %v", err)
	}
}
