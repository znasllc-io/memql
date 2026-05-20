package memql

import (
	"context"
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
		require.Equal(t, "default:v1:cognition:space:daily-9dc3b323-2026-05-06", got.Value)
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
		require.Equal(t, "default:v1:identity:user:user-abc", got.Value)
	})

	t.Run("already-canonical RHS is unchanged (no AST clone)", func(t *testing.T) {
		expr := &ComparisonExpression{
			Field:    FieldReference{Parts: []string{"payload", "spaceId"}, Raw: "payload.spaceId"},
			Operator: OpEq,
			Value:    "default:v1:cognition:space:daily-abc",
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
		require.Equal(t, "default:v1:cognition:space:daily-abc", got.Left.(*ComparisonExpression).Value)
		require.Equal(t, "default:v1:identity:user:user-xyz", got.Right.(*ComparisonExpression).Value)
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
		"v1:identity:user":    {Name: "v1:identity:user"},
		"v1:cognition:space":  {Name: "v1:cognition:space"}, // partition-scoped
		"v1:agents:agent":  {Name: "v1:agents:agent"}, // partition-scoped
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
			name:        "bare slug for global concept gets _system prefix",
			value:       "user-abc",
			conceptType: "v1:identity:user",
			want:        "default:v1:identity:user:user-abc",
		},
		{
			name:        "bare slug for partition concept gets default prefix",
			value:       "ga-xyz",
			conceptType: "v1:agents:agent",
			want:        "default:v1:agents:agent:ga-xyz",
		},
		{
			name:        "already-canonical value is returned as-is",
			value:       "default:v1:identity:user:user-abc",
			conceptType: "v1:identity:user",
			want:        "default:v1:identity:user:user-abc",
		},
		{
			name:        "canonical with stale partition is re-partitioned to global",
			value:       "default:v1:identity:user:user-abc",
			conceptType: "v1:identity:user",
			want:        "default:v1:identity:user:user-abc",
		},
		{
			name:        "canonical for wrong concept errors (caller passed wrong type tag)",
			value:       "default:v1:identity:user:user-abc",
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
			"spaceId":     "daily-2026-05-06",     // bare -> default:v1:cognition:space:daily-...
			"userId":      "user-abc",             // bare -> default:v1:identity:user:user-abc
			"deliveredTo": "utt-bare",             // incoming -> NOT rewritten
			"displayName": "Jose",                 // not a relationship field
		}
		err := engine.canonicalizeRelationshipFields(ctx, "v1:cognition:participant", payload)
		require.NoError(t, err)
		require.Equal(t, "default:v1:cognition:space:daily-2026-05-06", payload["spaceId"])
		require.Equal(t, "default:v1:identity:user:user-abc", payload["userId"])
		require.Equal(t, "utt-bare", payload["deliveredTo"])
		require.Equal(t, "Jose", payload["displayName"])
	})

	t.Run("already-canonical values pass through unchanged", func(t *testing.T) {
		payload := map[string]any{
			"spaceId": "default:v1:cognition:space:abc",
			"userId":  "default:v1:identity:user:user-xyz",
		}
		err := engine.canonicalizeRelationshipFields(ctx, "v1:cognition:participant", payload)
		require.NoError(t, err)
		require.Equal(t, "default:v1:cognition:space:abc", payload["spaceId"])
		require.Equal(t, "default:v1:identity:user:user-xyz", payload["userId"])
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
		require.Equal(t, "default:v1:identity:user:user-abc", payload["userId"])
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
			"memberIds": []any{"user-a", "default:v1:identity:user:user-b", "user-c"},
		}
		err := eng.canonicalizeRelationshipFields(ctx, "v1:identity:group", payload)
		require.NoError(t, err)
		got := payload["memberIds"].([]any)
		require.Equal(t, "default:v1:identity:user:user-a", got[0])
		require.Equal(t, "default:v1:identity:user:user-b", got[1])
		require.Equal(t, "default:v1:identity:user:user-c", got[2])
	})
}
