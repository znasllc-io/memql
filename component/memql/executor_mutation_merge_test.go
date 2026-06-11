package memql

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// These tests pin the memql#1339 fix: update() does TOP-LEVEL field
// replacement (mergePayloadFields with no merge list), so a mutation
// that writes `preferences: { computerUseEnabled: ... }` replaces the
// ENTIRE stored preferences object -- wiping theme, timezone,
// archiveRetentionDays, dailySpaceEnabled, voice mode, and the
// CoPresent Control settings on every kill-switch flip. The
// @mergeFields("preferences") annotation opts that one field into an
// engine-side deep-merge so sibling keys survive.

// TestMergePayloadFields_DefaultTopLevelReplaceWipesNestedKeys is the
// reproduction: it documents (and pins) the default update() contract
// that caused the bug -- WITHOUT a merge list, a partial nested object
// replaces the stored object wholesale. If this test ever fails, the
// global update() contract changed and every mutation written against
// top-level-replace semantics (e.g. memql#350's restate-every-field
// mutations) must be re-audited.
func TestMergePayloadFields_DefaultTopLevelReplaceWipesNestedKeys(t *testing.T) {
	prior := map[string]any{
		"displayName": "Jose",
		"preferences": map[string]any{
			"theme":                "dark",
			"timezone":             "Europe/Madrid",
			"archiveRetentionDays": float64(60),
			"computerUseEnabled":   true,
		},
	}
	partial := map[string]any{
		"preferences": map[string]any{
			"computerUseEnabled": false,
		},
	}

	mergePayloadFields(prior, partial, nil)

	prefs, ok := prior["preferences"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, false, prefs["computerUseEnabled"])
	// The wipe: every sibling key is gone under the default contract.
	require.NotContains(t, prefs, "theme",
		"default update() semantics changed from top-level replace; re-audit memql#350-style mutations")
	require.NotContains(t, prefs, "timezone")
	require.NotContains(t, prefs, "archiveRetentionDays")
	// Unrelated top-level fields inherit from the prior row as before.
	require.Equal(t, "Jose", prior["displayName"])
}

// TestMergePayloadFields_MergeFieldPreservesSiblingKeys is the fix:
// with "preferences" in the merge list, a single-key write merges into
// the stored object and every sibling preference survives.
func TestMergePayloadFields_MergeFieldPreservesSiblingKeys(t *testing.T) {
	prior := map[string]any{
		"displayName": "Jose",
		"role":        "owner",
		"preferences": map[string]any{
			"theme":                "dark",
			"timezone":             "Europe/Madrid",
			"archiveRetentionDays": float64(60),
			"dailySpaceEnabled":    true,
			"cursorTweenMs":        float64(250),
			"takeoverMode":         "dim",
			"computerUseEnabled":   true,
		},
	}
	partial := map[string]any{
		"preferences": map[string]any{
			"computerUseEnabled": false,
		},
	}

	mergePayloadFields(prior, partial, []string{"preferences"})

	prefs, ok := prior["preferences"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, false, prefs["computerUseEnabled"], "the written key must win")
	require.Equal(t, "dark", prefs["theme"])
	require.Equal(t, "Europe/Madrid", prefs["timezone"])
	require.Equal(t, float64(60), prefs["archiveRetentionDays"])
	require.Equal(t, true, prefs["dailySpaceEnabled"])
	require.Equal(t, float64(250), prefs["cursorTweenMs"])
	require.Equal(t, "dim", prefs["takeoverMode"])
	require.Equal(t, "owner", prior["role"], "untouched top-level fields inherit from the prior row")
}

// TestMergePayloadFields_MergeFieldNonObjectFallsBackToReplace pins
// the type-mismatch edges: when the prior value is absent / null /
// scalar, or the partial value is not an object, the merge-listed
// field falls back to plain replacement instead of erroring.
func TestMergePayloadFields_MergeFieldNonObjectFallsBackToReplace(t *testing.T) {
	// Prior value absent: partial object lands as-is.
	prior := map[string]any{"displayName": "Jose"}
	mergePayloadFields(prior, map[string]any{
		"preferences": map[string]any{"computerUseEnabled": false},
	}, []string{"preferences"})
	prefs, ok := prior["preferences"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, false, prefs["computerUseEnabled"])

	// Prior value scalar (legacy bad data): replaced, no panic.
	prior = map[string]any{"preferences": "corrupt"}
	mergePayloadFields(prior, map[string]any{
		"preferences": map[string]any{"theme": "light"},
	}, []string{"preferences"})
	prefs, ok = prior["preferences"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "light", prefs["theme"])

	// Partial value scalar: replaces the stored object (explicit
	// caller intent to overwrite the field wins).
	prior = map[string]any{"preferences": map[string]any{"theme": "dark"}}
	mergePayloadFields(prior, map[string]any{"preferences": nil}, []string{"preferences"})
	require.Nil(t, prior["preferences"])
}

// TestDeepMergeObjects_RecursiveAndNonMutating pins the recursive
// merge used for merge-listed fields: nested sub-objects merge level
// by level, the partial wins on scalar collision, and neither input
// map is mutated.
func TestDeepMergeObjects_RecursiveAndNonMutating(t *testing.T) {
	prior := map[string]any{
		"theme": "dark",
		"voice": map[string]any{"mode": "cascade", "rate": float64(1)},
	}
	partial := map[string]any{
		"voice": map[string]any{"mode": "realtime"},
	}

	out := deepMergeObjects(prior, partial)

	voice, ok := out["voice"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "realtime", voice["mode"])
	require.Equal(t, float64(1), voice["rate"], "sibling sub-key must survive the nested merge")
	require.Equal(t, "dark", out["theme"])

	// Inputs untouched.
	require.Equal(t, "cascade", prior["voice"].(map[string]any)["mode"])
	require.NotContains(t, partial["voice"].(map[string]any), "rate")
}

// TestMutationTemplate_MergeFieldsAnnotationPlumbing parses an inline
// @mergeFields mutation through the same loader path the unified tree
// uses and asserts the annotation lands on the rendered MutationNode
// (the value executeUpdate consults).
func TestMutationTemplate_MergeFieldsAnnotationPlumbing(t *testing.T) {
	registry := newMemoryRegistry(map[string]*concept.Concept{
		"v1:identity:user": {Name: "v1:identity:user"},
	})

	src := `@enabled
@mergeFields("preferences")
@description("Set User.preferences.computerUseEnabled.")
mutation user mutationToggleComputerUseEnabled {
  args {
    userId   string  @required
    enabled  bool    @required
  }
  update {
    id: args.userId
    preferences: {
        computerUseEnabled: args.enabled
      }
  }
}`

	fn, err := tryParseNewFunctionSyntax("mutationToggleComputerUseEnabled", "mutation", src, "test.memql", registry)
	require.NoError(t, err)
	require.NotNil(t, fn.MutationTemplate)
	require.Equal(t, []string{"preferences"}, fn.MutationTemplate.MergeFields)

	engine := &MemQLEngine{}
	mutation, err := engine.renderMutationTemplate(context.Background(), fn.MutationTemplate, map[string]any{
		"userId":  "user-001",
		"enabled": false,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"preferences"}, mutation.MergeFields)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(mutation.PayloadRaw), &payload))
	prefs, ok := payload["preferences"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, false, prefs["computerUseEnabled"])
}

// TestMutationTemplate_MergeFieldsRejectedOnInsert pins the load-time
// gate: @mergeFields on an insert-kind mutation is a hard error (an
// insert writes the full payload; a silent no-op would hide an
// authoring mistake).
func TestMutationTemplate_MergeFieldsRejectedOnInsert(t *testing.T) {
	registry := newMemoryRegistry(map[string]*concept.Concept{
		"v1:identity:user": {Name: "v1:identity:user"},
	})

	src := `@enabled
@mergeFields("preferences")
mutation user mutationCreateUserBad {
  args {
    userId  string  @required
  }
  insert {
    id: args.userId
    preferences: {}
  }
}`

	_, err := tryParseNewFunctionSyntax("mutationCreateUserBad", "mutation", src, "test.memql", registry)
	require.Error(t, err)
	require.Contains(t, err.Error(), "@mergeFields is only valid on update mutations")
}

// TestToggleComputerUseEnabled_DSLCarriesMergeFields is the memql#1339
// regression test against the REAL DSL tree: the shipped
// mutationToggleComputerUseEnabled (dsl/worker/mutations.memql) must
// carry @mergeFields("preferences"). Without it, flipping the
// computer-use kill switch wipes every other User.preferences key
// (theme, timezone, archiveRetentionDays, dailySpaceEnabled, voice
// mode, CoPresent Control settings).
func TestToggleComputerUseEnabled_DSLCarriesMergeFields(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	conceptRegistry := concept.DefaultRegistry()
	require.NotNil(t, conceptRegistry)

	fnRegistry := newFunctionRegistry()
	_, _, err := LoadUnifiedFunctions(nil, fnRegistry, conceptRegistry)
	require.NoError(t, err)

	fn, err := fnRegistry.Get("mutationToggleComputerUseEnabled")
	require.NoError(t, err, "mutationToggleComputerUseEnabled must load from dsl/worker/mutations.memql")
	require.NotNil(t, fn.MutationTemplate)
	require.Equal(t, []string{"preferences"}, fn.MutationTemplate.MergeFields,
		"mutationToggleComputerUseEnabled must deep-merge preferences or the kill switch wipes every other preference key (memql#1339)")
}
