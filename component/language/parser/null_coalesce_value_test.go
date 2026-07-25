package parser

import (
	"reflect"
	"testing"
)

// memql#2772: `??` (#2611) in the remaining VALUE slots.
//
// #2611 wired the operator into the expression cascade, cond-args and the
// object-literal value slot, but three value positions call parseValue()
// and then simply do not look for a following `??`, so the token falls
// through to the enclosing `expect(')')` as `expected ')', got "??"`:
//
//   - named args of a construct call -- `query q( planId: x ?? "" )`
//   - the `id=` / `createdAt=` / `parent=` / `aliasOf=` slots of the
//     normalised `insert(...)` write form, which is what the authored
//     `insert { id: ... }` field becomes
//   - positional args of the same calls
//
// These tests drive the AUTHORED struct form through NormaliseAll first,
// exactly as the loader does, so they pin the author-facing contract
// rather than the internal procedural shape.

func parseAuthored(t *testing.T, src string) error {
	t.Helper()
	norm, err := NormaliseAll(src)
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	lx := NewLexer(norm)
	toks, err := lx.Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	p := NewParser(toks)
	p.SetDocComments(lx.DocComments())
	p.SetSource(norm)
	_, perr := p.Parse()
	return perr
}

func TestNullCoalesce_StepCallNamedArg(t *testing.T) {
	cases := []struct{ name, src string }{
		{
			name: "query step named arg",
			src: "logic probe {\n  args {\n    event object!\n  }\n  body {\n" +
				"    return query workspaceForPlan( planId: args.event.node.id ?? \"\" )\n  }\n}\n",
		},
		{
			name: "builtin step named arg, second of two",
			src: "logic probe {\n  args {\n    current string!\n    bump string\n  }\n  body {\n" +
				"    return builtin suggestNextVersion( current: args.current, bump: args.bump ?? \"patch\" )\n  }\n}\n",
		},
		{
			name: "named arg with a chain",
			src: "logic probe {\n  args {\n    a string\n    b string\n  }\n  body {\n" +
				"    return query q( x: args.a ?? args.b ?? \"\" )\n  }\n}\n",
		},
		{
			name: "brace-literal fallback in a named arg",
			src: "logic probe {\n  args {\n    labels object\n  }\n  body {\n" +
				"    return query q( labels: args.labels ?? {} )\n  }\n}\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := parseAuthored(t, tc.src); err != nil {
				t.Errorf("must parse post-#2772, got: %v", err)
			}
		})
	}
}

// The authored `insert { id: ... }` field normalises to the `id=` slot of
// the internal insert(...) call, which is why it rejected the token while
// its sibling payload fields did not.
func TestNullCoalesce_WriteBlockIdSlot(t *testing.T) {
	cases := []struct{ name, src string }{
		{
			name: "id falls back to another arg",
			src: "mutate role probe {\n  args {\n    roleId string\n    slug string!\n  }\n" +
				"  insert {\n    id: args.roleId ?? args.slug\n  }\n}\n",
		},
		{
			name: "id falls back to a call",
			src: "mutate node probe {\n  args {\n    id string\n    nodeType string!\n  }\n" +
				"  insert {\n    id: args.id ?? concat(\"node-\", args.nodeType)\n  }\n}\n",
		},
		{
			name: "sibling payload fields still parse alongside",
			src: "mutate role probe {\n  args {\n    roleId string\n    slug string!\n    active bool\n  }\n" +
				"  insert {\n    id: args.roleId ?? args.slug\n    active: args.active ?? true\n  }\n}\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := parseAuthored(t, tc.src); err != nil {
				t.Errorf("must parse post-#2772, got: %v", err)
			}
		})
	}
}

// The fold must be the SAME node the coalesce() spelling produces -- one
// flat n-ary CoalesceExpr -- so every downstream consumer is untouched
// by construction (#2611's central claim).
func TestNullCoalesce_ValueSlotFoldShape(t *testing.T) {
	val := parseValueSlot(t, `args.a ?? args.b ?? ""`)
	co, ok := val.(*CoalesceExpr)
	if !ok {
		t.Fatalf("want *CoalesceExpr, got %T", val)
	}
	if len(co.Args) != 3 {
		t.Fatalf("chain must fold flat n-ary; got %d args", len(co.Args))
	}
	if _, nested := co.Args[0].(*CoalesceExpr); nested {
		t.Error("chain folded left-nested instead of flat")
	}

	// A value with no `??` must come back exactly as parseValue produced
	// it -- the helper must not wrap every value in a CoalesceExpr.
	plain := parseValueSlot(t, `args.a`)
	if _, wrapped := plain.(*CoalesceExpr); wrapped {
		t.Error("a ??-free value must not be wrapped in a CoalesceExpr")
	}
}

// The arms must be the SAME node kind the coalesce() call form
// produces. parseValue returns a bare Go string for an identifier and
// valueToExprNode wraps it in a LiteralExpr, so folding parseValue
// results turned `body ?? ""` into coalesce("body", "") -- the
// automation then wrote the identifier's own NAME into the row
// (`summary: "body"` instead of the note body), the memql#580
// render-the-identifier-as-its-own-name class. A source-text assertion
// cannot see this; only the node kind can.
func TestNullCoalesce_BareIdentifierArmStaysAReference(t *testing.T) {
	shorthand := parseValueSlot(t, `body ?? ""`)
	co, ok := shorthand.(*CoalesceExpr)
	if !ok {
		t.Fatalf("want *CoalesceExpr, got %T", shorthand)
	}
	if lit, isLit := co.Args[0].(*LiteralExpr); isLit {
		t.Fatalf("bare identifier arm became a string literal %v -- it renders as its own name", lit.Value)
	}

	// Pin it positively against the call spelling: the same operand must
	// produce the same node kind in both.
	baseline := parseFilterExpr(t, `coalesce(body, "")`)
	baseCo, ok := baseline.(*CoalesceExpr)
	if !ok {
		t.Fatalf("baseline: want *CoalesceExpr, got %T", baseline)
	}
	if got, want := reflect.TypeOf(co.Args[0]), reflect.TypeOf(baseCo.Args[0]); got != want {
		t.Errorf("arm node kind differs from the coalesce() spelling: got %v, want %v", got, want)
	}

	// Dotted references too -- `node.id ?? ""` is the cluster-registration
	// shape that wrote the literal "node.id" into every registered node.
	dotted := parseValueSlot(t, `node.id ?? ""`)
	dco, ok := dotted.(*CoalesceExpr)
	if !ok {
		t.Fatalf("dotted: want *CoalesceExpr, got %T", dotted)
	}
	if lit, isLit := dco.Args[0].(*LiteralExpr); isLit {
		t.Fatalf("dotted reference arm became a string literal %v", lit.Value)
	}
}

func parseValueSlot(t *testing.T, src string) any {
	t.Helper()
	lx := NewLexer(src)
	toks, err := lx.Tokenize()
	if err != nil {
		t.Fatalf("tokenize %q: %v", src, err)
	}
	p := NewParser(toks)
	val, err := p.parseValueMaybeCoalesce()
	if err != nil {
		t.Fatalf("parseValueMaybeCoalesce %q: %v", src, err)
	}
	return val
}
