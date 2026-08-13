package memql

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// mint_action_required_fields_3619_test.go -- memql#3619.
//
// `mintAction` could never validate, so the whole action-minting path was dead.
//
// v1:actions:action declares kind / reliability / reinforceCount / version as
// @required WITH @default(...). Per memql#2960 a concept @default is never
// applied on insert, so a default does NOT satisfy a required field. mintAction
// wrote none of the four -- not in args, not in accept {}, not in stamp {} --
// while it DID use `??` for the two neighbours (sideEffectClass, status). The
// workaround was applied to two of six.
//
// Every call therefore failed JSON-schema validation. mintActionFromRun
// (app/integrations_harness_replay.go) is best-effort, so it failed into
// `a.Logger.Warn("mint action failed")` on every step and the replay library
// never got a row: loud in the log, invisible in behaviour.
//
// The fix supplies the four in the mutation body with `??` defaults matching
// the concept's declared defaults -- exactly what the author already did for
// the other two.

// TestMintAction_SuppliesEveryRequiredConceptField is the reproduction. It
// renders the most complete call possible (every declared arg supplied) and
// checks the payload against the concept's own required set.
func TestMintAction_SuppliesEveryRequiredConceptField(t *testing.T) {
	reg := load1633Functions(t)

	payload := renderPayload(t, reg, "mintAction", map[string]any{
		"actionId":            "act-1",
		"slug":                "shell.exec",
		"intent":              "run the build",
		"capability":          "shell.exec",
		"sideEffectClass":     "exec",
		"status":              "candidate",
		"inputFingerprint":    "fp-in",
		"calls":               []any{map[string]any{"index": int64(0)}},
		"resourceEdges":       []any{},
		"paramBindings":       []any{},
		"templateFingerprint": "fp-tpl",
		"recordedResult":      map[string]any{"ok": true},
		"resultFingerprint":   "fp-res",
		"recordedSurface":     "workbench",
		"provenancePlanId":    "plan-1",
		"provenanceStepId":    "step-1",
	})

	action, err := concept.DefaultRegistry().Get("v1:actions:action")
	require.NoError(t, err, "v1:actions:action must be registered")

	var missing []string
	for _, field := range action.RequiredFields() {
		if _, present := payload[field]; !present {
			missing = append(missing, field)
		}
	}
	sort.Strings(missing)
	require.Emptyf(t, missing,
		"mintAction leaves @required concept field(s) %v unset, so every call fails schema "+
			"validation and the action library never gets a row (memql#3619). A concept "+
			"@default does NOT fill them -- it is never applied on insert (memql#2960) -- so "+
			"the mutation body has to supply them with `??`.", missing)
}

// TestMintAction_DefaultsMatchTheConceptDeclaration pins the VALUES, not just
// presence. A stamped value that drifts from the concept's @default would make
// the two readings of "the default" disagree, which is how the field ended up
// unwritten in the first place.
func TestMintAction_DefaultsMatchTheConceptDeclaration(t *testing.T) {
	reg := load1633Functions(t)

	// Nothing optional supplied: every `??` arm takes its default and every
	// constant stamp stands alone.
	payload := renderPayload(t, reg, "mintAction", map[string]any{
		"slug":             "shell.exec",
		"intent":           "run the build",
		"inputFingerprint": "fp-in",
		"calls":            []any{},
	})

	// EqualValues, not Equal: renderPayload round-trips through JSON, so every
	// number comes back float64 regardless of how the template typed it. The
	// integer-ness that the concept schema cares about is a property of the
	// MARSHALLED payload, which TestMintAction_RendersIntegersAsIntegers pins.
	for field, want := range map[string]any{
		"kind":            "primitive",
		"reliability":     0.5,
		"reinforceCount":  0,
		"version":         1,
		"sideEffectClass": "write",
		"status":          "active",
	} {
		require.EqualValuesf(t, want, payload[field],
			"mintAction's default for %q must match the concept's @default (memql#3619)", field)
	}
}

// TestMintAction_RendersIntegersAsIntegers guards the half the JSON round-trip
// hides: `reinforceCount` and `version` are int-typed on the concept, so a
// default that rendered as 0.0 / 1.0 would satisfy the required check above and
// still fail schema validation on type.
func TestMintAction_RendersIntegersAsIntegers(t *testing.T) {
	reg := load1633Functions(t)
	fn, err := reg.Get("mintAction")
	require.NoError(t, err)

	e := &MemQLEngine{}
	node, err := e.renderMutationTemplate(context.Background(), fn.MutationTemplate, map[string]any{
		"slug":             "shell.exec",
		"intent":           "run the build",
		"inputFingerprint": "fp-in",
		"calls":            []any{},
	})
	require.NoError(t, err)

	require.Contains(t, node.PayloadRaw, `"reinforceCount":0`,
		"reinforceCount is int-typed on the concept; a 0.0 default fails schema validation")
	require.Contains(t, node.PayloadRaw, `"version":1`,
		"version is int-typed on the concept; a 1.0 default fails schema validation")
}

// TestMintAction_TrustGateValuesSurvive keeps the two `??` arms from collapsing
// into constants: mintActionFromRun computes sideEffectClass and status from
// the surface-aware trust gate, and a default that overrode them would silently
// auto-promote every real-machine write.
func TestMintAction_TrustGateValuesSurvive(t *testing.T) {
	reg := load1633Functions(t)

	payload := renderPayload(t, reg, "mintAction", map[string]any{
		"slug":             "shell.exec",
		"intent":           "run the build",
		"inputFingerprint": "fp-in",
		"calls":            []any{},
		"sideEffectClass":  "exec",
		"status":           "candidate",
	})

	require.Equal(t, "exec", payload["sideEffectClass"])
	require.Equal(t, "candidate", payload["status"],
		"the trust gate's status must survive; a `??` that swallowed it would auto-promote "+
			"every real-machine write")
}

// TestMintAction_TrustLadderFieldsAreNotCallerSettable is the reason the four
// new stamps are constants rather than `??` arms. A mint that could be handed
// reliability 1.0 would enter the library already trusted, skipping the
// fingerprint-verified replay that is supposed to earn that number; version and
// reinforceCount are the same story one step down. reinforceAction is the only
// path that may move them, and it computes the values engine-side.
func TestMintAction_TrustLadderFieldsAreNotCallerSettable(t *testing.T) {
	reg := load1633Functions(t)
	fn, err := reg.Get("mintAction")
	require.NoError(t, err)
	require.NotNil(t, fn.ArgsSchema)

	declared := map[string]struct{}{}
	for _, f := range fn.ArgsSchema.Fields {
		declared[f.Name] = struct{}{}
	}
	for _, name := range []string{"kind", "reliability", "reinforceCount", "version"} {
		require.NotContainsf(t, declared, name,
			"mintAction must not accept %q as an arg: a fresh mint is a primitive at the "+
				"starting confidence with no verified replays at version 1, by definition, "+
				"and a caller-settable reliability would walk straight past the trust ladder",
			name)
	}
}
