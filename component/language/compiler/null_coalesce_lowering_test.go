package compiler

import (
	"strings"
	"testing"

	parser "github.com/znasllc-io/memql/component/language/parser"
)

// #2611 (review finding 3): the operator's central claim is that `??` lowers
// byte-identically to the coalesce() spelling, so every downstream evaluator
// is untouched by construction. This pins the claim at the serializer, using
// the identifier-fallback row the review proved DIVERGED pre-fix:
// `payload.stage ?? payload.def == "active"` must serialize exactly as
// `coalesce(payload.stage, payload.def) == "active"`.
func TestNullCoalesce_LowersIdenticallyToCoalesceSpelling(t *testing.T) {
	c := &Compiler{}
	serialize := func(src string) string {
		t.Helper()
		expr, err := parser.ParseExpression(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		return c.expressionToString(expr)
	}

	rows := [][2]string{
		{`payload.stage??"x"`, `coalesce(payload.stage, "x")`},
		{`payload.a??payload.b??payload.c`, `coalesce(payload.a, payload.b, payload.c)`},
		{`payload.stage??payload.def=="active"`, `coalesce(payload.stage, payload.def)=="active"`},
	}
	for _, r := range rows {
		short, long := serialize(r[0]), serialize(r[1])
		if short != long {
			t.Errorf("lowering diverged:\n  ?? spelling %q -> %s\n  baseline    %q -> %s", r[0], short, r[1], long)
		}
	}

	// cmp-then-?? has no identifier-led baseline spelling (the fold's RHS is
	// parseValue: values only, no parenthesized expressions), so pin the
	// serialized IR shape directly: the coalesce must sit on the VALUE side
	// of the comparison, never swallow it.
	got := serialize(`payload.a=="b"??"c"`)
	if !strings.Contains(got, "coalesce") {
		t.Errorf("cmp-then-??: coalesce missing from the lowering: %s", got)
	}
	if strings.HasPrefix(got, "coalesce") {
		t.Errorf("cmp-then-??: lowered JS-loose (comparison swallowed into the coalesce): %s", got)
	}
}
