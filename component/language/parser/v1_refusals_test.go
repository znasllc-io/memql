package parser

// v1_refusals_test.go -- what the edition-2026 expression parser refuses, and
// how: a retired spelling names its replacement and the migrator that writes
// it, and every other refusal names the fix and where it is.
//
// memqlmigrate:keep-file -- the retired spellings in this file are its cases,
// so the fixture codemod must leave them as written.

import (
	"errors"
	"strings"
	"testing"
)

// v1RetiredSamples is one input per retired form, plus extra positions for the
// forms that can appear in more than one place. TestV1EveryRetiredFormHasASample
// holds the table and this list together. A FILE sample is a whole authored
// construct, parsed the way the engine parses one (the struct-form rewriter,
// then the parser): the predicate positions' legacy forms are refused only
// there.
var v1RetiredSamples = []struct {
	rule string
	src  string
	file bool
}{
	{"retired_when_guard", `row.a == 1 && when(args.x) { row.f == args.x }`, false},
	{"retired_when_guard", `p ? when(args.x) { a } : b`, false},
	{"retired_conditional_prefix", `?.status == args.status`, false},
	{"retired_conditional_prefix", `row.a == 1 && ?.b == args.b`, false},
	{"retired_semicolon_connective", `row.a == 1; row.b == 2`, false},
	{"retired_semicolon_connective", `f(a; b)`, false},
	{"retired_semicolon_connective", `(a == 1; b == 2)`, false},
	{"retired_comma_connective", `row.a == 1, row.b == 2`, false},
	{"retired_comma_connective", `(a == 1, b == 2)`, false},
	{"retired_comma_connective", `x => x.a, x.b`, false},
	// A comma after a predicate clause's lambda is the connective too, at each
	// of the four clauses whose lambda the construct parser reads mid-stream
	// (memql#5369: each used to report "expected )" and name no fix).
	{"retired_comma_connective", "query thing probe {\n  filter row => row.a == 1, row.b == 2\n}", true},
	{"retired_comma_connective", "query thing probe {\n  filter row => row.a == 1\n  paginate 5\n  refine row => row.a == 1, row.b == 2\n}", true},
	{"retired_comma_connective", "spec thing isX = row => row.a == 1, row.b == 2", true},
	{"retired_comma_connective", "@filter(row => row.a == 1, row.b == 2)\n@trigger(event=\"node.created\", concept=\"v1:probe:thing\")\nautomation probe {\n  step s {\n    logic f(x: 1)\n  }\n}", true},
	{"retired_has", `row.tags has "x"`, false},
	{"retired_has", `has x`, false},
	{"retired_has", `xs.any(x => x has 1)`, false},
	{"retired_not_in", `row.status not in ["a", "b"]`, false},
	{"retired_and_call", `and(a, b)`, false},
	{"retired_or_call", `or(a, b)`, false},
	{"retired_not_call", `not(a)`, false},
	{"retired_not_call", `not a`, false},
	{"retired_lt_call", `lt(a, b)`, false},
	{"retired_gt_call", `gt(a, b)`, false},
	{"retired_lte_call", `lte(a, b)`, false},
	{"retired_gte_call", `gte(a, b)`, false},
	{"retired_cond_call", `cond(p, a, b)`, false},
	{"retired_cond_call", `[cond(a == 1, "x", "y")]`, false},
	{"retired_concat_call", `concat(a, b)`, false},
	{"retired_concat_call", `CONCAT(a, b)`, false},
	{"retired_coalesce_call", `coalesce(a, b)`, false},
	{"retired_coalesce_call", `Coalesce(a, b)`, false},
	{"retired_exists_call", `exists(x)`, false},
	{"retired_len_call", `len(x)`, false},
	{"retired_len_call", `{a: len(x)}`, false},
	{"retired_count_call", `count(x)`, false},
	{"retired_contains_call", `contains(s, "sub")`, false},
	{"retired_mean_call", `mean(xs)`, false},
	{"retired_mean_call", `row.a > mean(args.scores)`, false},
	{"retired_first_call", `first(xs)`, false},
	{"retired_last_call", `last(xs)`, false},
	{"retired_timestamp_call", `timestamp()`, false},
	{"retired_now_call", `now()`, false},
	{"retired_null", `row.a == null`, false},
	{"retired_null", `null`, false},
	{"retired_null", `null.x`, false},
	{"retired_dollar_args", `$args.x == 1`, false},
	{"retired_dollar_args", `row.a == $args.x`, false},
	{"retired_spec_reference", `spec isActive`, false},
	{"retired_spec_reference", `row.a == 1 && spec isActive`, false},
	{"retired_trait_reference", `trait isActiveRecord`, false},
	{"retired_contains_method", `row.tags.contains("x")`, false},
	{"retired_contains_method", `f(x).contains(y)`, false},
	{"retired_keyless_map_entry", `{delegationId: args.event.payload.id, args.event.payload.identityId, timestamp: now}`, false},
	{"retired_keyless_map_entry", `{a}`, false},
	{"retired_keyless_map_entry", `f(x: {a: 1, row.b})`, false},
	{"retired_filter_without_lambda", "query thing probe {\n  filter a == 1\n}", true},
	{"retired_filter_without_lambda", "query thing probe {\n  filter a == 1 && isX\n  paginate 5\n  shape probeCard\n}", true},
	{"retired_spec_return_body", "spec thing isX {\n  return a == 1\n}", true},
	{"retired_trait_return_body", "trait isX {\n  return a == 1\n}", true},
	{"retired_filter_annotation", "@filter(payload.a == 1)\n@trigger(event=\"node.created\", concept=\"v1:probe:thing\")\nautomation probe {\n  step s {\n    logic f(x: 1)\n  }\n}", true},
	{"retired_filter_annotation", "@trigger(event=\"node.created\", concept=\"v1:probe:thing\", filter=\"payload.a == 1\")\nautomation probe {\n  step s {\n    logic f(x: 1)\n  }\n}", true},
}

// v1ConcreteRetiredMessages are the refusals that name the author's own entry
// written out instead of the table's placeholder form: a key-less map entry's
// fix is that entry with its key, which is also what the codemod writes.
var v1ConcreteRetiredMessages = map[string]string{
	`{delegationId: args.event.payload.id, args.event.payload.identityId, timestamp: now}`: "a map entry needs a key: write identityId: args.event.payload.identityId (memqlmigrate --rewrite=expressions rewrites it)",
	`{a}`:                 "a map entry needs a key: write a: a (memqlmigrate --rewrite=expressions rewrites it)",
	`f(x: {a: 1, row.b})`: "a map entry needs a key: write b: row.b (memqlmigrate --rewrite=expressions rewrites it)",
}

// TestV1RetiredFormsRefuse: every retired spelling refuses with the pinned
// message shape, carrying its rule, its replacement, the migrator and a
// position.
func TestV1RetiredFormsRefuse(t *testing.T) {
	forms := map[string]RetiredForm{}
	for _, f := range V1RetiredForms() {
		forms[f.Rule] = f
	}
	for _, c := range v1RetiredSamples {
		t.Run(c.rule+"/"+c.src, func(t *testing.T) {
			form, ok := forms[c.rule]
			if !ok {
				t.Fatalf("no retired form has rule %q", c.rule)
			}
			var err error
			if c.file {
				_, err = parseV1Authored(t, c.src)
			} else {
				_, err = ParseV1Expression(c.src)
			}
			if err == nil {
				t.Fatalf("%q was accepted, a retired form", c.src)
			}
			var rf *RetiredFormError
			if !errors.As(err, &rf) {
				t.Fatalf("want a *RetiredFormError, got %T: %v", err, err)
			}
			if rf.Form.Rule != c.rule {
				t.Fatalf("refused under rule %q, want %q: %v", rf.Form.Rule, c.rule, err)
			}
			msg := err.Error()
			if want, concrete := v1ConcreteRetiredMessages[c.src]; concrete {
				if !strings.Contains(msg, want) {
					t.Fatalf("message does not name the entry written out:\n got  %s\n want %s", msg, want)
				}
			} else {
				want := form.Spelling + " is retired in edition 2026: write " + form.Replacement + " (memqlmigrate --rewrite=expressions rewrites it)"
				if !strings.Contains(msg, want) {
					t.Fatalf("message does not carry the pinned shape:\n got  %s\n want %s", msg, want)
				}
				if !strings.Contains(msg, form.Replacement) {
					t.Fatalf("message must name the replacement: %s", msg)
				}
			}
			if !strings.Contains(msg, "memqlmigrate --rewrite=expressions") {
				t.Fatalf("message must name the migrator: %s", msg)
			}
			var pe *ParseError
			if !errors.As(err, &pe) || pe.Line < 1 || pe.Column < 1 {
				t.Fatalf("the refusal carries no line/column: %v", err)
			}
			if !errors.Is(err, ErrInvalidSyntax) {
				t.Fatalf("a refusal must unwrap to ErrInvalidSyntax: %v", err)
			}
		})
	}
}

// TestV1EveryRetiredFormHasASample: a table entry with no sample is a refusal
// nothing proves, and a sample naming no entry tests a rule that does not exist.
func TestV1EveryRetiredFormHasASample(t *testing.T) {
	sampled := map[string]bool{}
	for _, c := range v1RetiredSamples {
		sampled[c.rule] = true
	}
	seen := map[string]bool{}
	for _, f := range V1RetiredForms() {
		if f.Rule == "" || f.Spelling == "" || f.Replacement == "" {
			t.Errorf("incomplete retired form %+v", f)
		}
		if seen[f.Rule] {
			t.Errorf("rule %q appears twice in the table", f.Rule)
		}
		seen[f.Rule] = true
		if !sampled[f.Rule] {
			t.Errorf("retired form %q (%s) has no sample input in v1RetiredSamples", f.Rule, f.Spelling)
		}
	}
	for rule := range sampled {
		if !seen[rule] {
			t.Errorf("a sample names rule %q, which is not in V1RetiredForms()", rule)
		}
	}
	// The table is data other packages read; a caller editing its copy must
	// not edit the parser's.
	forms := V1RetiredForms()
	forms[0].Replacement = "changed"
	if V1RetiredForms()[0].Replacement == "changed" {
		t.Error("V1RetiredForms returns the parser's own slice, not a copy")
	}
}

// TestV1ParseErrors: every other refusal names what to write instead.
func TestV1ParseErrors(t *testing.T) {
	cases := []struct {
		src  string
		want []string // each must appear in the error text
	}{
		// Comparisons are non-associative.
		{`a < b < c`, []string{"comparisons do not chain: parenthesise one side"}},
		{`a == b != c`, []string{"comparisons do not chain"}},
		{`a in b in c`, []string{"comparisons do not chain"}},
		{`a startsWith b == c`, []string{"comparisons do not chain"}},
		// `=` is never a comparison.
		{`a = 1`, []string{"`=` is not a comparison: write `==`"}},
		{`row.a = "x" && b`, []string{"write `==`"}},
		{`f(a = 1)`, []string{"a named argument is written `a: ...`"}},
		// A hyphen glued into a name is one name, never subtraction.
		{`remaining == total-used`, []string{"`total-used` reads as one name; write `total - used` (spaces) for subtraction"}},
		// The suggestion repeats the object path on the right: a bare `used`
		// is refused in turn, so `row.total - used` is not a fix.
		{`row.total-used > 0`, []string{"`row.total-used` reads as one name; write `row.total - row.used` (spaces)"}},
		{`args.a.total-used-spent`, []string{"write `args.a.total - args.a.used - args.a.spent`"}},
		{`args.a-args.b`, []string{"write `args.a - args.b`"}},
		{`row.n-1`, []string{"write `row.n - 1`"}},
		{`f(x).a-b`, []string{"reads as one name", "write `.a - b`"}},
		{`total-used(x)`, []string{"reads as one name"}},
		{`{row.total-used}`, []string{"write `row.total - row.used`"}},
		{`(a-b) => a`, []string{"a lambda parameter is a simple name"}},
		// A colon glued into a name is a canonical id or a missing space.
		{`row.concept == v1:crm:lead`, []string{`a canonical id in an expression is written as a string: "v1:crm:lead"`}},
		{`f(a:b)`, []string{`a canonical id in an expression is written as a string: "a:b"`, "a: b"}},
		{`{a:b}`, []string{"a: b"}},
		{`p ? f(x).a:b`, []string{"put a space after it: .a: b"}},
		// Map literals. A key-less entry is a retired form (v1RetiredSamples);
		// a dotted KEY is not, and nesting is its fix.
		{`{"a": 1}`, []string{"authoring rule 18"}},
		{`{a: 1, a: 2}`, []string{"duplicate key", "a"}},
		{`{a.b: 1}`, []string{"a map key is one name, got `a.b`: nest a map for a path"}},
		{`{in}`, []string{"key: value"}},
		// Call arguments.
		{`f(a, b: 1)`, []string{"all positional or all named", "a: a"}},
		{`f(1, b: 1)`, []string{"all positional or all named"}},
		{`f(a: 1, a: 2)`, []string{"duplicate argument"}},
		{`f(now: 1)`, []string{"reserved engine name"}},
		{`query x(1)`, []string{"positional args are removed on construct calls"}},
		{`query x(1, a: 2)`, []string{"positional args are removed on construct calls"}},
		{`logic x(a, a: 1)`, []string{"duplicate argument"}},
		{`logic x(a: 1, a)`, []string{"duplicate argument"}},
		{`logic x(now)`, []string{"reserved engine name"}},
		// The retired object-literal wrapper (memql#2335).
		{`f({k: 1})`, []string{"object-literal call args are removed; pass named args directly: f(k: v, ...)"}},
		{`query x({k: 1})`, []string{"object-literal call args are removed; pass named args directly: x(k: v, ...)"}},
		{`ensureDailySpaceForUser({userId: args.id})`, []string{"object-literal call args are removed"}},
		{`f("a": 1)`, []string{"name is not quoted"}},
		{`query activeUsers`, []string{"argument list"}},
		{`mutate createNode(id: "x")`, []string{"not a construct-invocation kind", "did you mean 'mutation'"}},
		{`query a.b(x: 1)`, []string{"construct name is a simple identifier"}},
		// Lambda parameters.
		{`(a.b) => 1`, []string{"a lambda parameter is a simple name"}},
		{`a.b => 1`, []string{"a lambda parameter is a simple name"}},
		{`(a, a) => 1`, []string{"lambda parameter `a` appears twice"}},
		{`true => 1`, []string{"`true` is a value"}},
		{`f(x) => 1`, []string{"`=>` follows a lambda's parameters"}},
		{`x =>`, []string{"expected an expression after `=>`"}},
		// Stray continuations.
		{`x not startsWith "a"`, []string{"!(x startsWith"}},
		{`a;`, []string{"trailing `;`"}},
		{`x.?0`, []string{"`.?` reads a named field"}},
		{`row.?any(x => x)`, []string{"`.?` reads a field; a method is called with `.`"}},
		{`a . b`, []string{"no space"}},
		{`x[0]`, []string{"indexing is not an operator"}},
		{`a -5`, []string{"`-5` directly after an operand", "`- 5`"}},
		{`5-3`, []string{"directly after an operand"}},
		{`a ?.b`, []string{"optional member access is written `.?`"}},
		{`args.items.0(x)`, []string{"`0` is not a method name"}},
		{`٣(x)`, []string{"is not a function name"}},
		{`a b`, []string{"unexpected `b` after the expression"}},
		{`"a" "b"`, []string{"unexpected"}},
		{`.x == 1`, []string{"a member needs an object"}},
		{`()`, []string{"empty parentheses"}},
		{``, []string{"expected an expression, got end of input"}},
		{`)`, []string{"expected an expression"}},
		{`!`, []string{"expected an expression after `!`"}},
		{`a && || b`, []string{"expected an expression after `&&`"}},
		// Unterminated forms name the opener and where it is.
		{`a ==`, []string{"line 1, column 5", "expected an expression after `==`, got end of input"}},
		{`(a == 1`, []string{"line 1, column 8", "expected `)` to close the `(` at line 1, column 1"}},
		{`f(a`, []string{"expected `,` or `)`"}},
		{`[1, 2`, []string{"expected `,` or `]`"}},
		{`{a: 1`, []string{"expected `,` or `}`"}},
		{`p ? a`, []string{"expected `:`"}},
		{"a &&\n  (b ==", []string{"line 2, column 8", "expected an expression after `==`"}},
		// Nesting is bounded, so pathological input is a refusal, not a
		// crashed process.
		{strings.Repeat("(", 5000) + "a" + strings.Repeat(")", 5000), []string{"nests too deeply"}},
		{strings.Repeat("!", 5000) + "a", []string{"nests too deeply"}},
	}
	for _, c := range cases {
		name := c.src
		if len(name) > 40 {
			name = name[:40]
		}
		t.Run(name, func(t *testing.T) {
			_, err := ParseV1Expression(c.src)
			if err == nil {
				t.Fatalf("ParseV1Expression(%q) accepted", c.src)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not contain %q:\n %v", w, err)
				}
			}
			var rf *RetiredFormError
			if errors.As(err, &rf) {
				t.Errorf("a plain parse error was reported as the retired form %q: %v", rf.Form.Rule, err)
			}
		})
	}
}

// TestV1MidStreamRefusesMistakes: a token that can never continue an
// expression in ANY construct is refused in place, not left for the caller to
// report as something vaguer.
func TestV1MidStreamRefusesMistakes(t *testing.T) {
	for _, src := range []string{`a = b {`, `a has b }`, `a; b`, `a ?.b )`, `x[0] }`, `f(a) => b }`, `row.a -5 }`, `a not in b }`, `a $args.x }`} {
		t.Run(src, func(t *testing.T) {
			tokens, err := NewLexer(src).Tokenize()
			if err != nil {
				t.Fatalf("tokenize: %v", err)
			}
			if _, err := NewParser(tokens).parseV1Expression(); err == nil {
				t.Fatalf("parseV1Expression(%q) accepted a mistaken continuation", src)
			}
		})
	}
}

// TestV1ContainsDiscriminatesByShape: contains is the graph traversal with a
// lambda and the retired substring test without one.
func TestV1ContainsDiscriminatesByShape(t *testing.T) {
	for _, ok := range []string{
		`contains(p => p.active)`,
		`contains("label", p => p.active)`,
		`contains("filedUnder", r => r.id == args.x)`,
		`contains((p) => p.a)`,
	} {
		if _, err := ParseV1Expression(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{`contains(s, "x")`, `contains(row.title, args.q)`} {
		_, err := ParseV1Expression(bad)
		var rf *RetiredFormError
		if !errors.As(err, &rf) || rf.Form.Rule != "retired_contains_call" {
			t.Errorf("%s: want the retired_contains_call refusal, got %v", bad, err)
		}
	}
	// The METHOD cannot know its receiver's type, so its refusal names both
	// replacements: membership for a list, includes for a string.
	_, err := ParseV1Expression(`row.tags.contains("x")`)
	if err == nil {
		t.Fatal("the .contains method was accepted")
	}
	for _, want := range []string{"v in <list>", "s.includes(sub)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the .contains refusal does not name %q: %v", want, err)
		}
	}
}

// TestV1KeylessEntryInPunPosition: where a map is a construct call's
// arguments in map clothing (a step config's args), a lone name is a pun, and
// a path is still an entry with no key -- refused with the entry written out.
func TestV1KeylessEntryInPunPosition(t *testing.T) {
	parse := func(src string) error {
		t.Helper()
		tokens, err := NewLexer(src).Tokenize()
		if err != nil {
			t.Fatalf("tokenize %q: %v", src, err)
		}
		_, err = NewParser(tokens).parseV1MapWith(true)
		return err
	}
	if err := parse(`{x, y: 1}`); err != nil {
		t.Fatalf("a pun where the position admits one was refused: %v", err)
	}
	err := parse(`{delegationId: args.event.payload.id, args.event.payload.identityId}`)
	var rf *RetiredFormError
	if !errors.As(err, &rf) || rf.Form.Rule != "retired_keyless_map_entry" {
		t.Fatalf("a path with no key must be refused as a key-less entry, got %v", err)
	}
	if want := "a map entry needs a key: write identityId: args.event.payload.identityId (memqlmigrate --rewrite=expressions rewrites it)"; !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want the message to contain %q", err, want)
	}
}
