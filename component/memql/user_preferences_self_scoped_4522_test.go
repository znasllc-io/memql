package memql

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// user_preferences_self_scoped_4522_test.go -- memql#4522.
//
// updateMyPreferences is the portal's write for v1:identity:user.preferences.
// Two keys of that block must be UNREACHABLE through it:
//
//	computerUseEnabled  the computer-use kill switch. toggleComputerUseEnabled
//	                    owns it, deliberately with no admin override
//	                    (memql#2840), and every worker dispatch consults it.
//	activeAssistantId   an app-managed pointer (memql#406).
//
// The protection is structural rather than inspected: neither key is named in
// the mutation template, and @mergeFields("preferences") makes the write a deep
// merge, so a stored key the call does not carry survives. These tests are what
// keep it structural -- delete the annotation and the write starts REPLACING
// preferences wholesale, an engaged kill switch comes back ABSENT, and the
// @default("true") that is never applied on write reads as enabled.

// updateMyPreferencesFn loads the shipped mutation out of the real DSL tree.
func updateMyPreferencesFn(t *testing.T) *Function {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	conceptRegistry := concept.DefaultRegistry()
	require.NotNil(t, conceptRegistry)

	fnRegistry := newFunctionRegistry()
	_, _, err := LoadUnifiedFunctions(nil, fnRegistry, conceptRegistry)
	require.NoError(t, err)

	fn, err := fnRegistry.Get("updateMyPreferences")
	require.NoError(t, err, "updateMyPreferences must load from dsl/identity/mutations.memql")
	require.NotNil(t, fn.MutationTemplate)
	return fn
}

// TestUpdateMyPreferences_CarriesMergeFields is the annotation pin. It is the
// single line the whole protected-key guarantee rests on, and removing it
// leaves every happy-path assertion in this file still green.
func TestUpdateMyPreferences_CarriesMergeFields(t *testing.T) {
	fn := updateMyPreferencesFn(t)
	require.Equal(t, []string{"preferences"}, fn.MutationTemplate.MergeFields,
		"updateMyPreferences must deep-merge preferences: without it the write REPLACES the "+
			"whole block, so an engaged computerUseEnabled:false comes back absent and reads "+
			"as the @default(\"true\") that is never applied on write (memql#4522 / #3617)")
}

// TestUpdateMyPreferences_TargetsTheCallerOnly asserts the row is the actor's
// and that a stale client supplying somebody else's id cannot retarget it.
// Undeclared args are dropped leniently on the engine path, so the supplied
// ids below are exactly what a retargeting attempt looks like.
func TestUpdateMyPreferences_TargetsTheCallerOnly(t *testing.T) {
	fn := updateMyPreferencesFn(t)

	eng := &MemQLEngine{}
	const callerID = "user-caller-4522"
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: callerID,
		Role:   auth.RoleWriter,
	})

	node, err := eng.renderMutationTemplate(ctx, fn.MutationTemplate, map[string]any{
		"theme":  "dark",
		"userId": "user-someone-else",
		"id":     "user-someone-else",
	})
	require.NoError(t, err, "an empty id here would mean the mutation is inert, not that it is safe")
	require.Equal(t, callerID, node.ID,
		"updateMyPreferences must target the CALLER's row; %q would mean a supplied id retargets it", node.ID)
}

// With no actor the target must come back EMPTY, so the update fails loudly
// ("update() requires an explicit id") rather than selecting some row.
func TestUpdateMyPreferences_NoActorYieldsNoTarget(t *testing.T) {
	fn := updateMyPreferencesFn(t)

	eng := &MemQLEngine{}
	node, err := eng.renderMutationTemplate(context.Background(), fn.MutationTemplate, map[string]any{
		"theme":  "dark",
		"userId": "user-someone-else",
	})
	if err == nil {
		require.Empty(t, node.ID,
			"with no actor the target must be empty (fail-closed); %q means the write selected a row for a caller that has no identity", node.ID)
	}
}

// TestUpdateMyPreferences_ProtectedKeysAreUnspellable is the STRUCTURAL pin,
// and it runs with no database: whatever a caller sends, the rendered payload
// must not carry either protected key. They are not filtered out -- they are
// not fields of this template, so there is nothing for a caller to reach.
func TestUpdateMyPreferences_ProtectedKeysAreUnspellable(t *testing.T) {
	fn := updateMyPreferencesFn(t)

	eng := &MemQLEngine{}
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "user-caller-4522",
		Role:   auth.RoleWriter,
	})

	node, err := eng.renderMutationTemplate(ctx, fn.MutationTemplate, map[string]any{
		"theme":              "dark",
		"computerUseEnabled": true,
		"activeAssistantId":  "v1:agents:agent:attacker",
		// The nested spelling a caller might reach for next.
		"preferences": map[string]any{"computerUseEnabled": true},
	})
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(node.PayloadRaw), &payload))
	prefs, ok := payload["preferences"].(map[string]any)
	require.True(t, ok, "the write must carry a preferences object")

	require.NotContains(t, prefs, "computerUseEnabled",
		"updateMyPreferences must not be able to write the computer-use kill switch -- "+
			"toggleComputerUseEnabled owns it (memql#2840)")
	require.NotContains(t, prefs, "activeAssistantId",
		"updateMyPreferences must not be able to write the app-managed assistant pointer (memql#406)")
	require.Equal(t, "dark", prefs["theme"], "the declared field must still be written")
}

// TestUpdateMyPreferences_DeclaresNoProtectedArg is the same claim read off the
// args schema, so a future author who adds the field as an ARGUMENT trips a
// test naming the reason rather than discovering it through the render.
func TestUpdateMyPreferences_DeclaresNoProtectedArg(t *testing.T) {
	fn := updateMyPreferencesFn(t)
	require.NotNil(t, fn.ArgsSchema)
	for _, f := range fn.ArgsSchema.Fields {
		if f == nil {
			continue
		}
		require.NotEqual(t, "computerUseEnabled", f.Name,
			"the kill switch is toggleComputerUseEnabled's, deliberately with no second door (memql#2840)")
		require.NotEqual(t, "activeAssistantId", f.Name,
			"the assistant pointer is app-managed (memql#406) and is not a user-facing setting")
		require.NotEqual(t, "userId", f.Name,
			"a caller-supplied target would defeat the self-scoping; the row comes from actor.userId")
	}
}

// TestUpdateMyPreferences_NumericBoundsAreDeclared pins the two ranges the
// concept documents onto the args schema. They are @minimum/@maximum rather
// than an @enum because valueInEnum compares with reflect.DeepEqual and a
// numeric member would never equal the float64 a JSON caller sends.
func TestUpdateMyPreferences_NumericBoundsAreDeclared(t *testing.T) {
	fn := updateMyPreferencesFn(t)
	require.NotNil(t, fn.ArgsSchema)

	bounds := map[string][2]float64{
		"cursorTweenMs":        {250, 2500},
		"archiveRetentionDays": {30, 60},
	}
	seen := map[string]bool{}
	for _, f := range fn.ArgsSchema.Fields {
		if f == nil {
			continue
		}
		want, ok := bounds[f.Name]
		if !ok {
			continue
		}
		seen[f.Name] = true
		require.NotNil(t, f.Minimum, "%s must declare @minimum", f.Name)
		require.NotNil(t, f.Maximum, "%s must declare @maximum", f.Name)
		require.Equal(t, want[0], *f.Minimum, "%s minimum", f.Name)
		require.Equal(t, want[1], *f.Maximum, "%s maximum", f.Name)
	}
	for name := range bounds {
		require.True(t, seen[name], "%s must be a declared arg", name)
	}
}
