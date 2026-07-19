package compiler

import (
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

	// cmp-then-??: the identifier-led baseline IS expressible -- parseValue
	// accepts a coalesce() call as the comparison value (review round 2
	// corrected the earlier claim here) -- so the shorthand must reproduce
	// that exact node and emission on the value side too.
	cmpRows := [][2]string{
		{`payload.a==payload.b??payload.c`, `payload.a==coalesce(payload.b, payload.c)`},
		{`payload.a=="b"??"c"`, `payload.a==coalesce("b", "c")`},
		{`payload.n>payload.m??0`, `payload.n>coalesce(payload.m, 0)`},
		{`payload.a!=payload.b??"z"`, `payload.a!=coalesce(payload.b, "z")`},
	}
	for _, r := range cmpRows {
		short, long := serialize(r[0]), serialize(r[1])
		if short != long {
			t.Errorf("cmp-then-?? diverged:\n  ?? spelling %q -> %s\n  baseline    %q -> %s", r[0], short, r[1], long)
		}
	}
}
