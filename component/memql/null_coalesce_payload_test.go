package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// memql#2772: the `??` operator (#2611) must reach write-block payloads.
//
// #2611's central claim is that `??` lowers byte-identically to the
// coalesce() spelling, so every downstream evaluator is untouched by
// construction. The write-block payload path broke that claim in two
// places, because it never parses the value -- it carries raw text:
//
//  1. parseLiteralOrExpr tracked nesting for PARENS ONLY, so a `}` or
//     `]` at paren-depth zero ended the value: `args.labels ?? {}`
//     truncated to `args.labels ?? {`.
//  2. the runtime value evaluator dispatches on a string PREFIX -- a
//     value starting with `args.` had to be a bare dotted path, so
//     `args.x ?? "d"` died with `invalid character '?'`.
//
// The fix restores the claim rather than teaching two string scanners
// about operator precedence: the template stores the lowered
// coalesce() spelling, so the evaluator never sees the token.

// The brace/bracket arm is the load-time half: the value must survive
// scanning intact instead of truncating at the closing brace.
func TestParseObjectLiteral_NullCoalesceBraceArm(t *testing.T) {
	obj := parseObjectLiteral(`{
		labels: ctx.labels ?? {},
		tags: ctx.tags ?? [],
		nested: ctx.src ?? { inputMethod: "si" },
		plain: ctx.status ?? "healthy"
	}`)
	require.NotNil(t, obj, "a ??-with-brace-arm payload must parse, not truncate at the `}`")
	require.Equal(t, `coalesce(ctx.labels, {})`, obj["labels"])
	require.Equal(t, `coalesce(ctx.tags, [])`, obj["tags"])
	require.Equal(t, `coalesce(ctx.src, { inputMethod: "si" })`, obj["nested"])
	require.Equal(t, `coalesce(ctx.status, "healthy")`, obj["plain"])
}

// The lowering itself, at the point the template value is classified.
func TestLowerNullCoalesceExpr(t *testing.T) {
	cases := []struct{ in, want string }{
		{`ctx.status ?? "healthy"`, `coalesce(ctx.status, "healthy")`},
		{`args.a ?? args.b ?? ""`, `coalesce(args.a, args.b, "")`},
		{`ctx.createdAt ?? now`, `coalesce(ctx.createdAt, now)`},
		// Looser than additive: the SUM is the coalesced arm.
		{`ctx.n + 1 ?? 0`, `coalesce(ctx.n + 1, 0)`},
		// Tighter than comparison: the coalesce binds first, so the
		// comparison stays outside it (#2611's Swift-tight choice).
		{`ctx.stage ?? "" == "active"`, `coalesce(ctx.stage, "") == "active"`},
		// A `??` nested inside a call argument lowers in place.
		{`concat("P-", ctx.w ?? "30", "D")`, `concat("P-", coalesce(ctx.w, "30"), "D")`},
		// Already-lowered and ??-free text is returned untouched.
		{`coalesce(ctx.a, "")`, `coalesce(ctx.a, "")`},
		{`ctx.plain`, `ctx.plain`},
		// `??` inside a string literal is data, not an operator.
		{`"a ?? b"`, `"a ?? b"`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			require.Equal(t, tc.want, lowerNullCoalesceExpr(tc.in))
		})
	}
}

// Idempotence: lowering an already-lowered expression is a no-op, so a
// template rebuilt from stored text cannot double-wrap.
func TestLowerNullCoalesceExpr_Idempotent(t *testing.T) {
	once := lowerNullCoalesceExpr(`ctx.a ?? ctx.b ?? ""`)
	require.Equal(t, once, lowerNullCoalesceExpr(once))
}

// End to end through the template builder: the stored payload must be
// the coalesce() spelling, which is what makes the runtime evaluator
// work unchanged.
func TestParsePayloadRawToTemplate_LowersNullCoalesce(t *testing.T) {
	tpl, err := parsePayloadRawToTemplate(`{ sinceAt: ctx.sinceAt ?? now, labels: ctx.labels ?? {} }`)
	require.NoError(t, err)
	require.Equal(t, `coalesce(ctx.sinceAt, now)`, tpl["sinceAt"])
	require.Equal(t, `coalesce(ctx.labels, {})`, tpl["labels"])
}
