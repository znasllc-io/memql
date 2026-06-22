package memql

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestCanonicalizeRelationshipComparisons exercises the engine's
// query-time auto-canon pre-walk. Symmetric to
// canonicalizeRelationshipFields (insert side); without this pass, a
// query with `payload.spaceId == "bare-slug"` would miss canonical-
// stored rows and the daily-space presence panel would render empty
// on first load.
func TestCanonicalizeRelationshipComparisons(t *testing.T) {
	engine := newTestEngineWithConcepts(t, map[string]*memoryNodes.Concept{
		"v1:identity:user":   {Name: "v1:identity:user"},
		"v1:cognition:space": {Name: "v1:cognition:space"},
		"v1:cognition:participant": {
			Name: "v1:cognition:participant",
			Relationships: []memoryNodes.RelationshipDefinition{
				{Type: "parent", Field: "spaceId", TargetConcept: "v1:cognition:space", Direction: "outgoing"},
				{Type: "parent", Field: "userId", TargetConcept: "v1:identity:user", Direction: "outgoing"},
			},
		},
	})
	ctx := context.Background()

	t.Run("rewrites payload.spaceId with bare slug to canonical", func(t *testing.T) {
		expr := &ComparisonExpression{
			Field:    FieldReference{Parts: []string{"payload", "spaceId"}, Raw: "payload.spaceId"},
			Operator: OpEq,
			Value:    "daily-9dc3b323-2026-05-06",
		}
		out := engine.canonicalizeRelationshipComparisons(ctx, expr, "v1:cognition:participant")
		got, ok := out.(*ComparisonExpression)
		require.Truef(t, ok, "expected *ComparisonExpression, got %T", out)
		require.Equal(t, "v1:cognition:space:daily-9dc3b323-2026-05-06", got.Value)
		// Original is left alone (caching invariant).
		require.Equal(t, "daily-9dc3b323-2026-05-06", expr.Value)
	})

	t.Run("rewrites payload.userId for global concept to _system prefix", func(t *testing.T) {
		expr := &ComparisonExpression{
			Field:    FieldReference{Parts: []string{"payload", "userId"}, Raw: "payload.userId"},
			Operator: OpEq,
			Value:    "user-abc",
		}
		out := engine.canonicalizeRelationshipComparisons(ctx, expr, "v1:cognition:participant")
		got := out.(*ComparisonExpression)
		require.Equal(t, "v1:identity:user:user-abc", got.Value)
	})

	t.Run("already-canonical RHS is unchanged (no AST clone)", func(t *testing.T) {
		expr := &ComparisonExpression{
			Field:    FieldReference{Parts: []string{"payload", "spaceId"}, Raw: "payload.spaceId"},
			Operator: OpEq,
			Value:    "v1:cognition:space:daily-abc",
		}
		out := engine.canonicalizeRelationshipComparisons(ctx, expr, "v1:cognition:participant")
		require.Same(t, expr, out, "no rewrite should return the original node")
	})

	t.Run("non-relationship payload field passes through", func(t *testing.T) {
		expr := &ComparisonExpression{
			Field:    FieldReference{Parts: []string{"payload", "displayName"}, Raw: "payload.displayName"},
			Operator: OpEq,
			Value:    "Sofia",
		}
		out := engine.canonicalizeRelationshipComparisons(ctx, expr, "v1:cognition:participant")
		require.Same(t, expr, out)
	})

	t.Run("intrinsic id field (not payload.X) passes through", func(t *testing.T) {
		expr := &ComparisonExpression{
			Field:    FieldReference{Parts: []string{"id"}, Raw: "id"},
			Operator: OpEq,
			Value:    "anything",
		}
		out := engine.canonicalizeRelationshipComparisons(ctx, expr, "v1:cognition:participant")
		require.Same(t, expr, out)
	})

	t.Run("logical AND propagates rewrites into both arms", func(t *testing.T) {
		expr := &LogicalExpression{
			Op: LogicalAnd,
			Left: &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"payload", "spaceId"}, Raw: "payload.spaceId"},
				Operator: OpEq,
				Value:    "daily-abc",
			},
			Right: &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"payload", "userId"}, Raw: "payload.userId"},
				Operator: OpEq,
				Value:    "user-xyz",
			},
		}
		out := engine.canonicalizeRelationshipComparisons(ctx, expr, "v1:cognition:participant")
		got := out.(*LogicalExpression)
		require.Equal(t, "v1:cognition:space:daily-abc", got.Left.(*ComparisonExpression).Value)
		require.Equal(t, "v1:identity:user:user-xyz", got.Right.(*ComparisonExpression).Value)
	})

	t.Run("no concept context skips rewrites", func(t *testing.T) {
		expr := &ComparisonExpression{
			Field:    FieldReference{Parts: []string{"payload", "spaceId"}, Raw: "payload.spaceId"},
			Operator: OpEq,
			Value:    "daily-abc",
		}
		out := engine.canonicalizeRelationshipComparisons(ctx, expr, "")
		require.Same(t, expr, out)
	})
}

// TestCanonicalIdExpr_ParsesAsTypedNode catches the lower-cased
// dispatch trap that originally landed canonicalId() through the
// generic FunctionCallExpr fallback (and made every mutation using
// it fail at evaluate-id time with "unsupported expression: *ast.
// FunctionCallExpr"). Direct round-trip from source through the
// parser to verify the AST type is the typed CanonicalIdExpr.
func TestCanonicalIdExpr_ParsesAsTypedNode(t *testing.T) {
	src := `canonicalId(args.userId, "v1:identity:user")`
	lex := languageParser.NewLexer(src)
	tokens, err := lex.Tokenize()
	require.NoError(t, err)
	p := languageParser.NewParser(tokens)
	node, err := p.Parse()
	require.NoError(t, err)
	cid, ok := node.(*ast.CanonicalIdExpr)
	require.Truef(t, ok, "expected *ast.CanonicalIdExpr, got %T", node)
	require.Equal(t, "v1:identity:user", cid.Concept)
}

// TestResolveCanonicalIdComparisons is the #1109 regression: an inlined
// canonicalId(...) on a comparison RHS must be resolved to its canonical
// string in the filter pre-walk. Without the fix the typed
// *ast.CanonicalIdExpr survives to the literal evaluator and fails the
// query with "unsupported literal type *ast.CanonicalIdExpr".
func TestResolveCanonicalIdComparisons(t *testing.T) {
	engine := newTestEngineWithConcepts(t, map[string]*memoryNodes.Concept{
		"v1:identity:user":   {Name: "v1:identity:user"},
		"v1:cognition:space": {Name: "v1:cognition:space"},
	})
	ctx := context.Background()

	t.Run("bare slug literal RHS resolves to canonical id", func(t *testing.T) {
		expr := &ComparisonExpression{
			Field:    FieldReference{Parts: []string{"payload", "spaceId"}, Raw: "payload.spaceId"},
			Operator: OpEq,
			Value:    &ast.CanonicalIdExpr{Value: &ast.LiteralExpr{Value: "daily-abc"}, Concept: "v1:cognition:space"},
		}
		out := engine.resolveCanonicalIdComparisons(ctx, expr)
		got, ok := out.(*ComparisonExpression)
		require.Truef(t, ok, "expected *ComparisonExpression, got %T", out)
		require.Equal(t, "v1:cognition:space:daily-abc", got.Value)
		// Original is left untouched (cached-plan invariant).
		_, stillExpr := expr.Value.(*ast.CanonicalIdExpr)
		require.True(t, stillExpr, "original comparison value must not be mutated")
	})

	t.Run("already-canonical literal RHS resolves to itself", func(t *testing.T) {
		expr := &ComparisonExpression{
			Field:    FieldReference{Parts: []string{"payload", "spaceId"}, Raw: "payload.spaceId"},
			Operator: OpEq,
			Value:    &ast.CanonicalIdExpr{Value: &ast.LiteralExpr{Value: "v1:cognition:space:daily-abc"}, Concept: "v1:cognition:space"},
		}
		out := engine.resolveCanonicalIdComparisons(ctx, expr).(*ComparisonExpression)
		require.Equal(t, "v1:cognition:space:daily-abc", out.Value)
	})

	t.Run("intrinsic id field with canonicalId RHS resolves to canonical", func(t *testing.T) {
		expr := &ComparisonExpression{
			Field:    FieldReference{Parts: []string{"id"}, Raw: "id"},
			Operator: OpEq,
			Value:    &ast.CanonicalIdExpr{Value: &ast.LiteralExpr{Value: "user-abc"}, Concept: "v1:identity:user"},
		}
		out := engine.resolveCanonicalIdComparisons(ctx, expr).(*ComparisonExpression)
		require.Equal(t, "v1:identity:user:user-abc", out.Value)
	})

	t.Run("canonicalId RHS resolves inside a logical AND alongside a bool comparison", func(t *testing.T) {
		// The exact shape from the staging repro: a canonicalId RHS in
		// one arm and a bool comparison in the other. Pre-fix this tree
		// reached normalizeScalarValue with the typed node.
		expr := &LogicalExpression{
			Op: LogicalAnd,
			Left: &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"payload", "spaceId"}, Raw: "payload.spaceId"},
				Operator: OpEq,
				Value:    &ast.CanonicalIdExpr{Value: &ast.LiteralExpr{Value: "daily-abc"}, Concept: "v1:cognition:space"},
			},
			Right: &ComparisonExpression{
				Field:    FieldReference{Parts: []string{"payload", "active"}, Raw: "payload.active"},
				Operator: OpEq,
				Value:    true,
			},
		}
		out := engine.resolveCanonicalIdComparisons(ctx, expr).(*LogicalExpression)
		require.Equal(t, "v1:cognition:space:daily-abc", out.Left.(*ComparisonExpression).Value)
		require.Equal(t, true, out.Right.(*ComparisonExpression).Value)
	})

	t.Run("non-canonicalId comparison passes through untouched", func(t *testing.T) {
		expr := &ComparisonExpression{
			Field:    FieldReference{Parts: []string{"payload", "spaceId"}, Raw: "payload.spaceId"},
			Operator: OpEq,
			Value:    "plain-string",
		}
		out := engine.resolveCanonicalIdComparisons(ctx, expr)
		require.Same(t, expr, out)
	})
}

// TestCanonicalIdArgBinding_RealLoadedQueries is the #1360 regression:
// the three shipped struct queries whose filter inlines
// `canonicalId(args.spaceId, space)` alongside another comparison must
// survive query-arg binding + the #1109 runtime pre-walk without
// leaving a typed `*ast.CanonicalIdExpr` in any comparison value.
//
// Unlike TestResolveCanonicalIdComparisons (synthetic ASTs with
// LiteralExpr inners), this loads the REAL embedded DSL tree and runs
// the REAL runtime entry (`engine.Parse` -> resolvePlanFunctions arg
// binding -> resolveCanonicalIdComparisons, the same pre-walk
// evaluateExpressionSet runs before SQL compile). In the live tree the
// CanonicalIdExpr inner is an `*ast.ArgRefExpr` (args.spaceId is NOT
// substituted inside canonicalId() during call-arg binding pre-fix),
// which the #1109 pre-walk could not resolve -- the node survived to
// the literal evaluator and failed the whole query with "unsupported
// literal type *ast.CanonicalIdExpr" (voice session stuck in
// Preparing).
func TestCanonicalIdArgBinding_RealLoadedQueries(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (dsl/ domain-first tree): %v", err)
	}
	registry := memoryNodes.DefaultRegistry()
	require.NotNil(t, registry)

	eng, err := New(nil)
	require.NoError(t, err)
	// Quiet the provider loader (no DB / no secrets here; same posture
	// as TestEngineInitLoadsFullDSL).
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(registry))

	ctx := context.Background()
	wantCanonical, err := eng.canonicalizeIdValue(ctx, "demo-space", "v1:cognition:space")
	require.NoError(t, err)
	require.NotEmpty(t, wantCanonical)

	cases := []struct {
		name string
		call string
	}{
		{"queryAudioOverridesForSpace", `queryAudioOverridesForSpace({spaceId: "demo-space"})`},
		{"queryVideoOverridesForSpace", `queryVideoOverridesForSpace({spaceId: "demo-space"})`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The real runtime entry: Parse runs resolvePlanFunctions,
			// which binds the call args into the loaded filter tree --
			// the exact path engine.Execute takes before evaluation.
			plan, err := eng.Parse(tc.call)
			require.NoError(t, err)
			require.NotNil(t, plan.Root)

			// Mirror evaluateExpressionSet's pre-walk (executor.go):
			// after this, NO comparison value may still carry the
			// typed node -- any survivor fails the query at the
			// literal evaluator.
			resolved := eng.resolveCanonicalIdComparisons(ctx, plan.Root)

			var spaceVal any
			walkComparisonExpressions(resolved, func(c *ComparisonExpression) {
				if _, isCid := c.Value.(*ast.CanonicalIdExpr); isCid {
					t.Errorf("comparison %q still carries *ast.CanonicalIdExpr after arg binding + canonicalId pre-walk; the query would fail with \"unsupported literal type *ast.CanonicalIdExpr\" (#1360)", c.Field.Raw)
				}
				if c.Field.Raw == "payload.spaceId" {
					spaceVal = c.Value
				}
			})
			require.Equal(t, wantCanonical, spaceVal,
				"payload.spaceId comparison must resolve to the canonical space id")

			// Cached-plan invariant (#1109): the registered function's
			// stored expression tree must NOT have been mutated by the
			// call -- its CanonicalIdExpr inner stays the arg reference.
			fn, err := eng.functions.Get(tc.name)
			require.NoError(t, err)
			require.NotNil(t, fn.Expr)
			foundStoredArgRef := false
			walkComparisonExpressions(fn.Expr, func(c *ComparisonExpression) {
				if cid, ok := c.Value.(*ast.CanonicalIdExpr); ok {
					if _, isArgRef := cid.Value.(*ast.ArgRefExpr); isArgRef {
						foundStoredArgRef = true
					}
				}
			})
			require.True(t, foundStoredArgRef,
				"registered function tree must keep its *ast.ArgRefExpr canonicalId inner (cached plans are never mutated in place)")
		})
	}
}

// walkComparisonExpressions visits every ComparisonExpression in an
// engine expression tree, descending through the wrapper node kinds a
// loaded struct-query expression can carry.
func walkComparisonExpressions(expr ExpressionNode, visit func(*ComparisonExpression)) {
	switch n := expr.(type) {
	case nil:
		return
	case *ComparisonExpression:
		if n != nil {
			visit(n)
		}
	case *LogicalExpression:
		if n != nil {
			walkComparisonExpressions(n.Left, visit)
			walkComparisonExpressions(n.Right, visit)
		}
	case *RelationshipExpression:
		if n != nil {
			walkComparisonExpressions(n.Target, visit)
		}
	case *ShapeExpression:
		if n != nil {
			walkComparisonExpressions(n.Target, visit)
		}
	case *SortExpression:
		if n != nil {
			walkComparisonExpressions(n.Target, visit)
		}
	case *PaginateExpression:
		if n != nil {
			walkComparisonExpressions(n.Target, visit)
		}
	case *SelectExpression:
		if n != nil {
			walkComparisonExpressions(n.Target, visit)
		}
	case *TimestampExpression:
		if n != nil {
			walkComparisonExpressions(n.Target, visit)
		}
	case *DepthExpression:
		if n != nil {
			walkComparisonExpressions(n.Target, visit)
		}
	case *ConditionalFilterExpression:
		if n != nil {
			walkComparisonExpressions(n.Filter, visit)
		}
	}
}

// newTestEngineWithConcepts builds a minimal engine seeded with the
// given concept definitions. Used by canonicalId + auto-canon tests
// that need the engine to resolve scope + relationship metadata.
func newTestEngineWithConcepts(t *testing.T, defs map[string]*memoryNodes.Concept) *MemQLEngine {
	t.Helper()
	reg := newMemoryRegistry(defs)
	return &MemQLEngine{
		concepts: reg,
	}
}

// newMemoryRegistry builds an in-memory concept registry from the
// supplied map. Avoids polluting memoryNodes.DefaultRegistry across
// parallel test runs.
func newMemoryRegistry(defs map[string]*memoryNodes.Concept) memoryNodes.Registry {
	r := &testRegistry{concepts: defs}
	return r
}

type testRegistry struct {
	concepts map[string]*memoryNodes.Concept
}

func (r *testRegistry) Get(name string) (*memoryNodes.Concept, error) {
	if c, ok := r.concepts[name]; ok {
		return c, nil
	}
	return nil, errNotFound{name: name}
}

func (r *testRegistry) List() []*memoryNodes.Concept {
	out := make([]*memoryNodes.Concept, 0, len(r.concepts))
	for _, c := range r.concepts {
		out = append(out, c)
	}
	return out
}

type errNotFound struct{ name string }

func (e errNotFound) Error() string { return "concept not found: " + e.name }

func TestCanonicalizeIdValue(t *testing.T) {
	engine := newTestEngineWithConcepts(t, map[string]*memoryNodes.Concept{
		"v1:identity:user":   {Name: "v1:identity:user"},
		"v1:cognition:space": {Name: "v1:cognition:space"}, // partition-scoped
		"v1:agents:agent":    {Name: "v1:agents:agent"},    // partition-scoped
	})
	ctx := context.Background()

	cases := []struct {
		name        string
		value       string
		conceptType string
		want        string
		wantErr     bool
	}{
		{
			name:        "empty value returns empty (optional fk)",
			value:       "",
			conceptType: "v1:identity:user",
			want:        "",
		},
		{
			name:        "bare slug for any concept gets concept prefix",
			value:       "user-abc",
			conceptType: "v1:identity:user",
			want:        "v1:identity:user:user-abc",
		},
		{
			name:        "bare slug for partition-domain concept also gets concept prefix",
			value:       "ga-xyz",
			conceptType: "v1:agents:agent",
			want:        "v1:agents:agent:ga-xyz",
		},
		{
			name:        "already-canonical value is returned as-is",
			value:       "v1:identity:user:user-abc",
			conceptType: "v1:identity:user",
			want:        "v1:identity:user:user-abc",
		},
		{
			name:        "canonical for wrong concept errors (caller passed wrong type tag)",
			value:       "v1:identity:user:user-abc",
			conceptType: "v1:cognition:space",
			wantErr:     true,
		},
		{
			name:        "unknown concept errors",
			value:       "anything",
			conceptType: "v1:does:not:exist",
			wantErr:     true,
		},
		{
			name:        "missing concept errors with empty value still passes",
			value:       "",
			conceptType: "v1:does:not:exist",
			want:        "", // empty value short-circuits before concept lookup
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := engine.canonicalizeIdValue(ctx, tc.value, tc.conceptType)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestCanonicalizeRelationshipFields(t *testing.T) {
	engine := newTestEngineWithConcepts(t, map[string]*memoryNodes.Concept{
		"v1:identity:user":   {Name: "v1:identity:user"},
		"v1:cognition:space": {Name: "v1:cognition:space"},
		"v1:cognition:participant": {
			Name: "v1:cognition:participant",
			Relationships: []memoryNodes.RelationshipDefinition{
				{Type: "parent", Field: "spaceId", TargetConcept: "v1:cognition:space", Direction: "outgoing"},
				{Type: "parent", Field: "userId", TargetConcept: "v1:identity:user", Direction: "outgoing"},
				// Reverse relationship -- engine should NOT rewrite it.
				{Type: "child", Field: "deliveredTo", TargetConcept: "v1:cognition:utterance", Direction: "incoming"},
			},
		},
	})
	ctx := context.Background()

	t.Run("outgoing fields canonicalize, incoming and untagged fields untouched", func(t *testing.T) {
		payload := map[string]any{
			"spaceId":     "daily-2026-05-06", // bare -> default:v1:cognition:space:daily-...
			"userId":      "user-abc",         // bare -> v1:identity:user:user-abc
			"deliveredTo": "utt-bare",         // incoming -> NOT rewritten
			"displayName": "Jose",             // not a relationship field
		}
		err := engine.canonicalizeRelationshipFields(ctx, "v1:cognition:participant", payload)
		require.NoError(t, err)
		require.Equal(t, "v1:cognition:space:daily-2026-05-06", payload["spaceId"])
		require.Equal(t, "v1:identity:user:user-abc", payload["userId"])
		require.Equal(t, "utt-bare", payload["deliveredTo"])
		require.Equal(t, "Jose", payload["displayName"])
	})

	t.Run("already-canonical values pass through unchanged", func(t *testing.T) {
		payload := map[string]any{
			"spaceId": "v1:cognition:space:abc",
			"userId":  "v1:identity:user:user-xyz",
		}
		err := engine.canonicalizeRelationshipFields(ctx, "v1:cognition:participant", payload)
		require.NoError(t, err)
		require.Equal(t, "v1:cognition:space:abc", payload["spaceId"])
		require.Equal(t, "v1:identity:user:user-xyz", payload["userId"])
	})

	t.Run("missing fields are skipped (relationships optional unless @required)", func(t *testing.T) {
		payload := map[string]any{
			"displayName": "Sofia",
		}
		err := engine.canonicalizeRelationshipFields(ctx, "v1:cognition:participant", payload)
		require.NoError(t, err)
		_, hasSpace := payload["spaceId"]
		require.False(t, hasSpace)
	})

	t.Run("empty-string field skipped (optional null-equivalent)", func(t *testing.T) {
		payload := map[string]any{
			"spaceId": "",
			"userId":  "user-abc",
		}
		err := engine.canonicalizeRelationshipFields(ctx, "v1:cognition:participant", payload)
		require.NoError(t, err)
		require.Equal(t, "", payload["spaceId"])
		require.Equal(t, "v1:identity:user:user-abc", payload["userId"])
	})

	t.Run("array-of-foreign-keys field canonicalizes each entry", func(t *testing.T) {
		// For concepts that store relationship arrays (e.g. groupIds).
		// Reuses a synthetic concept to exercise the slice path.
		eng := newTestEngineWithConcepts(t, map[string]*memoryNodes.Concept{
			"v1:identity:user": {Name: "v1:identity:user"},
			"v1:identity:group": {
				Name: "v1:identity:group",
				Relationships: []memoryNodes.RelationshipDefinition{
					{Type: "members", Field: "memberIds", TargetConcept: "v1:identity:user", Direction: "outgoing"},
				},
			},
		})
		payload := map[string]any{
			"memberIds": []any{"user-a", "v1:identity:user:user-b", "user-c"},
		}
		err := eng.canonicalizeRelationshipFields(ctx, "v1:identity:group", payload)
		require.NoError(t, err)
		got := payload["memberIds"].([]any)
		require.Equal(t, "v1:identity:user:user-a", got[0])
		require.Equal(t, "v1:identity:user:user-b", got[1])
		require.Equal(t, "v1:identity:user:user-c", got[2])
	})
}
