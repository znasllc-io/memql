package memql

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// mutation_nested_object_merge_3617_test.go -- memql#3617.
//
// A mutation that REBUILDS a nested object from optional args destroys every
// leaf it was not passed. An absent optional arg omits its key from the nested
// object (evalValue's map branch drops missingValue{}); the object is a
// TOP-LEVEL payload field, so the read-merge replaces it WHOLESALE unless the
// mutation names it in @mergeFields. The un-passed leaves are then gone from
// durable storage with no error at any layer.
//
// recordPasskeyAssertion is the live instance: five of its ten args are
// optional, and a minimal legal call drops `backupEligible` -- which reads back
// false, so a synced authenticator asserting true is refused. That is
// memql#3605 reached by a different route.
//
// Both in-tree Go callers pass every arg, so this was LATENT. What was broken
// is the contract: nothing in the args schema, the loader, or the write path
// prevented the destroying call.
//
// The two remedies are different because the two mutations are different kinds:
//
//   - recordPasskeyAssertion is an UPDATE, so @mergeFields("credentials")
//     applies and makes the write merge-semantic -- the un-passed leaves
//     survive instead of every caller having to restate immutable fields.
//   - createPasskeyIdentity is an INSERT, where @mergeFields is a load-time
//     error, so the destroying call is made unspellable instead: the five args
//     are @required. A registration ceremony always has all ten values.

// passkeyOptionalLeafArgs are the five args that were optional and are the
// leaves a minimal call dropped.
var passkeyOptionalLeafArgs = []string{"signCount", "aaguid", "transports", "backupEligible", "backupState"}

// TestRecordPasskeyAssertion_MergesCredentials is the reproduction. It renders
// the mutation with exactly the @required set and then runs the engine's own
// read-merge, which is where the leaves die.
func TestRecordPasskeyAssertion_MergesCredentials(t *testing.T) {
	reg := load1633Functions(t)

	fn, err := reg.Get("recordPasskeyAssertion")
	require.NoError(t, err)
	require.NotNil(t, fn.MutationTemplate)

	// The stored row, as the register ceremony wrote it.
	stored := map[string]any{
		"credentials": map[string]any{
			"credentialId":   "cred-1",
			"publicKey":      "pk-1",
			"signCount":      float64(0),
			"aaguid":         "aaguid-1",
			"transports":     []any{"internal", "hybrid"},
			"backupEligible": true,
			"backupState":    true,
			"registeredBy":   "webauthn",
			"lastUsedAt":     "",
		},
	}

	// A minimal LEGAL call: every @required arg, no optional one.
	delta := renderPayload(t, reg, "recordPasskeyAssertion", map[string]any{
		"identityId":   "id-1",
		"credentialId": "cred-1",
		"publicKey":    "pk-1",
		"registeredBy": "webauthn",
		"lastUsedAt":   "2026-08-12T00:00:00Z",
	})

	// The delta really is missing the leaves -- this is the mechanism, not the
	// defect, and it stays true after the fix.
	deltaCreds, ok := delta["credentials"].(map[string]any)
	require.True(t, ok, "credentials must render as an object")
	for _, leaf := range passkeyOptionalLeafArgs {
		require.NotContains(t, deltaCreds, leaf,
			"an absent optional arg omits its key from the nested object (evalValue map branch)")
	}

	// Now the engine's own read-merge, with the mutation's declared merge set.
	mergePayloadFields(stored, delta, fn.MutationTemplate.MergeFields)
	merged, ok := stored["credentials"].(map[string]any)
	require.True(t, ok)

	require.Equal(t, true, merged["backupEligible"],
		"backupEligible must survive a call that did not pass it. Without "+
			`@mergeFields("credentials") the top-level object is replaced wholesale and the `+
			"leaf is GONE -- it then reads back false, a synced authenticator asserts true, "+
			"and WebAuthn refuses. That is memql#3605 reached through memql#3617.")
	require.Equal(t, true, merged["backupState"], "backupState must survive an unpassed call")
	require.Equal(t, "aaguid-1", merged["aaguid"], "aaguid must survive an unpassed call")
	require.Equal(t, []any{"internal", "hybrid"}, merged["transports"],
		"transports must survive an unpassed call")

	// The three fields the spec says move must still move.
	require.Equal(t, "2026-08-12T00:00:00Z", merged["lastUsedAt"],
		"lastUsedAt is what the mutation exists to write; the merge must not block it")
}

// TestRecordPasskeyAssertion_DeclaresCredentialsMerge pins the annotation
// itself, so removing it fails here as well as through the behaviour above.
func TestRecordPasskeyAssertion_DeclaresCredentialsMerge(t *testing.T) {
	reg := load1633Functions(t)
	fn, err := reg.Get("recordPasskeyAssertion")
	require.NoError(t, err)
	require.NotNil(t, fn.MutationTemplate)
	require.Contains(t, fn.MutationTemplate.MergeFields, "credentials",
		`recordPasskeyAssertion must declare @mergeFields("credentials") -- it rebuilds a `+
			"nested object out of optional args (memql#3617)")
}

// TestCreatePasskeyIdentity_LeafArgsAreRequired is the insert-kind half.
// @mergeFields is a load-time error on an insert, so the remedy is to make the
// destroying call unspellable.
func TestCreatePasskeyIdentity_LeafArgsAreRequired(t *testing.T) {
	reg := load1633Functions(t)
	fn, err := reg.Get("createPasskeyIdentity")
	require.NoError(t, err)
	require.NotNil(t, fn.ArgsSchema)

	byName := map[string]*FunctionArgsField{}
	for _, f := range fn.ArgsSchema.Fields {
		byName[f.Name] = f
	}
	for _, name := range passkeyOptionalLeafArgs {
		f, ok := byName[name]
		require.Truef(t, ok, "createPasskeyIdentity must declare arg %q", name)
		require.Falsef(t, f.Optional,
			"createPasskeyIdentity arg %q must be @required: it is a LEAF of the rebuilt "+
				"`credentials` object, and an insert cannot carry @mergeFields, so an optional "+
				"leaf is a spelling that silently drops it (memql#3617)", name)
	}
}

// knownNestedObjectRebuilds is the ACCEPTED inventory of the memql#3617 shape:
// mutations the detector flags where a human has looked and judged wholesale
// replace to be what the field means.
//
// sendRealtimeTranscriptUtterance's `source` is the one entry. Six of its eight
// leaves are `args.source.<leaf>` -- it is projecting a caller-supplied
// provenance blob with defaults, which is the category the memql#3617 sweep
// classified as "wholesale replace is correct" (16 mutations in the tree do the
// same thing via a bare `args.source` splat and are not flagged at all, because
// a splat is not an object literal). The single bare `args.idempotencyKey` leaf
// rides along with the blob rather than being an independently-owned field, and
// a second write that supplies a different `source` is MEANT to replace the
// provenance rather than merge into it.
var knownNestedObjectRebuilds = map[string][]string{
	"sendRealtimeTranscriptUtterance": {"source"},
}

// TestNestedObjectFromOptionalArgs_InventoryIsPinned enforces the general shape
// across the whole loaded tree. A NEW mutation that rebuilds a nested object
// from optional args without merge semantics fails here.
//
// Detectable from the template alone: a top-level payload field whose template
// is an object literal with MORE THAN ONE leaf, at least one of them a bare
// single-segment reference to an OPTIONAL arg, and the field not named in
// @mergeFields. A single-leaf object has no sibling to lose; `args.x ?? "d"`
// always produces a value so it cannot drop a key; and a splat
// (`credentials: args.credentials`) is not an object literal at all.
//
// This is a pinned inventory rather than a bare emptiness assertion because the
// detector is advisory at load (a warning, not a boot refusal -- see
// validateNestedObjectMergeSemantics): the remedy needs an author's judgement
// per case, and refusing boot would turn a latent hazard in a downstream
// product bundle into an outage.
func TestNestedObjectFromOptionalArgs_InventoryIsPinned(t *testing.T) {
	reg := load1633Functions(t)

	got := map[string][]string{}
	for _, fn := range reg.List() {
		if fn == nil || fn.MutationTemplate == nil {
			continue
		}
		if fields := destructiveNestedObjectFields(fn); len(fields) > 0 {
			got[fn.Name] = fields
		}
	}

	var unexpected []string
	for name, fields := range got {
		known, listed := knownNestedObjectRebuilds[name]
		if !listed {
			unexpected = append(unexpected, name+": "+strings.Join(fields, ", "))
			continue
		}
		// An accepted entry covers the fields it names, not the mutation. A
		// second nested object appearing on a listed mutation is a new
		// instance and gets looked at like any other.
		for _, field := range fields {
			if !slices.Contains(known, field) {
				unexpected = append(unexpected, name+": "+field)
			}
		}
	}
	sort.Strings(unexpected)
	require.Emptyf(t, unexpected,
		"mutation(s) rebuild a nested object from optional args without merge semantics "+
			"(memql#3617):\n  %s\n\nAn absent optional arg omits its leaf, and the top-level "+
			"field is then written wholesale, so every stored leaf the caller did not pass is "+
			`destroyed with no error. Fix: name the field in @mergeFields("...") on an update, `+
			"or mark the leaf args @required. If wholesale replace really is what the field "+
			"means, add it to knownNestedObjectRebuilds with the reason.",
		strings.Join(unexpected, "\n  "))

	// The other direction: an accepted entry that stops being flagged has been
	// fixed, and leaving it listed hides the next one.
	for name := range knownNestedObjectRebuilds {
		require.Containsf(t, got, name,
			"%q is listed in knownNestedObjectRebuilds but no longer trips the detector -- "+
				"drop the entry", name)
	}
}
