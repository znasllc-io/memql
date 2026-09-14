package memql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// mutation_values_v1_test.go pins the edition-2026 mutation values (epic
// memql#5363, memql#5367): a mutation whose insert/update block and slots
// are parsed v1 nodes, rendered by EvalExpr (mutation_values_v1.go).
//
// The parser option that makes the loader build these templates for the
// tree lands separately, so every template here is built the way that
// option will build it: the block and the slots are parsed with
// languageParser.ParseV1Expression and handed to newMutationTemplateV1.
//
// Where a test is the twin of a pinned test of the string half, it names
// it, and where the v1 table deliberately answers differently it says so at
// the assertion. The legacy tests stay as they are: the string half renders
// the tree until the flip.

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// v1Expr parses one edition-2026 expression.
func v1Expr(t *testing.T, src string) ast.ExpressionNode {
	t.Helper()
	n, err := languageParser.ParseV1Expression(src)
	require.NoErrorf(t, err, "parse %s", src)
	return n
}

// v1MutationSrc is a mutation's values as v1 source text: the block (a map
// literal, "" for none) and the four slots ("" for an unwritten slot).
type v1MutationSrc struct {
	kind                           ast.MutationKind
	block                          string
	id, createdAt, parent, aliasOf string
}

// buildV1Mutation parses src and builds its template. A parse failure fails
// the test; a build refusal is returned.
func buildV1Mutation(t *testing.T, concept string, src v1MutationSrc) (*FunctionMutationTemplate, error) {
	t.Helper()
	slot := func(s string) ast.ExpressionNode {
		if s == "" {
			return nil
		}
		return v1Expr(t, s)
	}
	var block ast.ExpressionNode
	if src.block != "" {
		block = v1Expr(t, src.block)
	}
	return newMutationTemplateV1(src.kind, concept, block, mutationSlotsV1{
		ID: slot(src.id), CreatedAt: slot(src.createdAt), Parent: slot(src.parent), AliasOf: slot(src.aliasOf),
	})
}

func mustV1Mutation(t *testing.T, concept string, src v1MutationSrc) *FunctionMutationTemplate {
	t.Helper()
	tmpl, err := buildV1Mutation(t, concept, src)
	require.NoError(t, err)
	require.True(t, tmpl.ValuesV1)
	return tmpl
}

// renderV1Payload renders tmpl and decodes its payload.
func renderV1Payload(t *testing.T, e *MemQLEngine, ctx context.Context, tmpl *FunctionMutationTemplate, args map[string]any) (MutationNode, map[string]any) {
	t.Helper()
	node, err := e.renderMutationTemplate(ctx, tmpl, args)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(node.PayloadRaw), &payload))
	return node, payload
}

// v1ValueOf renders `{v: <src>}` with args and returns the rendered value of
// v and whether the key is present.
func v1ValueOf(t *testing.T, ctx context.Context, src string, args map[string]any) (any, bool, error) {
	t.Helper()
	tmpl, err := buildV1Mutation(t, "v1:cognition:space", v1MutationSrc{block: "{v: " + src + "}"})
	if err != nil {
		return nil, false, err
	}
	node, err := (&MemQLEngine{}).renderMutationTemplate(ctx, tmpl, args)
	if err != nil {
		return nil, false, err
	}
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(node.PayloadRaw), &payload))
	v, present := payload["v"]
	return v, present, nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// the rendering matrix: every value shape the corpus writes
// ---------------------------------------------------------------------------

// TestMutationValuesV1RenderingMatrix renders one value per row, as a
// mutation value, and compares what lands in the payload. absent means the
// key is omitted.
func TestMutationValuesV1RenderingMatrix(t *testing.T) {
	ctx := auth.ContextWithUserActor(context.Background(), "user-matrix")
	const absent = "<absent>"
	args := map[string]any{
		"s":      "active",
		"n":      int64(9),
		"f":      2.5,
		"yes":    true,
		"no":     false,
		"zero":   int64(0),
		"blank":  "",
		"space":  " ",
		"null":   nil,
		"list":   []any{"x", "y"},
		"empty":  []any{},
		"obj":    map[string]any{"k": "v"},
		"objEmp": map[string]any{},
		"a":      "A",
		"b":      "B",
		"canon":  "v1:cluster:deployment:abc123",
		"nested": map[string]any{"type": "chat"},
	}
	for _, tc := range []struct {
		src  string
		want any
	}{
		// args.X: present, missing (the key is omitted), explicit nil (kept).
		{`args.s`, "active"},
		{`args.n`, float64(9)},
		{`args.f`, 2.5},
		{`args.missing`, absent},
		{`args.null`, nil},
		{`args.nested.type`, "chat"},
		{`args.nested.absentLeaf`, absent},
		{`args.list`, []any{"x", "y"}},

		// Literals keep their type. A quoted "123" is a STRING: the string
		// half decoded it to 123, and read a quoted "args.s" as the arg,
		// a quoted "true" as a bool and a quoted "null" as null.
		{`"text"`, "text"},
		{`"123"`, "123"},
		{`"args.s"`, "args.s"},
		{`"true"`, "true"},
		{`"null"`, "null"},
		{`"now"`, "now"},
		{`123`, float64(123)},
		{`1.5`, 1.5},
		{`-5`, float64(-5)},
		{`true`, true},
		{`false`, false},
		{`nil`, nil},

		// The actor.
		{`actor.userId`, "user-matrix"},

		// `??` is blank-coalescing (rule 30), through the one selection rule.
		{`args.missing ?? "d"`, "d"},
		{`args.blank ?? "d"`, "d"},
		{`args.space ?? "d"`, "d"},
		{`args.null ?? "d"`, "d"},
		{`args.s ?? "d"`, "active"},
		{`args.missing ?? 0`, float64(0)},
		{`args.zero ?? 7`, float64(0)},
		{`args.missing ?? []`, []any{}},
		{`args.empty ?? ["d"]`, []any{}},
		{`args.missing ?? {}`, map[string]any{}},
		{`args.objEmp ?? {d: 1}`, map[string]any{}},
		{`args.missing ?? false`, false},
		{`args.no ?? true`, false},
		{`args.missing ?? args.b ?? ""`, "B"},
		{`args.missing ?? args.alsoMissing ?? ""`, ""},
		{`args.missing ?? args.alsoMissing`, nil},

		// Text composition: `+` is the old concat, hash is fixed-width.
		{`args.a + "-" + args.b`, "A-B"},
		{`args.missing + "-" + args.b`, "-B"},
		{`hash(args.a + "-" + args.b)`, sha256Hex("A-B")},
		{`hash(args.missing)`, sha256Hex("")},
		{`"prefix-" + hash(args.a)`, "prefix-" + sha256Hex("A")},
		{`shortId(args.canon)`, "abc123"},
		{`shortId(args.missing)`, ""},
		{`lower("MiXed")`, "mixed"},

		// Conditional values are `? :`, over a boolean.
		{`args.s == "active" ? "MATCH" : "NOMATCH"`, "MATCH"},
		{`args.n > 5 ? "BIG" : "SMALL"`, "BIG"},
		{`args.missing == "" ? "unset" : "set"`, "unset"},

		// Containers under the container rule.
		{`[args.a, args.missing, "c"]`, []any{"A", "c"}},
		{`[nil, args.b]`, []any{nil, "B"}},
		{`{x: args.a, y: args.missing, z: {w: args.b, q: args.missing}}`, map[string]any{"x": "A", "z": map[string]any{"w": "B"}}},
		{`args.list.count()`, float64(2)},
	} {
		t.Run(tc.src, func(t *testing.T) {
			got, present, err := v1ValueOf(t, ctx, tc.src, args)
			require.NoError(t, err)
			if tc.want == absent {
				require.Falsef(t, present, "%s: a missing argument omits its key; got %#v", tc.src, got)
				return
			}
			require.Truef(t, present, "%s: the key must be written", tc.src)
			require.Equalf(t, tc.want, got, "%s", tc.src)
		})
	}
}

// TestMutationValuesV1NowIsOneClock: every `now` in one call reads one
// instant, formatted RFC3339Nano UTC, as the string half's single e.now did.
func TestMutationValuesV1NowIsOneClock(t *testing.T) {
	tmpl := mustV1Mutation(t, "v1:notes:note", v1MutationSrc{
		block:     `{updatedAt: now, seenAt: args.seenAt ?? now, label: "now"}`,
		createdAt: `now`,
	})
	before := time.Now().UTC()
	node, payload := renderV1Payload(t, &MemQLEngine{}, context.Background(), tmpl, nil)
	updatedAt, ok := payload["updatedAt"].(string)
	require.True(t, ok)
	ts, err := time.Parse(time.RFC3339Nano, updatedAt)
	require.NoError(t, err)
	require.False(t, ts.Before(before.Add(-time.Second)))
	require.Equal(t, updatedAt, payload["seenAt"], "two reads of now in one call are one instant")
	require.Equal(t, "now", payload["label"], "a quoted \"now\" is the string")
	require.NotNil(t, node.CreatedAt, "createdAt: now lands on the node, not the payload")
	require.Equal(t, updatedAt, node.CreatedAt.UTC().Format(time.RFC3339Nano))
}

// TestMutationValuesV1CanonicalId renders canonicalId against a registry,
// through the engine's one canonicalisation (canonicalizeIdValue).
func TestMutationValuesV1CanonicalId(t *testing.T) {
	e := &MemQLEngine{concepts: newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:campaigns:campaign": {Name: "v1:campaigns:campaign"},
	})}
	tmpl := mustV1Mutation(t, "v1:campaigns:campaign", v1MutationSrc{
		block: `{bare: canonicalId(args.x, "v1:campaigns:campaign"), missing: canonicalId(args.none, "v1:campaigns:campaign")}`,
		id:    `hash(canonicalId(args.x, "v1:campaigns:campaign"))`,
	})
	for _, x := range []string{"c-1", "v1:campaigns:campaign:c-1"} {
		node, payload := renderV1Payload(t, e, context.Background(), tmpl, map[string]any{"x": x})
		require.Equal(t, "v1:campaigns:campaign:c-1", payload["bare"], "bare and canonical in, canonical out")
		require.Equal(t, "", payload["missing"], "an absent id canonicalises to empty, as every spelling did")
		require.Equal(t, sha256Hex("v1:campaigns:campaign:c-1"), node.ID, "both shapes derive one id")
	}
	_, err := e.renderMutationTemplate(context.Background(), tmpl, map[string]any{"x": "v1:identity:user:u-1"})
	require.Error(t, err, "an id under another concept is refused, never rewritten")
}

// TestMutationValuesV1VariablesReachTheEngineResolvers: var() and the secret
// readers reach the engine's resolvers, and the resolver's error reaches the
// caller typed.
func TestMutationValuesV1VariablesReachTheEngineResolvers(t *testing.T) {
	tmpl := mustV1Mutation(t, "v1:cognition:space", v1MutationSrc{block: `{v: var("REGION")}`})
	_, err := (&MemQLEngine{}).renderMutationTemplate(context.Background(), tmpl, nil)
	require.Error(t, err)
	require.Truef(t, errors.Is(err, ErrEngineNotInitialized),
		"an engine with no database cannot resolve a variable, and says so typed; got %v", err)
}

// TestMutationValuesV1Config binds the allow-listed configuration.
func TestMutationValuesV1Config(t *testing.T) {
	got, present, err := v1ValueOf(t, context.Background(), `config.demoMode`, nil)
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, "", got, "with no snapshot a non-sensitive key reads empty, as buildAmbientEnvelope builds it")
}

// ---------------------------------------------------------------------------
// the layout the builder produces
// ---------------------------------------------------------------------------

// TestNewMutationTemplateV1LaysOutTheBlockAsTheLoaderDoes: the hoist and the
// layout are the loader's, so the gates that read the layout see the shape
// they have always seen.
func TestNewMutationTemplateV1LaysOutTheBlockAsTheLoaderDoes(t *testing.T) {
	t.Run("id and createdAt hoist out of the block", func(t *testing.T) {
		tmpl := mustV1Mutation(t, "v1:x:y", v1MutationSrc{block: `{id: args.id, createdAt: now, name: args.name}`})
		require.IsType(t, &ast.MemberExpr{}, tmpl.IDTemplate)
		require.IsType(t, &ast.IdentExpr{}, tmpl.CreatedAtTemplate)
		payload, ok := tmpl.PayloadTemplate.(map[string]any)
		require.True(t, ok)
		require.Len(t, payload, 1)
		require.Contains(t, payload, "name")
	})
	t.Run("an object literal is a nested map, a list literal a slice", func(t *testing.T) {
		tmpl := mustV1Mutation(t, "v1:x:y", v1MutationSrc{block: `{meta: {a: args.a, b: [args.b, "c"]}, tags: args.tags ?? []}`})
		payload := tmpl.PayloadTemplate.(map[string]any)
		meta, ok := payload["meta"].(map[string]any)
		require.True(t, ok, "an object literal lays out as a nested map")
		require.IsType(t, []any{}, meta["b"])
		require.IsType(t, &ast.BinaryExpr{}, payload["tags"], "a list inside an operator is part of its node")
	})
	t.Run("a payload splat with explicit fields is splat plus overlay", func(t *testing.T) {
		tmpl := mustV1Mutation(t, "v1:x:y", v1MutationSrc{block: `{payload: args.payload, ownerUserId: actor.userId}`, id: `args.id`})
		require.IsType(t, &ast.MemberExpr{}, tmpl.PayloadTemplate)
		require.True(t, isPayloadSplat(tmpl.PayloadTemplate))
		require.Contains(t, tmpl.PayloadOverlayTemplate, "ownerUserId")
	})
	t.Run("a payload object literal is the field map", func(t *testing.T) {
		tmpl := mustV1Mutation(t, "v1:x:y", v1MutationSrc{block: `{payload: {a: 1}}`})
		payload, ok := tmpl.PayloadTemplate.(map[string]any)
		require.True(t, ok)
		require.Contains(t, payload, "a")
		require.Empty(t, tmpl.PayloadOverlayTemplate)
	})
	t.Run("no block is an empty payload", func(t *testing.T) {
		tmpl := mustV1Mutation(t, "v1:x:y", v1MutationSrc{id: `args.id`})
		require.Equal(t, map[string]any{}, tmpl.PayloadTemplate)
	})
}

// TestNewMutationTemplateV1RefusesAtLoad is every value the builder refuses
// when the tree is read -- each one a value that would otherwise have loaded
// and failed, or rendered wrong, on its first call.
func TestNewMutationTemplateV1RefusesAtLoad(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  v1MutationSrc
		want string
	}{
		{"an unbound name", v1MutationSrc{block: `{status: active}`}, "reads `active`, which it does not bind"},
		{"the retired ctx root", v1MutationSrc{block: `{name: ctx.name}`}, "reads `ctx`"},
		{"an event field outside an automation", v1MutationSrc{block: `{id: event.payload.id}`}, "reads `event`"},
		{"an unknown actor field", v1MutationSrc{block: `{owner: actor.userld}`}, "actor.userld is not a field of the actor envelope"},
		{"the actor read whole", v1MutationSrc{block: `{who: actor}`}, "has no whole value"},
		{"an unknown config key", v1MutationSrc{block: `{p: config.nope}`}, "not an allow-listed configuration key"},
		{"an unknown function", v1MutationSrc{block: `{v: frobnicate(args.x)}`}, "frobnicate() is not a function"},
		{"a function of the wrong shape", v1MutationSrc{block: `{v: hash(args.a, args.b)}`}, "argument_count"},
		{"a relationship traversal", v1MutationSrc{block: `{v: childOf(r => r.a == 1)}`}, "relationship traversal"},
		{"a construct call", v1MutationSrc{block: `{v: query activeUsers()}`}, "constructCall"},
		{"a method no receiver has", v1MutationSrc{block: `{v: args.xs.frob()}`}, ".frob() is not a method"},
		{"a lambda on its own", v1MutationSrc{block: `{v: x => x}`}, "lambda standing on its own"},
		{"a bad slot", v1MutationSrc{block: `{a: 1}`, id: `nope`}, "id=:"},
		{"the id written twice", v1MutationSrc{block: `{id: args.a}`, id: `args.b`}, "the id is written twice"},
		{"createdAt written twice", v1MutationSrc{block: `{createdAt: now}`, createdAt: `now`}, "createdAt is written twice"},
		{"a literal splat", v1MutationSrc{block: `{payload: "x"}`}, "can never be an object"},
		{"a nil splat", v1MutationSrc{block: `{payload: nil}`}, "can never be an object"},
		{"a block that is not a map", v1MutationSrc{block: `args.payload`}, "must be a map literal"},
		{"a scan nested in a scan", v1MutationSrc{block: `{v: args.xs.where(x => args.ys.any(y => args.zs.any(z => z == y && y == x)))}`}, "static cost estimate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildV1Mutation(t, "v1:x:y", tc.src)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestNewMutationTemplateV1RefusesADuplicateKey: a hand-built block may carry
// a key twice (the parser refuses it in source); the builder refuses too,
// because a map collapses it and the first value vanishes.
func TestNewMutationTemplateV1RefusesADuplicateKey(t *testing.T) {
	block := &ast.MapExpr{Entries: []ast.MapEntry{
		{Key: "a", Value: &ast.LiteralExpr{Value: int64(1)}},
		{Key: "a", Value: &ast.LiteralExpr{Value: int64(2)}},
	}}
	_, err := newMutationTemplateV1("", "v1:x:y", block, mutationSlotsV1{})
	require.ErrorContains(t, err, `field "a" is written twice`)
}

// TestNewMutationTemplateV1RefusesANodeOutsideV1: a legacy node reaching the
// v1 builder is a wiring defect, refused rather than rendered by guesswork.
func TestNewMutationTemplateV1RefusesANodeOutsideV1(t *testing.T) {
	_, err := newMutationTemplateV1("", "v1:x:y", nil, mutationSlotsV1{ID: &languageParser.ArgRefExpr{Path: "id"}})
	require.ErrorContains(t, err, "not an edition-2026 expression node")
}

// ---------------------------------------------------------------------------
// twins of the string half's pinned tests
// ---------------------------------------------------------------------------

// TestMutationValuesV1TernaryComparison is the twin of
// cond_quoted_string_3618_test.go: `cond(p, a, b)` is `p ? a : b` in v1, and a
// quoted literal compares as its content (the lexer decoded it). Two of the
// legacy rows answer differently BY DESIGN (D8, no truthiness, and no bare
// name that is not bound), and are asserted below the table.
func TestMutationValuesV1TernaryComparison(t *testing.T) {
	args := map[string]any{"s": "active", "other": "active", "n": int64(5), "blank": ""}
	for _, tc := range []struct {
		pred string
		want string
	}{
		{`args.s == "active"`, "T"},
		{`args.s != "active"`, "F"},
		{`"active" == args.s`, "T"},
		{`args.s == "archived"`, "F"},
		{`args.s != "archived"`, "T"},
		{`args.missing == ""`, "T"},
		{`args.blank == ""`, "T"},
		{`args.n == 5`, "T"},
		{`args.s == args.other`, "T"},
		{`true`, "T"},
		{`false`, "F"},
		{`args.n > 4`, "T"},
		{`args.n > 5`, "F"},
		{`args.n >= 5`, "T"},
		{`args.n < 6`, "T"},
		{`args.n <= 4`, "F"},
		// A missing argument in a condition is false, never an error.
		{`args.missing`, "F"},
	} {
		t.Run(tc.pred, func(t *testing.T) {
			got, _, err := v1ValueOf(t, context.Background(), tc.pred+` ? "T" : "F"`, args)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}

	// CHANGED (D8): a string is not a condition. The string half read
	// `cond(args.s, ...)` for truthiness and took the THEN branch.
	_, _, err := v1ValueOf(t, context.Background(), `args.s ? "T" : "F"`, args)
	require.ErrorContains(t, err, "condition_not_boolean")

	// CHANGED: `args.s == active` compared against the literal text
	// "active" in the string half; in v1 `active` is a name, unbound here,
	// and refused where the tree is read.
	_, _, err = v1ValueOf(t, context.Background(), `args.s == active ? "T" : "F"`, args)
	require.ErrorContains(t, err, "reads `active`")
}

// TestMutationValuesV1BooleanIsNotTruthy is the twin of
// TestTruthinessBlastRadius_MutationTemplateConditional. CHANGED (D8): the
// string half read the historically-permissive values through IsTruthy
// ("false", "0", 0, [] and {} false; a non-empty string true); v1 has no
// truthiness, so each is REFUSED as a condition rather than read as one --
// the fail-closed answer to a question the value cannot answer.
func TestMutationValuesV1BooleanIsNotTruthy(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
	}{
		{`the string "false"`, "false"},
		{`the string "0"`, "0"},
		{"zero", int64(0)},
		{"empty list", []any{}},
		{"empty object", map[string]any{}},
		{"non-empty string", "nonempty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := v1ValueOf(t, context.Background(), `args.allowed ? "yes" : "no"`, map[string]any{"allowed": tc.in})
			require.ErrorContains(t, err, "condition_not_boolean")
		})
	}
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"true", map[string]any{"allowed": true}, "yes"},
		{"false", map[string]any{"allowed": false}, "no"},
		{"absent is false", map[string]any{}, "no"},
		{"nil is false", map[string]any{"allowed": nil}, "no"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := v1ValueOf(t, context.Background(), `args.allowed ? "yes" : "no"`, tc.args)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestMutationValuesV1CoalesceIsTheOneSelectionRule is the twin of
// coalesce_array_missing_3627_test.go: `??` is blank-coalescing, and the v1
// spelling selects exactly what the string half's coalesce() selects for the
// same inputs -- both are driven through coalesceSelect.
func TestMutationValuesV1CoalesceIsTheOneSelectionRule(t *testing.T) {
	for _, tc := range []struct {
		v1, legacy string
		args       map[string]any
	}{
		{`args.a ?? "" ?? args.c`, `coalesce(args.a, "", args.c)`, map[string]any{}},
		{`args.a ?? "" ?? args.c`, `coalesce(args.a, "", args.c)`, map[string]any{"c": "C"}},
		{`args.a ?? ""`, `coalesce(args.a, "")`, map[string]any{}},
		{`args.a ?? "B" ?? "C"`, `coalesce(args.a, "B", "C")`, map[string]any{}},
		{`args.a ?? "B"`, `coalesce(args.a, "B")`, map[string]any{"a": "A"}},
		{`args.a ?? "B"`, `coalesce(args.a, "B")`, map[string]any{"a": "  "}},
		{`args.a ?? args.b`, `coalesce(args.a, args.b)`, map[string]any{}},
		{`args.v ?? "DEFAULT"`, `coalesce(args.v, "DEFAULT")`, map[string]any{"v": "\t\n"}},
		{`args.v ?? "DEFAULT"`, `coalesce(args.v, "DEFAULT")`, map[string]any{"v": false}},
		{`args.v ?? "DEFAULT"`, `coalesce(args.v, "DEFAULT")`, map[string]any{"v": int64(0)}},
	} {
		t.Run(tc.v1, func(t *testing.T) {
			legacy, err := (&mutationTemplateEvaluator{args: tc.args}).evalString(context.Background(), tc.legacy)
			require.NoError(t, err)
			got, present, err := v1ValueOf(t, context.Background(), tc.v1, tc.args)
			require.NoError(t, err)
			require.True(t, present, "a ?? chain always yields a value (nil for an all-missing chain)")
			// JSON round trip on both sides: the payload is JSON.
			raw, err := json.Marshal(legacy)
			require.NoError(t, err)
			var want any
			require.NoError(t, json.Unmarshal(raw, &want))
			require.Equal(t, want, got, "v1 %s and legacy %s select differently (memql#3627)", tc.v1, tc.legacy)
		})
	}
}

// TestMutationValuesV1ListOmitsMissingKeepsNil is the twin of
// TestArrayLiteral_OmitsMissingArgs and TestArrayLiteral_KeepsExplicitNull.
func TestMutationValuesV1ListOmitsMissingKeepsNil(t *testing.T) {
	tmpl := mustV1Mutation(t, "v1:cognition:space", v1MutationSrc{block: `{v: [args.a, args.b, "c"], w: [nil, args.b]}`})
	_, payload := renderV1Payload(t, &MemQLEngine{}, context.Background(), tmpl, map[string]any{"b": "B"})
	require.Equal(t, []any{"B", "c"}, payload["v"], "a missing argument contributes nothing to a list")
	require.Equal(t, []any{nil, "B"}, payload["w"], "an explicit nil is a value the author wrote")
}

// TestMutationValuesV1OverlayOmitsAMissingArgument pins the container rule on
// the overlay. CHANGED: the string half wrote its missing sentinel into the
// overlay unchecked, and the sentinel marshals as `{}` -- so
// `{payload: args.payload, note: args.note}` without a note stored
// `"note": {}`, an object nobody sent (measured on this branch). In v1 the
// missing argument omits its key and the splat's own value stands.
func TestMutationValuesV1OverlayOmitsAMissingArgument(t *testing.T) {
	tmpl := mustV1Mutation(t, "v1:x:y", v1MutationSrc{
		block: `{payload: args.payload, note: args.note, ownerUserId: actor.userId}`,
		id:    `args.id`,
	})
	ctx := auth.ContextWithUserActor(context.Background(), "user-overlay")
	caller := map[string]any{"a": float64(1), "note": "from-splat", "ownerUserId": "forged"}
	_, payload := renderV1Payload(t, &MemQLEngine{}, ctx, tmpl, map[string]any{"id": "x-1", "payload": caller})
	require.Equal(t, "from-splat", payload["note"], "the absent overlay value leaves the splat's value standing")
	require.Contains(t, payload["ownerUserId"], "user-overlay", "an overlay stamp wins over the splat (memql#401)")
	require.Equal(t, float64(1), payload["a"])
	require.Equal(t, "forged", caller["ownerUserId"], "rendering must not write into the caller's own map")
}

// TestMutationValuesV1ActorInsideACall is the twin of
// actor_in_call_id_render_4746_test.go: an actor reference inside a call in
// an id derivation renders, one person derives one id and two derive two.
func TestMutationValuesV1ActorInsideACall(t *testing.T) {
	tmpl := mustV1Mutation(t, "v1:os:desktop", v1MutationSrc{id: `"desktop-" + hash(actor.userId)`, block: `{a: 1}`})
	derive := func(user string) string {
		node, err := (&MemQLEngine{}).renderMutationTemplate(auth.ContextWithUserActor(context.Background(), user), tmpl, nil)
		require.NoError(t, err)
		return node.ID
	}
	first := derive("user-abc")
	require.Equal(t, "desktop-"+sha256Hex("user-abc"), first)
	require.NotContains(t, first, ":", "a derived row id is a bare slug")
	require.Equal(t, first, derive("user-abc"), "one person, one id")
	require.NotEqual(t, first, derive("user-xyz"), "two people, two ids")

	// An unknown actor path refuses at LOAD in v1 (the string half refused
	// it at render); the render-time refusal is still there as the backstop
	// for a template built past the check.
	_, err := buildV1Mutation(t, "v1:os:desktop", v1MutationSrc{id: `hash(actor.notAField)`})
	require.ErrorContains(t, err, "actor.notAField is not a field")
	past := &FunctionMutationTemplate{Concept: "v1:os:desktop", ValuesV1: true, IDTemplate: v1Expr(t, `hash(actor.notAField)`), PayloadTemplate: map[string]any{}}
	_, err = (&MemQLEngine{}).renderMutationTemplate(auth.ContextWithUserActor(context.Background(), "u"), past, nil)
	require.ErrorContains(t, err, "unsupported actor reference path")
}

// TestMutationValuesV1ShortIdInAnIdDerivation is the twin of
// shortid_id_render_2925_test.go: shortId renders in an id slot, canonical and
// bare derive one id, it is a no-op on a bare id, and the separator aliasing
// is still live at the evaluator (it is closed at the argument boundary).
func TestMutationValuesV1ShortIdInAnIdDerivation(t *testing.T) {
	derive := func(dep, node string, normalise bool) string {
		fk := "args.deploymentId"
		if normalise {
			fk = "shortId(args.deploymentId)"
		}
		tmpl := mustV1Mutation(t, "v1:cluster:deploymentNodeSpec", v1MutationSrc{id: `hash(` + fk + ` + ":" + args.nodeType)`, block: `{a: 1}`})
		n, err := (&MemQLEngine{}).renderMutationTemplate(context.Background(), tmpl, map[string]any{"deploymentId": dep, "nodeType": node})
		require.NoError(t, err)
		return n.ID
	}
	require.Equal(t, derive("abc123", "bff", true), derive("v1:cluster:deployment:abc123", "bff", true))
	bare := "9f8e7d6c-1234-4abc-9def-000000000001"
	require.Equal(t, derive(bare, "bff", false), derive(bare, "bff", true))
	require.Equal(t, derive("d:x", "y", true), derive("d", "x:y", true), "the aliasing is closed by @pattern, not by the derivation")

	got, _, err := v1ValueOf(t, context.Background(), `shortId(args.deploymentId)`, map[string]any{"deploymentId": "v1:cluster:deployment:abc123"})
	require.NoError(t, err)
	require.Equal(t, "abc123", got)
}

// TestMutationValuesV1HashIsFixedWidth is the twin of
// hash_fixed_width_test.go: hash() of every input, a missing one included,
// is 64 characters, so an absent part cannot alias a composite id.
func TestMutationValuesV1HashIsFixedWidth(t *testing.T) {
	args := map[string]any{"present": "x", "action": map[string]any{"type": "chat"}}
	for _, src := range []string{`hash(args.present)`, `hash(args.absent)`, `hash(args.action.type)`, `hash(args.action.idempotencyKey)`, `hash("")`} {
		got, _, err := v1ValueOf(t, context.Background(), src, args)
		require.NoError(t, err)
		require.Len(t, got, 64, "%s", src)
	}
	first, _, err := v1ValueOf(t, context.Background(), `hash(hash(args.none) + hash(args.x))`, map[string]any{"x": "x"})
	require.NoError(t, err)
	second, _, err := v1ValueOf(t, context.Background(), `hash(hash(args.x) + hash(args.none))`, map[string]any{"x": "x"})
	require.NoError(t, err)
	require.NotEqual(t, first, second, "(absent, x) and (x, absent) must derive two ids (memql#3009)")
}

// TestMutationValuesV1CoalesceInAnIdSlot is the twin of
// TestIdTemplateCoalesce_*: a blank non-final arm is skipped, the final arm
// is the fallback even when blank, and an all-missing chain is nil (so the
// engine mints an id, as the string half's AST path did).
func TestMutationValuesV1CoalesceInAnIdSlot(t *testing.T) {
	tmpl := mustV1Mutation(t, "v1:rbac:role", v1MutationSrc{id: `args.roleId ?? args.slug`, block: `{a: 1}`})
	id := func(args map[string]any) string {
		n, err := (&MemQLEngine{}).renderMutationTemplate(context.Background(), tmpl, args)
		require.NoError(t, err)
		return n.ID
	}
	require.Equal(t, "editor", id(map[string]any{"roleId": "", "slug": "editor"}), "a blank non-final arm is skipped (#1614)")
	require.Equal(t, "", id(map[string]any{"roleId": "", "slug": ""}), "the final arm is returned even when blank")
	require.Equal(t, "", id(map[string]any{}), "an all-missing chain is no id, and the engine mints one")
}

// TestMutationValuesV1NullCoalesceShapes is the twin of
// null_coalesce_payload_test.go's value shapes: a `??` arm that is an object
// or a list literal renders, and a `??` inside a string is data.
func TestMutationValuesV1NullCoalesceShapes(t *testing.T) {
	tmpl := mustV1Mutation(t, "v1:x:y", v1MutationSrc{block: `{
		labels: args.labels ?? {},
		tags: args.tags ?? [],
		nested: args.src ?? {inputMethod: "si"},
		plain: args.status ?? "healthy",
		note: "pass ?? to get a default",
		sinceAt: args.sinceAt ?? now
	}`})
	_, payload := renderV1Payload(t, &MemQLEngine{}, context.Background(), tmpl, nil)
	require.Equal(t, map[string]any{}, payload["labels"])
	require.Equal(t, []any{}, payload["tags"])
	require.Equal(t, map[string]any{"inputMethod": "si"}, payload["nested"])
	require.Equal(t, "healthy", payload["plain"])
	require.Equal(t, "pass ?? to get a default", payload["note"])
	_, err := time.Parse(time.RFC3339Nano, payload["sinceAt"].(string))
	require.NoError(t, err)

	// A malformed `??` never reaches the builder: the v1 parser refuses it
	// (the string half needed a sentinel and a load-time sweep for it).
	for _, src := range []string{`{active: args.a ?? }`, `{active: args.a ?? ?? args.b}`, `{tags: [args.a ??]}`} {
		_, err := languageParser.ParseV1Expression(src)
		require.Errorf(t, err, "%s must not parse", src)
	}
}

// TestMutationValuesV1MalformedLiteralsRefuseAndTerminate is the twin of
// mutation_templates_no_progress_test.go: every unbalanced literal that hung
// the string half's hand-rolled scanner is refused by the v1 parser, and in
// bounded time.
func TestMutationValuesV1MalformedLiteralsRefuseAndTerminate(t *testing.T) {
	for _, src := range []string{
		"[}{]", "[}]", "[ } { ]", "[a, }{]", "[[}{]]", `["ok", }{]`, "[]]", "[{]", `["unterminated]`, "[[)]",
		"{k: [}{]}", "{ a: }", "{a b}", "{{}", `{"k": 1}`,
	} {
		src := src
		var err error
		if !t.Run(src, func(t *testing.T) {
			if mustTerminate(t, "ParseV1Expression("+src+")", func() { _, err = languageParser.ParseV1Expression(src) }) {
				require.Errorf(t, err, "%s must be refused", src)
			}
		}) {
			break
		}
	}
}

// TestMutationValuesV1ActorUserIdNeedsACaller is the twin of the write half
// of actor_userid_no_caller_3620_test.go, over createWorkRun's insert block
// as it reads in edition 2026.
func TestMutationValuesV1ActorUserIdNeedsACaller(t *testing.T) {
	tmpl := mustV1Mutation(t, "v1:work:run", v1MutationSrc{block: `{
		goalId: args.goalId, automationName: args.automationName,
		templateFingerprint: args.templateFingerprint, status: args.status, startedAt: args.startedAt,
		id: args.runId,
		ownerUserId: actor.userId,
		mode: args.mode ?? "live",
		callerSuppliedPayload: args.callerSuppliedPayload ?? false,
		replayPolicy: args.replayPolicy ?? "strict",
		cancelRequested: false
	}`})
	args := mintArgs()

	_, err := (&MemQLEngine{}).renderMutationTemplate(context.Background(), tmpl, args)
	require.ErrorIs(t, err, auth.ErrActorEnvelopeNoCaller, "no caller, no owner: refused (memql#3620)")

	blank := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "   ", Role: auth.RoleWriter})
	_, err = (&MemQLEngine{}).renderMutationTemplate(blank, tmpl, args)
	require.ErrorIs(t, err, auth.ErrActorEnvelopeNoCaller, "a blank caller names nobody")

	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "user-3620", Role: auth.RoleWriter})
	node, payload := renderV1Payload(t, &MemQLEngine{}, ctx, tmpl, args)
	require.Equal(t, "user-3620", payload["ownerUserId"])
	require.Equal(t, "run-3620", node.ID)
	require.Equal(t, "live", payload["mode"])
	require.Equal(t, false, payload["callerSuppliedPayload"])
	require.NotContains(t, payload, "goalId", "a missing optional argument omits its key")

	// Under the seed materializer's system actor the stamp is its real,
	// non-empty synthetic identity.
	sys := systemActorContext(context.Background())
	_, payload = renderV1Payload(t, &MemQLEngine{}, sys, tmpl, args)
	require.NotEmpty(t, payload["ownerUserId"])
}

// TestMutationValuesV1ActorIsReadLazily: only a read that is evaluated may
// refuse. The untaken branch of `? :` and the unreached arm of `??` read
// nothing -- which is what the actor being an ExprMembers (rather than a map
// built up front) buys.
func TestMutationValuesV1ActorIsReadLazily(t *testing.T) {
	for _, src := range []string{
		`args.owner ?? actor.userId`,
		`args.system == true ? args.owner : actor.userId`,
	} {
		t.Run(src, func(t *testing.T) {
			got, _, err := v1ValueOf(t, context.Background(), src, map[string]any{"owner": "o-1", "system": true})
			require.NoError(t, err)
			require.Equal(t, "o-1", got)
			_, _, err = v1ValueOf(t, context.Background(), src, map[string]any{"system": false})
			require.ErrorIs(t, err, auth.ErrActorEnvelopeNoCaller, "the read that happens still refuses")
		})
	}
	// Every other envelope field binds against an absent caller, as on the
	// filter path: only userId refuses (auth.ActorEnvelopeBind).
	for _, f := range []string{"role", "identityId", "primaryEmail", "isClusterOwner", "now"} {
		_, _, err := v1ValueOf(t, context.Background(), `actor.`+f, nil)
		require.NoErrorf(t, err, "actor.%s", f)
	}
}

// TestMutationValuesV1SlotsTakeText: an id is text. CHANGED: a list or a map
// in an id slot is refused; the string half wrote Go's own "%v" of it.
func TestMutationValuesV1SlotsTakeText(t *testing.T) {
	tmpl := mustV1Mutation(t, "v1:x:y", v1MutationSrc{id: `args.id`, block: `{a: 1}`})
	for _, tc := range []struct {
		id   any
		want string
	}{
		{"x-1", "x-1"},
		{float64(5), "5"},
		{int64(7), "7"},
		{true, "true"},
		{nil, ""},
	} {
		n, err := (&MemQLEngine{}).renderMutationTemplate(context.Background(), tmpl, map[string]any{"id": tc.id})
		require.NoError(t, err)
		require.Equal(t, tc.want, n.ID)
	}
	_, err := (&MemQLEngine{}).renderMutationTemplate(context.Background(), tmpl, map[string]any{"id": []any{"a"}})
	require.ErrorContains(t, err, "this slot takes text")
}

// TestMutationValuesV1SplatMustBeAnObject: the splat evaluates to an object
// or the render refuses, as the string half's did.
func TestMutationValuesV1SplatMustBeAnObject(t *testing.T) {
	tmpl := mustV1Mutation(t, "v1:x:y", v1MutationSrc{block: `{payload: args.payload}`, id: `args.id`})
	_, err := (&MemQLEngine{}).renderMutationTemplate(context.Background(), tmpl, map[string]any{"id": "x"})
	require.ErrorContains(t, err, "payload must evaluate to an object")
	_, err = (&MemQLEngine{}).renderMutationTemplate(context.Background(), tmpl, map[string]any{"id": "x", "payload": "text"})
	require.ErrorContains(t, err, "payload must evaluate to an object")
}

// TestMutationValuesV1AgreesWithTheStringHalf renders one template through
// both renderers -- the legacy layout the loader builds today and the v1
// layout newMutationTemplateV1 builds -- over the values both spell the same
// way, and requires one payload. It is the evidence that the flip changes no
// stored row for these shapes.
func TestMutationValuesV1AgreesWithTheStringHalf(t *testing.T) {
	legacy, err := parsePayloadRawToTemplate(`{
		name: args.name, status: "active", active: true, count: 3,
		description: args.description, tags: [args.a, args.b, "c"],
		meta: {kind: args.kind, note: coalesce(args.note, "none")},
		mode: args.mode ?? "live", ref: concat("r-", hash(args.name))
	}`)
	require.NoError(t, err)
	v1 := mustV1Mutation(t, "v1:x:y", v1MutationSrc{block: `{
		name: args.name, status: "active", active: true, count: 3,
		description: args.description, tags: [args.a, args.b, "c"],
		meta: {kind: args.kind, note: args.note ?? "none"},
		mode: args.mode ?? "live", ref: "r-" + hash(args.name)
	}`})
	args := map[string]any{"name": "N", "b": "B", "kind": "k"}
	e := &MemQLEngine{}
	wantNode, err := e.renderMutationTemplate(context.Background(), &FunctionMutationTemplate{Concept: "v1:x:y", PayloadTemplate: legacy}, args)
	require.NoError(t, err)
	gotNode, err := e.renderMutationTemplate(context.Background(), v1, args)
	require.NoError(t, err)
	require.JSONEq(t, wantNode.PayloadRaw, gotNode.PayloadRaw)
}

// TestExprMembersIsAskedOnlyForTheReadsThatHappen pins the EvalExpr hook the
// actor envelope rides on: a member read on an ExprMembers value is its own
// answer, its error reaches the caller unchanged, and a read that is not
// evaluated asks nothing.
func TestExprMembersIsAskedOnlyForTheReadsThatHappen(t *testing.T) {
	sentinel := errors.New("refused by the value")
	probe := &exprMembersProbe{answers: map[string]any{"ok": "yes"}, refuse: map[string]error{"no": sentinel}}
	scope := MapScope{"v": probe}

	got, err := EvalExpr(context.Background(), v1Expr(t, `v.ok`), scope, EvalOptions{})
	require.NoError(t, err)
	require.Equal(t, "yes", got)

	_, err = EvalExpr(context.Background(), v1Expr(t, `v.no`), scope, EvalOptions{})
	require.ErrorIs(t, err, sentinel, "the value's refusal reaches the caller unchanged")

	probe.asked = nil
	got, err = EvalExpr(context.Background(), v1Expr(t, `true ? v.ok : v.no`), scope, EvalOptions{})
	require.NoError(t, err)
	require.Equal(t, "yes", got)
	require.Equal(t, []string{"ok"}, probe.asked, "the untaken branch asked nothing")

	got, err = EvalExpr(context.Background(), v1Expr(t, `v.missing`), scope, EvalOptions{})
	require.NoError(t, err)
	require.True(t, IsAbsent(got))
}

type exprMembersProbe struct {
	answers map[string]any
	refuse  map[string]error
	asked   []string
}

func (p *exprMembersProbe) ExprMember(field string) (any, error) {
	p.asked = append(p.asked, field)
	if err, ok := p.refuse[field]; ok {
		return nil, err
	}
	if v, ok := p.answers[field]; ok {
		return v, nil
	}
	return Absent, nil
}

// TestMutationValuesV1CheckReachesEveryPositiveShape is the reachable
// positive for the load-time check: every shape in the rendering matrix
// passes it, so a refusal above is about its input and not a check that
// refuses everything.
func TestMutationValuesV1CheckReachesEveryPositiveShape(t *testing.T) {
	for _, src := range []string{
		`args.s`, `"x"`, `1`, `nil`, `now`, `actor.userId`, `config.defaultProvider`,
		`args.a ?? args.b ?? ""`, `hash(args.a + "-" + args.b)`, `shortId(args.x)`,
		`canonicalId(args.x, "v1:a:b")`, `args.xs.where(x => x.on == true).count()`,
		`args.p ? "a" : "b"`, `[args.a, {k: args.b}]`, `var("X")`, `addDuration(now, "P1D")`,
	} {
		require.NoErrorf(t, mutationValueCheck.check(v1Expr(t, src)), "%s", src)
	}
	require.True(t, strings.Contains(mutationValueCheck.rootList(), "actor"))
}

// ---------------------------------------------------------------------------
// the layout gates read a v1 leaf
// ---------------------------------------------------------------------------

// TestMutationValuesV1CallerArgGate: C5 (memql#2035) refuses a v1 mutation
// that writes an @internal or @serverSet field from caller args, exactly as
// it refuses the string half's -- without the v1 reading it would pass every
// v1 mutation, finding no "args." text in a parsed leaf.
func TestMutationValuesV1CallerArgGate(t *testing.T) {
	concept := mustConcept(t, `
concept Space {
  name       string  @required
  status     string  @serverSet
  secretSalt string  @internal
}
`, "v1/cognition/space")
	reg := conceptRegistry(concept)
	layout := func(block string) map[string]any {
		tmpl := mustV1Mutation(t, "v1:cognition:space", v1MutationSrc{block: block})
		return tmpl.PayloadTemplate.(map[string]any)
	}

	err := validateMutationCallerArgs(reg, "v1:cognition:space", "mutBad", layout(`{name: args.name, status: args.status}`))
	require.ErrorContains(t, err, "status")
	err = validateMutationCallerArgs(reg, "v1:cognition:space", "mutBad2", layout(`{secretSalt: args.salt.value}`))
	require.ErrorContains(t, err, "secretSalt")

	err = validateMutationCallerArgs(reg, "v1:cognition:space", "mutGood", layout(`{name: args.name, status: "active", secretSalt: actor.userId}`))
	require.NoError(t, err, "public from args and sensitive from a literal or the actor are allowed")

	// The leaf test is the one the string half applied: a value that IS a
	// caller argument. A quoted "args.status" is a literal in v1 -- the
	// string half could not tell the two apart.
	err = validateMutationCallerArgs(reg, "v1:cognition:space", "mutQuoted", layout(`{status: "args.status"}`))
	require.NoError(t, err)
}

// TestMutationValuesV1NestedObjectGuard: memql#3617's guard sees a v1
// template's nested object literal and its bare optional-arg leaves.
func TestMutationValuesV1NestedObjectGuard(t *testing.T) {
	tmpl := mustV1Mutation(t, "v1:identity:identity", v1MutationSrc{
		kind:  ast.MutationKindUpdate,
		id:    `args.id`,
		block: `{credentials: {backupEligible: args.backupEligible, signCount: args.signCount}, label: args.label}`,
	})
	fn := &Function{
		Name:             "zzUpdateCredentials",
		MutationTemplate: tmpl,
		ArgsSchema: &ArgsSchemaConfig{Fields: []*FunctionArgsField{
			{Name: "id"}, {Name: "backupEligible", Optional: true}, {Name: "signCount", Optional: true}, {Name: "label", Optional: true},
		}},
	}
	require.Equal(t, []string{"credentials"}, destructiveNestedObjectFields(fn),
		"rebuilding an object from optional args drops the leaves a call omits")

	tmpl.MergeFields = []string{"credentials"}
	require.Empty(t, destructiveNestedObjectFields(fn), "@mergeFields makes the write a deep merge")
}

// TestMutationValuesV1OwnerProvenance: memql#2982's analysis reads a v1
// template: the actor stamp, a caller write, a caller-controllable fallback,
// and a splat re-stamped by its overlay.
func TestMutationValuesV1OwnerProvenance(t *testing.T) {
	mut := func(name string, src v1MutationSrc) *Function {
		return &Function{Name: name, FunctionKind: "mutation", MutationTemplate: mustV1Mutation(t, "v1:x:owned", src)}
	}
	for _, tc := range []struct {
		name    string
		src     v1MutationSrc
		stamped bool
	}{
		{"the actor stamp", v1MutationSrc{block: `{ownerUserId: actor.userId, title: args.title}`}, true},
		{"a caller write", v1MutationSrc{block: `{ownerUserId: args.ownerUserId}`}, false},
		{"a caller-controllable fallback", v1MutationSrc{block: `{ownerUserId: args.owner ?? actor.userId}`}, false},
		{"a derived caller value", v1MutationSrc{block: `{ownerUserId: "u-" + args.owner}`}, false},
		{"a splat re-stamped by its overlay", v1MutationSrc{block: `{payload: args.payload, ownerUserId: actor.userId}`, id: `args.id`}, true},
		{"a splat with no overlay", v1MutationSrc{block: `{payload: args.payload}`, id: `args.id`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ownerProvenanceFor("v1:x:owned", "ownerUserId", []*Function{mut("zz", tc.src)})
			require.Equalf(t, tc.stamped, got.ServerStamped, "%+v", got)
		})
	}
	// The self-owned clause reads the id slot the same way.
	got := ownerProvenanceFor("v1:x:owned", "id", []*Function{mut("zz", v1MutationSrc{id: `"d-" + hash(actor.userId)`, block: `{a: 1}`})})
	require.True(t, got.ServerStamped, "%+v", got)
	got = ownerProvenanceFor("v1:x:owned", "id", []*Function{mut("zz", v1MutationSrc{id: `args.id`, block: `{a: 1}`})})
	require.False(t, got.ServerStamped, "%+v", got)
}

// TestMutationValuesV1HashInASlotIsFixedWidth pins the one hash rule in the
// id / createdAt / parent / aliasOf slots too. CHANGED: those slots were the
// string half's lowered-AST path, whose hash() of a missing value returned ""
// -- zero width, so `id: hash(args.missing)` minted a random id on every call
// and a composite `hash(hash(a) + hash(b))` with a missing part aliased
// (memql#3009's hazard, live in the one position it was about; measured on
// this branch). The payload path already hashed "". In v1 every position
// hashes a missing value as "" -- 64 characters -- so an id derived from a
// missing OPTIONAL part is now deterministic. For a required part (every
// per-part hashed id in the tree today) the derived id is byte-identical.
func TestMutationValuesV1HashInASlotIsFixedWidth(t *testing.T) {
	tmpl := mustV1Mutation(t, "v1:x:y", v1MutationSrc{id: `hash(args.absent)`, block: `{a: 1}`})
	node, err := (&MemQLEngine{}).renderMutationTemplate(context.Background(), tmpl, nil)
	require.NoError(t, err)
	require.Equal(t, sha256Hex(""), node.ID)

	legacy, err := (&MemQLEngine{}).renderMutationTemplate(context.Background(), &FunctionMutationTemplate{
		Concept:         "v1:x:y",
		IDTemplate:      &languageParser.HashExpr{Target: &languageParser.ArgRefExpr{Path: "absent"}},
		PayloadTemplate: map[string]any{},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, "", legacy.ID, "the string half's slot path: zero width, then a minted id")

	// Required parts: one id under both renderers.
	parts := map[string]any{"a": "x", "b": "y"}
	v1 := mustV1Mutation(t, "v1:x:y", v1MutationSrc{id: `hash(hash(args.a) + hash(args.b))`, block: `{a: 1}`})
	gotV1, err := (&MemQLEngine{}).renderMutationTemplate(context.Background(), v1, parts)
	require.NoError(t, err)
	gotLegacy, err := (&MemQLEngine{}).renderMutationTemplate(context.Background(), &FunctionMutationTemplate{
		Concept: "v1:x:y",
		IDTemplate: &languageParser.HashExpr{Target: &languageParser.ConcatExpr{Args: []languageParser.ExpressionNode{
			&languageParser.HashExpr{Target: &languageParser.ArgRefExpr{Path: "a"}},
			&languageParser.HashExpr{Target: &languageParser.ArgRefExpr{Path: "b"}},
		}}},
		PayloadTemplate: map[string]any{},
	}, parts)
	require.NoError(t, err)
	require.Equal(t, gotLegacy.ID, gotV1.ID)
}

// TestMutationValuesV1PlusIsNotConcat records where `+` and the string half's
// concat() part ways. `+` is concatenation when EITHER side is a string (so
// the codemod's concat(a, b) -> a + b is exact whenever the call has a
// string operand, as every concat in the tree does); with no string operand
// it is arithmetic, and it has no meaning for two absent values. CHANGED:
// concat(1, 2) was "12" and concat(missing, missing) was "".
func TestMutationValuesV1PlusIsNotConcat(t *testing.T) {
	args := map[string]any{"n": int64(1), "m": int64(2)}
	got, _, err := v1ValueOf(t, context.Background(), `args.n + args.m`, args)
	require.NoError(t, err)
	require.Equal(t, float64(3), got)
	got, _, err = v1ValueOf(t, context.Background(), `"" + args.n + args.m`, args)
	require.NoError(t, err)
	require.Equal(t, "12", got, "a string operand makes it concatenation, left to right")

	_, _, err = v1ValueOf(t, context.Background(), `args.x + args.y`, nil)
	require.ErrorContains(t, err, "operand_type")
	got, _, err = v1ValueOf(t, context.Background(), `"" + args.x + args.y`, nil)
	require.NoError(t, err)
	require.Equal(t, "", got)
}

// TestMutationValuesV1ConfigKeyIsExact: a config key in the wrong case is
// refused at load; the map the scope binds is keyed by the exact spelling.
func TestMutationValuesV1ConfigKeyIsExact(t *testing.T) {
	_, err := buildV1Mutation(t, "v1:x:y", v1MutationSrc{block: `{p: config.DemoMode}`})
	require.ErrorContains(t, err, "not an allow-listed configuration key")
	_, err = buildV1Mutation(t, "v1:x:y", v1MutationSrc{block: `{p: config.demoMode}`})
	require.NoError(t, err)
}
