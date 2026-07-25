package parser

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// jsonDump renders an expression tree for a failure message. Only used
// on the failure path; DeepEqual is the assertion.
func jsonDump(t *testing.T, n ExpressionNode) string {
	t.Helper()
	b, err := json.Marshal(n)
	if err != nil {
		return "<unmarshalable>"
	}
	return string(b)
}

// The #2766 codemod: fold `coalesce(a, b, c)` down to the `??` shorthand
// #2611 shipped. `a ?? b ?? c` folds n-ary into ONE CoalesceExpr, so the
// rewrite is AST-identical to the call spelling -- the tests below pin the
// LEXICAL contract; TestNullCoalesceMigrate_ASTEquivalence pins the
// semantic one.
//
// The precedence #2611 chose (tighter than comparison, looser than
// additive) makes two adjacencies actively WRONG to rewrite, and the
// skip-cases below are the load-bearing half of this suite.

func TestRewriteNullCoalesce_Rewrites(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "object-literal field value",
			in:   "    active: coalesce(args.active, true)",
			want: "    active: args.active ?? true",
		},
		{
			name: "three args fold into one chain",
			in:   `    supersededReason: coalesce(args.reason, args.supersededReason, "")`,
			want: `    supersededReason: args.reason ?? args.supersededReason ?? ""`,
		},
		{
			name: "return operand",
			in:   "    return coalesce(args.gate.passed, false)",
			want: "    return args.gate.passed ?? false",
		},
		{
			name: "walrus assignment RHS",
			in:   `    st := coalesce(args.status, "")`,
			want: `    st := args.status ?? ""`,
		},
		{
			name: "call-argument position keeps the comma structure",
			in:   `  x := concat("P-", coalesce(w.first().payload.value, "90"), "D")`,
			want: `  x := concat("P-", w.first().payload.value ?? "90", "D")`,
		},
		{
			name: "nested coalesce flattens to one chain",
			in:   "    slug: coalesce(args.a, coalesce(args.b, args.c))",
			want: "    slug: args.a ?? args.b ?? args.c",
		},
		{
			name: "commas and parens inside a string literal are not arg separators",
			in:   `    label: coalesce(args.label, "a, b (c)")`,
			want: `    label: args.label ?? "a, b (c)"`,
		},
		{
			name: "two independent calls on one line both rewrite",
			in:   "    a: coalesce(args.x, 1), b: coalesce(args.y, 2)",
			want: "    a: args.x ?? 1, b: args.y ?? 2",
		},
		{
			name: "trailing line comment is preserved",
			in:   "    seq: coalesce(args.seq, 0) // ordering",
			want: "    seq: args.seq ?? 0 // ordering",
		},
		{
			name: "arithmetic INSIDE an arg is safe -- ?? is looser than additive",
			in:   "    n: coalesce(args.n, args.base + 1)",
			want: "    n: args.n ?? args.base + 1",
		},
		{
			// memql#2772 widened `??` to the row-id slot.
			name: "id field of a write block",
			in:   "    id: coalesce(args.roleId, args.slug)",
			want: "    id: args.roleId ?? args.slug",
		},
		{
			// ...and to step-call named arguments.
			name: "named argument of a step call",
			in:   `    return query q( userId: coalesce(args.id, ""), full: true )`,
			want: `    return query q( userId: args.id ?? "", full: true )`,
		},
		{
			// ...and to brace / bracket literal arms, which the payload
			// scanner now measures with the same depth it gives parens.
			name: "empty object literal fallback",
			in:   "    labels: coalesce(args.labels, {})",
			want: "    labels: args.labels ?? {}",
		},
		{
			name: "empty array literal fallback",
			in:   "    tags: coalesce(args.tags, [])",
			want: "    tags: args.tags ?? []",
		},
		{
			name: "populated object literal fallback",
			in:   `    source: coalesce(args.source, { inputMethod: "si" })`,
			want: `    source: args.source ?? { inputMethod: "si" }`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RewriteNullCoalesce([]byte(tc.in))
			if err != nil {
				t.Fatalf("RewriteNullCoalesce: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// The skip matrix. Each row is a shape where the `??` spelling would
// parse to something DIFFERENT from the call spelling, or where the
// rewrite cannot be performed lexically. memqlmigrate is documented as
// conservative: it leaves these exactly as written.
func TestRewriteNullCoalesce_SkipsUnsafe(t *testing.T) {
	cases := []struct {
		name, in, why string
	}{
		{
			name: "arithmetic operand after the call",
			in:   "    n: coalesce(args.n, 0) + 1",
			why:  "`a ?? 0 + 1` binds as `a ?? (0 + 1)` -- ?? is looser than additive",
		},
		{
			name: "arithmetic operand before the call",
			in:   "    n: args.base + coalesce(args.n, 0)",
			why:  "`base + a ?? 0` binds as `base + (a ?? 0)`'s sum being coalesced",
		},
		{
			name: "unary not operand",
			in:   "    ok: !coalesce(args.flag, false)",
			why:  "`!a ?? false` binds as `(!a) ?? false`",
		},
		{
			name: "comparison inside an arg",
			in:   `    ok: coalesce(args.a == args.b, false)`,
			why:  "`a == b ?? false` binds as `a == (b ?? false)` -- ?? is tighter than comparison",
		},
		{
			name: "logical operator inside an arg",
			in:   "    ok: coalesce(args.a && args.b, false)",
			why:  "`a && b ?? false` binds as `a && (b ?? false)`",
		},
		{
			name: "call does not close on this line",
			in:   "    id: coalesce(",
			why:  "multi-line call -- the line scanner cannot see the arg list",
		},
		{
			name: "single argument",
			in:   "    id: coalesce(args.id)",
			why:  "nothing to fold; a 1-arg call has no fallback arm",
		},
		{
			name: "inside a line comment",
			in:   "    //      via `coalesce(args.X, <v>)`.",
			why:  "prose, not code",
		},
		{
			name: "identifier suffix is not the builtin",
			in:   "    x: mycoalesce(args.a, args.b)",
			why:  "word boundary -- a different function",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RewriteNullCoalesce([]byte(tc.in))
			if err != nil {
				t.Fatalf("RewriteNullCoalesce: %v", err)
			}
			if string(got) != tc.in {
				t.Errorf("must skip (%s)\n got %q\nwant %q (unchanged)", tc.why, got, tc.in)
			}
		})
	}
}

// A `/* ... */` span is prose too, including one opened on an earlier
// line. The discriminating shape (#2615/#2658): a `/*` inside a LINE
// comment must not open a span, or a real `*/` downstream phantom-blanks
// live corpus -- here that would mean silently skipping a real call site.
func TestRewriteNullCoalesce_BlockComments(t *testing.T) {
	span := "/* note:\n   x: coalesce(args.a, 1)\n*/\ny: coalesce(args.b, 2)\n"
	wantSpan := "/* note:\n   x: coalesce(args.a, 1)\n*/\ny: args.b ?? 2\n"
	got, err := RewriteNullCoalesce([]byte(span))
	if err != nil {
		t.Fatalf("RewriteNullCoalesce: %v", err)
	}
	if string(got) != wantSpan {
		t.Errorf("block comment span:\n got %q\nwant %q", got, wantSpan)
	}

	// `/*` inside a // comment does NOT open a span: the call below it
	// is live code and must still rewrite.
	glob := "// maps scripts/* path\nz: coalesce(args.c, 3)\n"
	wantGlob := "// maps scripts/* path\nz: args.c ?? 3\n"
	got, err = RewriteNullCoalesce([]byte(glob))
	if err != nil {
		t.Fatalf("RewriteNullCoalesce: %v", err)
	}
	if string(got) != wantGlob {
		t.Errorf("line-comment glob:\n got %q\nwant %q", got, wantGlob)
	}

	// Same for a `/*` inside a string literal.
	str := "p string @pattern(\"/*\")\nz: coalesce(args.c, 3)\n"
	wantStr := "p string @pattern(\"/*\")\nz: args.c ?? 3\n"
	got, err = RewriteNullCoalesce([]byte(str))
	if err != nil {
		t.Fatalf("RewriteNullCoalesce: %v", err)
	}
	if string(got) != wantStr {
		t.Errorf("string glob:\n got %q\nwant %q", got, wantStr)
	}
}

// Step calls are routinely written multi-line. memql#2772 made their
// named arguments accept `??`, so every line of the call migrates -- and
// so does the write-block field after the call's parens close.
func TestRewriteNullCoalesce_MultiLineStepCallArgs(t *testing.T) {
	src := "" +
		"  step createRecord {\n" +
		"    mutation createDatabase (\n" +
		`      host:    coalesce(database.host, "localhost"),` + "\n" +
		`      sslMode: coalesce(database.sslMode, "disable")` + "\n" +
		"    )\n" +
		"  }\n" +
		"  insert {\n" +
		`    status: coalesce(args.status, "healthy")` + "\n" +
		"  }\n"
	got, err := RewriteNullCoalesce([]byte(src))
	if err != nil {
		t.Fatalf("RewriteNullCoalesce: %v", err)
	}
	for _, want := range []string{
		`host:    database.host ?? "localhost"`,
		`sslMode: database.sslMode ?? "disable"`,
		`status: args.status ?? "healthy"`,
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// Positional arguments of expression builtins parse fine and must keep
// migrating, even when the whole expression sits inside a step call's
// named argument -- the coalesce there is positional, not the named
// value itself.
func TestRewriteNullCoalesce_PositionalArgsInsideStepCall(t *testing.T) {
	in := `  c := query expired( createdBefore: addDuration(now, concat("P-", coalesce(w.first().payload.value, "30"), "D")) )`
	want := `  c := query expired( createdBefore: addDuration(now, concat("P-", w.first().payload.value ?? "30", "D")) )`
	got, err := RewriteNullCoalesce([]byte(in))
	if err != nil {
		t.Fatalf("RewriteNullCoalesce: %v", err)
	}
	if string(got) != want {
		t.Errorf("positional arg must still fold:\n got %q\nwant %q", got, want)
	}
}

// Idempotence: the migration channel runs per release, so a second pass
// must be a no-op (and must not chain an already-folded `??` further).
func TestRewriteNullCoalesce_Idempotent(t *testing.T) {
	src := []byte("    a: coalesce(args.x, 1)\n    b: coalesce(args.y, coalesce(args.z, 2))\n")
	once, err := RewriteNullCoalesce(src)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	twice, err := RewriteNullCoalesce(once)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if string(once) != string(twice) {
		t.Errorf("not idempotent:\nonce  %q\ntwice %q", once, twice)
	}
	if want := "    a: args.x ?? 1\n    b: args.y ?? args.z ?? 2\n"; string(once) != want {
		t.Errorf("\n got %q\nwant %q", once, want)
	}
}

// An untouched file must come back byte-identical (same []byte contract
// the sibling rewrites honour), so `-check` reports no diff.
func TestRewriteNullCoalesce_NoChangeReturnsInput(t *testing.T) {
	src := []byte("concept widget {\n  label string\n}\n")
	got, err := RewriteNullCoalesce(src)
	if err != nil {
		t.Fatalf("RewriteNullCoalesce: %v", err)
	}
	if string(got) != string(src) {
		t.Errorf("unchanged input must round-trip: got %q", got)
	}
}

// The semantic contract: every shape the codemod rewrites must parse to
// the AST of an equivalent call spelling. This is what makes the corpus
// migration a no-op at the engine layer.
//
// equivalent is the call spelling the rewrite is expected to match, and
// is the source itself except where a nested call flattens (see
// TestNullCoalesceMigrate_NestedFlattenIsEquivalent for why the flat
// form is the same computation).
func TestNullCoalesceMigrate_ASTEquivalence(t *testing.T) {
	cases := []struct{ src, equivalent string }{
		{`coalesce(args.status, "")`, ""},
		{`coalesce(args.a, args.b, "")`, ""},
		{`coalesce(args.stage, "x")=="active"`, ""},
		{`coalesce(payload.n, payload.m)`, ""},
		{
			// n-ary fold: the chain is ONE flat CoalesceExpr, which is
			// the canonical spelling of the same selection.
			src:        `coalesce(args.a, coalesce(args.b, args.c))`,
			equivalent: `coalesce(args.a, args.b, args.c)`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			rewritten, err := RewriteNullCoalesce([]byte(tc.src))
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if string(rewritten) == tc.src {
				t.Fatalf("expected a rewrite for %q", tc.src)
			}
			equivalent := tc.equivalent
			if equivalent == "" {
				equivalent = tc.src
			}
			// Expression AST nodes carry no position fields, so
			// DeepEqual is an exact structural comparison.
			want := parseFilterExpr(t, equivalent)
			got := parseFilterExpr(t, string(rewritten))
			if !reflect.DeepEqual(want, got) {
				t.Errorf("AST drift: %q rewrote to %q, which must match %q:\n want %s\n  got %s",
					tc.src, rewritten, equivalent, jsonDump(t, want), jsonDump(t, got))
			}
		})
	}
}

// Flattening a nested call into one chain is semantically neutral under
// the #1614 selection rule: the FINAL argument is the ultimate fallback
// and is returned even when it resolves to "", while every non-final
// argument is skipped when it is nil or blank.
//
// Nested  coalesce(a, coalesce(b, c)):
//
//	a non-blank -> a;  else the inner is final, so -> (b non-blank ? b : c)
//
// Flat    coalesce(a, b, c):
//
//	a non-blank -> a;  b non-blank -> b;  else c
//
// Same function of (a, b, c). This test pins that the two spellings
// agree on the discriminating inputs rather than merely asserting the
// shapes look alike.
func TestNullCoalesceMigrate_NestedFlattenIsEquivalent(t *testing.T) {
	nested := parseFilterExpr(t, `coalesce(args.a, coalesce(args.b, args.c))`)
	flat := parseFilterExpr(t, `coalesce(args.a, args.b, args.c)`)

	nestedOuter, ok := nested.(*CoalesceExpr)
	if !ok {
		t.Fatalf("nested: want CoalesceExpr, got %T", nested)
	}
	if len(nestedOuter.Args) != 2 {
		t.Fatalf("nested outer: want 2 args, got %d", len(nestedOuter.Args))
	}
	inner, ok := nestedOuter.Args[1].(*CoalesceExpr)
	if !ok {
		t.Fatalf("nested inner: want CoalesceExpr, got %T", nestedOuter.Args[1])
	}

	flatCo, ok := flat.(*CoalesceExpr)
	if !ok {
		t.Fatalf("flat: want CoalesceExpr, got %T", flat)
	}
	if len(flatCo.Args) != 3 {
		t.Fatalf("flat: want 3 args, got %d", len(flatCo.Args))
	}

	// The operands appear in the same order across both spellings; only
	// the grouping differs, and the #1614 rule makes the grouping
	// irrelevant because the inner call sits in the final slot.
	wantOrder := []ExpressionNode{nestedOuter.Args[0], inner.Args[0], inner.Args[1]}
	if !reflect.DeepEqual(wantOrder, flatCo.Args) {
		t.Errorf("operand order differs between the nested and flat spellings:\n nested %s\n   flat %s",
			jsonDump(t, nested), jsonDump(t, flat))
	}
}
