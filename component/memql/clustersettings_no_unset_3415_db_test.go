package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// clustersettings_no_unset_3415_db_test.go is the real-engine reproduction +
// fix guard for memql#3415: a write carrying an EMPTY bootstrappedAt landed on
// the singleton v1:identity:clusterSettings row and un-bootstrapped a live
// cluster -- `GET /` and `GET /login` started 302ing to `/setup`, and `/setup`
// (the wizard that mints the cluster owner) started answering 200 on a cluster
// that already had an owner and hundreds of users.
//
// WHY THE READ-MERGE MACHINERY (memql#1628 / #1709) DID NOT PROTECT THE FIELD.
// Read-merge inherits a stored value for every field ABSENT from the delta. It
// cannot help when the delta explicitly CARRIES the field. createClusterSettings
// is authored as `insert { ... bootstrappedAt: args.bootstrappedAt ?? "" ... }`,
// so an omitted arg is materialised by `??` into an explicit empty string --
// present in the delta, and therefore the winner of the merge. The field was
// never "omitted" from the engine's point of view; it was written blank.
//
// The fix is @noUnset("bootstrappedAt") on both clusterSettings mutations: on
// the read-merge path a named field whose incoming value is empty is dropped
// from the delta when the stored row holds a non-empty one, so
// bootstrapped -> un-bootstrapped stops being an expressible write.
//
// Postgres-gated, identical harness to executor_mutation_readmerge_db_test.go:
// skips when no DB is reachable.

// bootstrapStamp3415 is the "cluster is claimed" stamp these tests seed and
// then try (and must fail) to erase. Every test here writes a per-run unique
// id -- never the singleton id="cluster" a live cluster's identity surface
// reads. That isolation is half the fix (memql#3415).
const bootstrapStamp3415 = "2026-08-09T07:38:20Z"

// TestNoUnset3415_CreateClusterSettings_CannotBlankBootstrappedAt is the exact
// incident shape: a caller re-runs createClusterSettings without supplying
// bootstrappedAt. `?? ""` puts an explicit empty string in the delta, which
// before the fix won the read-merge and un-bootstrapped the cluster.
func TestNoUnset3415_CreateClusterSettings_CannotBlankBootstrappedAt(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	const conceptName = "v1:identity:clusterSettings"
	settingsId := "cs3415-" + uniqueSuffix("create")

	storedId := runMutation(t, ctx, eng, "createClusterSettings", map[string]any{
		"id":                  settingsId,
		"registrationMode":    "open",
		"internalDefaultRole": "reader",
		"bootstrappedAt":      bootstrapStamp3415,
	})
	before := latestPayload(t, ctx, db, conceptName, storedId)
	require.Equal(t, bootstrapStamp3415, before["bootstrappedAt"],
		"seed row must start out bootstrapped")

	// The clobber: same id, no bootstrappedAt arg.
	runMutation(t, ctx, eng, "createClusterSettings", map[string]any{
		"id":                  storedId,
		"registrationMode":    "open",
		"internalDefaultRole": "reader",
		"brandName":           "Acme",
	})

	after := latestPayload(t, ctx, db, conceptName, storedId)
	require.Equal(t, "Acme", after["brandName"], "the supplied field must still win")
	require.Equal(t, bootstrapStamp3415, after["bootstrappedAt"],
		"a write that omits bootstrappedAt must NOT un-bootstrap the cluster (memql#3415)")
}

// TestNoUnset3415_UpdateClusterSettings_CannotBlankBootstrappedAt closes the
// other half: a caller that passes bootstrappedAt EXPLICITLY empty. Omitting it
// was already safe under read-merge; passing "" was not, and "un-bootstrap the
// cluster" must not be an ordinary write on any path.
func TestNoUnset3415_UpdateClusterSettings_CannotBlankBootstrappedAt(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	const conceptName = "v1:identity:clusterSettings"
	settingsId := "cs3415-" + uniqueSuffix("update")

	storedId := runMutation(t, ctx, eng, "createClusterSettings", map[string]any{
		"id":                  settingsId,
		"registrationMode":    "open",
		"internalDefaultRole": "reader",
		"bootstrappedAt":      bootstrapStamp3415,
	})

	runMutation(t, ctx, eng, "updateClusterSettings", map[string]any{
		"id":                  storedId,
		"registrationMode":    "open",
		"internalDefaultRole": "reader",
		"bootstrappedAt":      "",
	})

	after := latestPayload(t, ctx, db, conceptName, storedId)
	require.Equal(t, bootstrapStamp3415, after["bootstrappedAt"],
		"an explicitly-empty bootstrappedAt must be refused, not applied (memql#3415)")
}

// TestNoUnset3415_StampingStillWorks is the counterweight: the guard must
// refuse only the set -> unset direction. The verifier / self-heal path
// (StampClusterBootstrapped) writes a NON-empty stamp onto a row that has
// none, and that must keep working, or a fresh cluster can never bootstrap.
func TestNoUnset3415_StampingStillWorks(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	const conceptName = "v1:identity:clusterSettings"
	settingsId := "cs3415-" + uniqueSuffix("stamp")

	// Fresh, unclaimed row: bootstrappedAt empty.
	storedId := runMutation(t, ctx, eng, "createClusterSettings", map[string]any{
		"id":                  settingsId,
		"registrationMode":    "open",
		"internalDefaultRole": "reader",
	})
	before := latestPayload(t, ctx, db, conceptName, storedId)
	require.Equal(t, "", before["bootstrappedAt"], "a fresh row starts unclaimed")

	// The stamp lands.
	runMutation(t, ctx, eng, "updateClusterSettings", map[string]any{
		"id":                  storedId,
		"registrationMode":    "open",
		"internalDefaultRole": "reader",
		"bootstrappedAt":      bootstrapStamp3415,
	})

	after := latestPayload(t, ctx, db, conceptName, storedId)
	require.Equal(t, bootstrapStamp3415, after["bootstrappedAt"],
		"empty -> set must remain writable (the one-way transition's allowed direction)")
}
