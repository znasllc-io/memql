package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
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

// Review finding: a malformed operator must not be silently absorbed
// into a coalesce() with an empty argument. The runtime evaluator has no
// parser behind it, so this has to fail at LOAD -- the strict-boot gate
// then refuses the construct instead of shipping a silent misread.
func TestParsePayloadRawToTemplate_RejectsMalformedNullCoalesce(t *testing.T) {
	for _, src := range []string{
		`{ active: ctx.a ?? }`,
		`{ active: ctx.a ?? ?? ctx.b }`,
		`{ nested: { inner: ctx.a ?? } }`,
	} {
		t.Run(src, func(t *testing.T) {
			_, err := parsePayloadRawToTemplate(src)
			require.Error(t, err, "a dangling/doubled ?? must be refused at load")
			require.Contains(t, err.Error(), "malformed `??`")
		})
	}
}

// Review finding: the call-argument lowering must find the paren that
// MATCHES the opener. Slicing to the LAST ')' in the string rejoined a
// trailing term inside the first call's argument list.
func TestLowerNullCoalesceExpr_DoesNotSwallowTrailingTerms(t *testing.T) {
	// The defect: slicing to the LAST ')' produced
	// `f(coalesce(a, 1) + g(2))`, moving `+ g(2)` inside f's arg list.
	// Each group is now lowered in place and siblings stay siblings.
	require.Equal(t, `f(coalesce(a, 1)) + g(2)`, lowerNullCoalesceExpr(`f(a ?? 1) + g(2)`))
	require.Equal(t, `daysBetween(a, coalesce(b, now)) + 1`,
		lowerNullCoalesceExpr(`daysBetween(a, b ?? now) + 1`))
	// A single well-formed call still lowers in place.
	require.Equal(t, `f(coalesce(a, 1))`, lowerNullCoalesceExpr(`f(a ?? 1)`))
	// A paren inside a string is not a group delimiter -- neither the
	// closer nor the opener.
	require.Equal(t, `f(coalesce(a, ")"))`, lowerNullCoalesceExpr(`f(a ?? ")")`))
	require.Equal(t, `x + "(" + f(coalesce(a, 1))`,
		lowerNullCoalesceExpr(`x + "(" + f(a ?? 1)`))
	// Nested groups lower at every level.
	require.Equal(t, `f(g(coalesce(a, 1)))`, lowerNullCoalesceExpr(`f(g(a ?? 1))`))
	// Unbalanced input is left alone rather than corrupted.
	require.Equal(t, `f(a ?? 1`, lowerNullCoalesceExpr(`f(a ?? 1`))
}

// The malformed-operator sentinel must never fire on a quoted literal:
// prose containing `??` is legitimate authoring, the guard runs at LOAD,
// and a false positive would refuse the node's boot entirely.
func TestParsePayloadRawToTemplate_AllowsNullCoalesceInsideStrings(t *testing.T) {
	tpl, err := parsePayloadRawToTemplate(
		`{ note: "pass ?? to get a default", hint: "a ?? b ?? c", slug: ctx.slug ?? "x" }`)
	require.NoError(t, err, "a `??` inside a string literal is prose, not an operator")
	require.Equal(t, `pass ?? to get a default`, tpl["note"])
	require.Equal(t, `a ?? b ?? c`, tpl["hint"])
	require.Equal(t, `coalesce(ctx.slug, "x")`, tpl["slug"])
}

// The load guard walks arrays as well as nested objects, so a malformed
// operator cannot hide in an array element.
func TestParsePayloadRawToTemplate_RejectsMalformedInsideArray(t *testing.T) {
	for _, src := range []string{
		`{ tags: [ctx.a ??] }`,
		`{ tags: [ctx.a ?? ?? ctx.b] }`,
		`{ tags: [{ inner: ctx.a ?? }] }`,
	} {
		t.Run(src, func(t *testing.T) {
			_, err := parsePayloadRawToTemplate(src)
			require.Error(t, err, "a malformed ?? in an array element must be refused at load")
			require.Contains(t, err.Error(), "malformed `??`")
		})
	}

	// ...and a well-formed one in an array still lowers.
	tpl, err := parsePayloadRawToTemplate(`{ tags: [ctx.a ?? "x"] }`)
	require.NoError(t, err)
	require.Equal(t, []any{`coalesce(ctx.a, "x")`}, tpl["tags"])
}

// Review finding: `&&` / `||` bind LOOSER than `??` in the parser
// cascade, so the lowering has to split them first or the two spellings
// drift.
func TestLowerNullCoalesceExpr_BooleanConnectivePrecedence(t *testing.T) {
	require.Equal(t, `a && coalesce(b, c)`, lowerNullCoalesceExpr(`a && b ?? c`))
	require.Equal(t, `coalesce(a, b) || c`, lowerNullCoalesceExpr(`a ?? b || c`))
	// `;` is the retired separator spelling of AND, still accepted by
	// parseLogicalAnd -- so it has to split like `&&` or the same drift
	// reappears through the back door.
	require.Equal(t, `a ; coalesce(b, c)`, lowerNullCoalesceExpr(`a ; b ?? c`))
	// Mixed connectives keep each side's coalesce independent.
	require.Equal(t, `coalesce(a, b) && coalesce(c, d)`, lowerNullCoalesceExpr(`a ?? b && c ?? d`))
}

// A paren opened INSIDE an object/array literal must not leave the group
// walker stuck "inside a literal": the missing ')' decrement silently
// skipped every later group, so a real `??` reached the template
// unlowered AND unreported.
func TestLowerNullCoalesceExpr_ParenInsideLiteralGroup(t *testing.T) {
	require.Equal(t, `f([g(1)]+h(coalesce(a, 1)))`, lowerNullCoalesceExpr(`f([g(1)]+h(a ?? 1))`))
	require.Equal(t, `f({x: g(1)}, h(coalesce(a, 1)))`, lowerNullCoalesceExpr(`f({x: g(1)}, h(a ?? 1))`))
}

// The sentinel must never cross the persistence boundary. A `??` inside
// a literal ARM is invisible to the load guard (the lowering does not
// descend into `key: value` members), so it is re-classified at runtime;
// without unwrapping, the unexported struct marshals to `{}` -- silently
// indistinguishable from a deliberate empty object.
func TestUnwrapMalformedNullCoalesce_KeepsRawTextOutOfPayloads(t *testing.T) {
	obj := parseObjectLiteral(`{ a: ctx.b ?? }`)
	require.NotNil(t, obj)
	require.IsType(t, malformedNullCoalesce{}, obj["a"], "classification marks it")

	unwrapped, ok := unwrapMalformedNullCoalesce(obj).(map[string]any)
	require.True(t, ok)
	require.Equal(t, `ctx.b ??`, unwrapped["a"], "the raw text survives, not a sentinel struct")

	arr, ok := unwrapMalformedNullCoalesce([]any{malformedNullCoalesce{raw: "x ??"}}).([]any)
	require.True(t, ok)
	require.Equal(t, "x ??", arr[0])
}

// ...and the same through the RUNTIME evaluator, which is where the
// sentinel would actually escape. Asserting on the helper alone leaves
// the two call sites in evalString unprotected -- reverting them keeps a
// helper-only test green.
func TestEvalString_NeverYieldsMalformedSentinel(t *testing.T) {
	eval := &mutationTemplateEvaluator{args: map[string]any{}}
	ctx := context.Background()
	for _, src := range []string{
		`{ a: args.b ?? }`,
		`[ args.b ?? ]`,
		`coalesce(args.missing, { a: args.b ?? })`,
		`coalesce(args.missing, [ args.b ?? ])`,
		`cond(args.missing, { a: args.b ?? }, 1)`,
		`{ outer: { inner: args.b ?? } }`,
	} {
		t.Run(src, func(t *testing.T) {
			got, err := eval.evalString(ctx, src)
			require.NoError(t, err)
			require.False(t, containsMalformedSentinel(got),
				"the unexported sentinel must never reach a stored payload; got %#v", got)
		})
	}
}

func containsMalformedSentinel(v any) bool {
	switch t := v.(type) {
	case malformedNullCoalesce:
		return true
	case map[string]any:
		for _, nested := range t {
			if containsMalformedSentinel(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range t {
			if containsMalformedSentinel(nested) {
				return true
			}
		}
	}
	return false
}

// The FINAL arm resolving to missing must come back as nil, not the
// missingValue{} sentinel: isTruthy(missingValue{}) is true, so leaking
// it would make an all-missing coalesce read as truthy.
func TestIdTemplateCoalesce_MissingFinalArmIsNil(t *testing.T) {
	eval := &mutationTemplateEvaluator{args: map[string]any{}}
	got, err := eval.evalParserExpression(context.Background(),
		coalesceOf(argRef("absentA"), argRef("absentB")))
	require.NoError(t, err)
	require.Nil(t, got, "an all-missing coalesce is nil, never the missingValue sentinel")
	require.False(t, isMissing(got))
}

// Review finding, the severe one: the id / createdAt / parent / aliasOf
// templates evaluate a CoalesceExpr through a DIFFERENT implementation
// than payload fields, and it skipped only nil/missing -- so a blank
// non-final arm won.
//
// "" is what clients send for an absent optional string, which is the
// whole reason #1614 exists. With the old behaviour
// `id: args.roleId ?? args.slug` yielded "" for roleId=="", the engine
// minted a RANDOM id instead of the deterministic slug, and idempotent
// upserts silently became duplicate rows.
func TestIdTemplateCoalesce_SkipsBlankNonFinalArm(t *testing.T) {
	eval := &mutationTemplateEvaluator{args: map[string]any{
		"roleId": "",
		"slug":   "editor",
		"blank":  "",
	}}
	ctx := context.Background()

	got, err := eval.evalParserExpression(ctx, coalesceOf(argRef("roleId"), argRef("slug")))
	require.NoError(t, err)
	require.Equal(t, "editor", got, "a blank non-final arm must be skipped (#1614)")

	// The FINAL arm stays the ultimate fallback: it is returned even
	// when it resolves to "", so an explicit empty default still wins
	// over inventing a value.
	got, err = eval.evalParserExpression(ctx, coalesceOf(argRef("roleId"), argRef("blank")))
	require.NoError(t, err)
	require.Equal(t, "", got, "the final arm is returned even when blank (#1614)")

	// A non-blank leading arm still short-circuits.
	got, err = eval.evalParserExpression(ctx, coalesceOf(argRef("slug"), argRef("blank")))
	require.NoError(t, err)
	require.Equal(t, "editor", got)
}

func argRef(name string) languageParser.ExpressionNode {
	return &languageParser.ArgRefExpr{Path: name}
}

func coalesceOf(args ...languageParser.ExpressionNode) languageParser.ExpressionNode {
	return &languageParser.CoalesceExpr{Args: args}
}
