package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// nestedRelationshipEngine builds an engine whose concept declares relationships
// on fields nested inside object blocks -- the shape four shipped declarations
// use, and the shape that has never worked.
func nestedRelationshipEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	return newTestEngineWithConcepts(t, map[string]*memoryNodes.Concept{
		"v1:identity:identity": {Name: "v1:identity:identity"},
		"v1:planner:plan":      {Name: "v1:planner:plan"},
		"v1:agents:agent": {
			Name: "v1:agents:agent",
			Relationships: []memoryNodes.RelationshipDefinition{
				{Type: "interactsWith", Field: "identity.identityId", TargetConcept: "v1:identity:identity", Direction: "outgoing"},
				{Type: "createdBy", Field: "lineage.originatingPlanId", TargetConcept: "v1:planner:plan", Direction: "outgoing"},
				{Type: "interactsWith", Field: "lineage.sourcePlanIds", TargetConcept: "v1:planner:plan", Direction: "outgoing"},
			},
		},
	})
}

// TestCanonicalizeNestedRelationshipField pins memql#3672 on the WRITE path.
//
// Both canonicalizers did a flat `payload[field]` lookup with no path walking,
// so a relationship on a field inside an object block resolved to nothing --
// neither by its leaf name nor by a dotted path, because nothing split on ".".
// The relationship passed every gate and then did nothing, leaving the ids it
// was meant to canonicalize stored in bare form.
func TestCanonicalizeNestedRelationshipField(t *testing.T) {
	engine := nestedRelationshipEngine(t)
	ctx := context.Background()

	t.Run("nested scalar canonicalizes", func(t *testing.T) {
		payload := map[string]any{
			"identity": map[string]any{
				"identityId":      "id-abc",
				"identitySubject": "sub-abc", // not a relationship field
			},
			"name": "Faye",
		}
		require.NoError(t, engine.canonicalizeRelationshipFields(ctx, "v1:agents:agent", payload))

		identity := payload["identity"].(map[string]any)
		require.Equal(t, "v1:identity:identity:id-abc", identity["identityId"])
		require.Equal(t, "sub-abc", identity["identitySubject"], "a sibling key must not be touched")
		require.Equal(t, "Faye", payload["name"])
	})

	t.Run("nested array canonicalizes every entry", func(t *testing.T) {
		payload := map[string]any{
			"lineage": map[string]any{
				"sourcePlanIds": []any{"plan-1", "plan-2"},
			},
		}
		require.NoError(t, engine.canonicalizeRelationshipFields(ctx, "v1:agents:agent", payload))

		got := payload["lineage"].(map[string]any)["sourcePlanIds"].([]any)
		require.Equal(t, []any{"v1:planner:plan:plan-1", "v1:planner:plan:plan-2"}, got)
	})

	t.Run("already-canonical nested value passes through", func(t *testing.T) {
		payload := map[string]any{
			"identity": map[string]any{"identityId": "v1:identity:identity:already"},
		}
		require.NoError(t, engine.canonicalizeRelationshipFields(ctx, "v1:agents:agent", payload))
		require.Equal(t, "v1:identity:identity:already", payload["identity"].(map[string]any)["identityId"])
	})

	t.Run("missing intermediate object is skipped, not created", func(t *testing.T) {
		payload := map[string]any{"name": "Sofia"}
		require.NoError(t, engine.canonicalizeRelationshipFields(ctx, "v1:agents:agent", payload))
		_, has := payload["identity"]
		require.False(t, has, "canonicalization must not materialise an absent object")
	})

	t.Run("missing leaf inside a present object is skipped", func(t *testing.T) {
		payload := map[string]any{
			"identity": map[string]any{"identitySubject": "sub-only"},
		}
		require.NoError(t, engine.canonicalizeRelationshipFields(ctx, "v1:agents:agent", payload))
		identity := payload["identity"].(map[string]any)
		_, has := identity["identityId"]
		require.False(t, has)
	})

	t.Run("empty nested value is skipped", func(t *testing.T) {
		payload := map[string]any{
			"identity": map[string]any{"identityId": ""},
		}
		require.NoError(t, engine.canonicalizeRelationshipFields(ctx, "v1:agents:agent", payload))
		require.Equal(t, "", payload["identity"].(map[string]any)["identityId"])
	})

	t.Run("a non-object at the path root is left alone", func(t *testing.T) {
		// Defensive: the schema says `identity` is an object, but a malformed
		// write must not panic the engine.
		payload := map[string]any{"identity": "not-an-object"}
		require.NoError(t, engine.canonicalizeRelationshipFields(ctx, "v1:agents:agent", payload))
		require.Equal(t, "not-an-object", payload["identity"])
	})
}

// TestCanonicalizeNestedRelationshipComparison pins the same fix on the READ
// path. An asymmetry here is the memql#3654 half-works defect in a new place:
// writes landing canonical while filters compare against bare ids would find
// nothing.
func TestCanonicalizeNestedRelationshipComparison(t *testing.T) {
	engine := nestedRelationshipEngine(t)
	ctx := context.Background()

	got, ok := engine.canonicalizeRelationshipFieldValue(ctx, "v1:agents:agent", "identity.identityId", "id-abc")
	require.True(t, ok, "a nested relationship field must canonicalize on the filter path")
	require.Equal(t, "v1:identity:identity:id-abc", got)
}

// TestShippedNestedRelationshipsNowFire is the acceptance check for memql#3672:
// the four real declarations that motivated it must actually canonicalize.
//
// It runs against the REAL embedded concepts rather than a fixture, because the
// bug was never in the mechanism in isolation -- it was that these specific
// declarations, written in good faith and passing every gate, did nothing.
func TestShippedNestedRelationshipsNowFire(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	engine := newTestEngineWithConcepts(t, memoryNodes.All())
	ctx := context.Background()

	t.Run("agent.identity.identityId", func(t *testing.T) {
		payload := map[string]any{"identity": map[string]any{"identityId": "ident-1"}}
		require.NoError(t, engine.canonicalizeRelationshipFields(ctx, "v1:agents:agent", payload))
		require.Equal(t, "v1:identity:identity:ident-1",
			payload["identity"].(map[string]any)["identityId"])
	})

	t.Run("agent.lineage.originatingPlanId and extendedFromAgentId", func(t *testing.T) {
		payload := map[string]any{"lineage": map[string]any{
			"originatingPlanId":   "plan-1",
			"extendedFromAgentId": "agent-1",
		}}
		require.NoError(t, engine.canonicalizeRelationshipFields(ctx, "v1:agents:agent", payload))
		lineage := payload["lineage"].(map[string]any)
		require.Equal(t, "v1:planner:plan:plan-1", lineage["originatingPlanId"])
		require.Equal(t, "v1:agents:agent:agent-1", lineage["extendedFromAgentId"])
	})

	t.Run("user.preferences.activeAssistantId", func(t *testing.T) {
		payload := map[string]any{"preferences": map[string]any{"activeAssistantId": "agent-2"}}
		require.NoError(t, engine.canonicalizeRelationshipFields(ctx, "v1:identity:user", payload))
		require.Equal(t, "v1:agents:agent:agent-2",
			payload["preferences"].(map[string]any)["activeAssistantId"])
	})
}
