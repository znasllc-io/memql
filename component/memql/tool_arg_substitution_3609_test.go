package memql

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// tool_arg_substitution_3609_test.go -- memql#3609.
//
// The substituters used to loop over the args MAP doing sequential
// strings.ReplaceAll over an accumulating buffer. Two silent defects fell out
// of that:
//
//  1. Order-dependence. Go randomizes map iteration, and `$args.id` is a
//     prefix of `$args.idempotencyKey`, so the same call rendered differently
//     from run to run (~85% of runs mangled).
//  2. A caller's own VALUE became template. Substituted text sat in the buffer
//     that later keys -- and the unfilled-placeholder cleanup pass -- kept
//     scanning, so a todo titled "explain how $args.foo works" was silently
//     stored as "explain how null works", and one arg's value could be aliased
//     into another arg's slot.
//
// The property pinned here: substitution is ONE left-to-right pass over the
// template. Every placeholder is resolved from the args map exactly once,
// against the longest identifier at that position, and what the pass writes is
// never read back as template.

// substitutionRuns is the loop count for the order-independence assertions.
// Under the old map-iteration implementation the prefix pair rendered wrong in
// roughly 85% of runs, so a few hundred iterations turn "usually caught" into
// "cannot be missed".
const substitutionRuns = 500

// TestSubstituteArgs_PrefixPairIsOrderIndependent covers defect 1: an arg name
// that is a prefix of another arg name. Both functions must resolve each
// placeholder to its OWN value on every single run.
func TestSubstituteArgs_PrefixPairIsOrderIndependent(t *testing.T) {
	args := map[string]any{"id": "abc", "idempotencyKey": "K-1"}

	for i := 0; i < substitutionRuns; i++ {
		got := substituteArgsInMemqlQuery(`mutation m(id: "$args.id", key: "$args.idempotencyKey")`, args)
		if want := `mutation m(id: "abc", key: "K-1")`; got != want {
			t.Fatalf("substituteArgsInMemqlQuery run %d:\n  got:  %s\n  want: %s", i, got, want)
		}

		got = substituteArgsInQuery(`https://example.test/x/$args.id/$args.idempotencyKey`, args)
		if want := `https://example.test/x/abc/K-1`; got != want {
			t.Fatalf("substituteArgsInQuery run %d:\n  got:  %s\n  want: %s", i, got, want)
		}
	}
}

// TestSubstituteArgs_CallerValueIsNotTemplate covers defect 2, the half that
// was live in shipped `type="query"` tools (calendarFind, notesUpdate,
// todosUpdate, ...): free text containing the literal `$args.` must reach the
// handler verbatim, not rewritten to null.
func TestSubstituteArgs_CallerValueIsNotTemplate(t *testing.T) {
	const title = `explain how $args.foo placeholders work`
	args := map[string]any{"todoId": "t1", "title": title}

	got := substituteArgsInMemqlQuery(`mutation createTodo(todoId: "$args.todoId", title: "$args.title")`, args)
	want := `mutation createTodo(todoId: "t1", title: ` + languageParser.QuoteString(title) + `)`
	if got != want {
		t.Errorf("caller value was treated as template:\n  got:  %s\n  want: %s", got, want)
	}

	rawGot := substituteArgsInQuery(`https://example.test/notes?title=$args.title`, args)
	if rawWant := `https://example.test/notes?title=` + title; rawGot != rawWant {
		t.Errorf("caller value was treated as template (raw path):\n  got:  %s\n  want: %s", rawGot, rawWant)
	}
}

// TestSubstituteArgs_NoCrossArgAliasing is the same defect seen from the other
// side: a value that IS a placeholder must not pull a sibling arg's value into
// its slot. Looped, because under the old implementation which of the two
// wrong answers you got depended on map order.
func TestSubstituteArgs_NoCrossArgAliasing(t *testing.T) {
	args := map[string]any{"todoId": "$args.title", "title": "SECRET"}

	for i := 0; i < substitutionRuns; i++ {
		got := substituteArgsInMemqlQuery(`mutation createTodo(todoId: "$args.todoId", title: "$args.title")`, args)
		want := `mutation createTodo(todoId: "$args.title", title: "SECRET")`
		if got != want {
			t.Fatalf("cross-arg aliasing on run %d:\n  got:  %s\n  want: %s", i, got, want)
		}
		// The load-bearing half of the assertion above, stated on its own:
		// the todoId slot never sees the title arg's value.
		if todoID, _, _ := strings.Cut(got, ", title:"); strings.Contains(todoID, "SECRET") {
			t.Fatalf("run %d: sibling arg leaked into todoId: %s", i, got)
		}
	}
}

// TestSubstituteArgs_UnfilledPlaceholdersCollapseToNull pins the behaviour the
// cleanup pass provides -- an optional arg the LLM omitted degrades to null
// instead of leaving a bare `$` the MemQL parser chokes on. Moving the cleanup
// INSIDE the walk (it used to run over already-substituted text) must not
// change what a genuinely-unfilled placeholder renders as.
func TestSubstituteArgs_UnfilledPlaceholdersCollapseToNull(t *testing.T) {
	for _, tc := range []struct {
		name string
		tmpl string
		args map[string]any
		want string
	}{
		{
			name: "bare form becomes an unquoted null",
			tmpl: `mutation m(planId: $args.planId)`,
			args: map[string]any{},
			want: `mutation m(planId: null)`,
		},
		{
			// The whole quoted token is the string-literal slot, and the
			// cleanup only ever rewrote the placeholder inside it, so a
			// missing string arg lands as the four-char string "null".
			name: "pre-quoted form becomes the string literal null",
			tmpl: `mutation m(planId: "$args.planId")`,
			args: map[string]any{},
			want: `mutation m(planId: "null")`,
		},
		{
			name: "embedded placeholder is removed surgically",
			tmpl: `mutation m(planId: "prefix-$args.planId")`,
			args: map[string]any{},
			want: `mutation m(planId: "prefix-null")`,
		},
		{
			name: "nil args map collapses every placeholder",
			tmpl: `mutation m(a: "$args.foo", b: $args.bar)`,
			args: nil,
			want: `mutation m(a: "null", b: null)`,
		},
		{
			name: "filled and unfilled placeholders coexist",
			tmpl: `mutation m(taskId: "$args.taskId", planId: $args.planId)`,
			args: map[string]any{"taskId": "t-9"},
			want: `mutation m(taskId: "t-9", planId: null)`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := substituteArgsInMemqlQuery(tc.tmpl, tc.args); got != tc.want {
				t.Errorf("substituteArgsInMemqlQuery:\n  got:  %s\n  want: %s", got, tc.want)
			}
		})
	}

	// The raw (webhook URL / body-template) path has never had the cleanup
	// pass: an unfilled placeholder stays literal there.
	got := substituteArgsInQuery(`https://example.test/$args.id?q=$args.missing`, map[string]any{"id": "abc"})
	if want := `https://example.test/abc?q=$args.missing`; got != want {
		t.Errorf("substituteArgsInQuery:\n  got:  %s\n  want: %s", got, want)
	}
}

// TestSubstituteArgs_NonStringValueEncodings guards the encodings the two paths
// disagree on -- the MemQL path quotes strings for a parser that will re-read
// them, the raw path hands back the bare stringified value for a host-language
// slot that already has its own quoting.
func TestSubstituteArgs_NonStringValueEncodings(t *testing.T) {
	args := map[string]any{
		"n":       float64(3),
		"f":       2.5,
		"b":       true,
		"payload": map[string]any{"k": "v"},
	}

	got := substituteArgsInMemqlQuery(`mutation m(n: $args.n, f: $args.f, b: $args.b, p: $args.payload)`, args)
	if want := `mutation m(n: 3, f: 2.5, b: true, p: {"k":"v"})`; got != want {
		t.Errorf("substituteArgsInMemqlQuery:\n  got:  %s\n  want: %s", got, want)
	}

	got = substituteArgsInQuery(`https://example.test/$args.n/$args.f/$args.b/$args.payload`, args)
	if want := `https://example.test/3/2.5/true/{"k":"v"}`; got != want {
		t.Errorf("substituteArgsInQuery:\n  got:  %s\n  want: %s", got, want)
	}
}

// TestSubstituteArgs_RenderedQueryLexes closes the loop on the reason the MemQL
// path quotes at all: whatever it emits is fed straight to the parser. A value
// carrying quotes, backslashes and a `$args.` reference must still lex, and
// must lex back to exactly the caller's text.
func TestSubstituteArgs_RenderedQueryLexes(t *testing.T) {
	const title = `a "quoted" \ $args.title value`

	rendered := substituteArgsInMemqlQuery(`mutation m(title: "$args.title")`, map[string]any{"title": title})

	toks, err := languageParser.NewLexer(rendered).Tokenize()
	if err != nil {
		t.Fatalf("lexer rejected rendered query %q: %v", rendered, err)
	}
	var found bool
	for _, tok := range toks {
		if tok.Type == languageParser.TokenString && tok.Literal == title {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("rendered query %q does not carry the caller's title back verbatim", rendered)
	}
}
