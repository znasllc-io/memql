package memql

// staged_transition_db_test.go -- the half of the staged -> live transition
// (memql#3986) that a fake store cannot vouch for, against a REAL engine and a
// REAL Postgres.
//
// Everything about the DECISION -- carry-forward, the independence of retirement
// and staging, the cross-node resolve -- is unit-tested against fakes in
// staged_transition_test.go. What is left here is the one thing those fakes
// stand in for, and it is the thing that has broken before and presents
// identically when broken (the transition reports success and the flag is
// silently still set):
//
//   - the `trainConstructConceptData(constructId: "...")` CALL FORM parses. The
//     `name({...})` object-literal wrapper was removed from the grammar in
//     memql#2335, and the neighbouring durable-promote persist calls were written
//     that way and failed on every live engine for months while every fake-store
//     test passed;
//   - the MUTATION WRITES `false`. A read-merge update that dropped a falsey
//     value -- or a mutation naming a field the concept does not declare --
//     leaves the stamp exactly where it was, and the boot fold then brings the
//     staging back after the operator was told the concept was trained.
//
// The read-back goes through enginePromoteRehydrateStore.LoadConstructsForBundle
// -- the production per-bundle load the boot walk uses -- rather than through its
// LoadPromotedBundles enumeration, for the reason memql#3980's sibling db test
// records at length: that enumeration prefix-matches against the CANONICAL id and
// therefore returns nothing (memql#4036). Driving the transition's own walk
// through it here would fail for a reason that has nothing to do with this
// mutation, which is why this test calls the store method directly and the walk
// is covered by the fakes next door.
//
// Postgres-gated via readMergeTestEngine: skips when no DB is reachable, like
// every other _db_ test in this package. The fixture carries a per-process unique
// namespace so concurrent runs never collide, and nothing here truncates
// anything.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestTrainConstructConceptData_ClearsTheStampOnARealRow: the acceptance that the
// durable half of the transition is real.
//
// It stages through the production store, reads the row back, clears through the
// production store, and reads it back again. Both reads are asserted, because
// "it came back false" would also pass against a mutation that never wrote
// anything and a stamp that never landed -- the staged read is what shows the
// instrument can move.
func TestTrainConstructConceptData_ClearsTheStampOnARealRow(t *testing.T) {
	before := memoryNodes.All()
	t.Cleanup(func() { memoryNodes.ReplaceAll(before) })
	eng, _, ctx := readMergeTestEngine(t)

	owner := "owner-" + uniqueSuffix("train3986")
	ns := "train3986" + strings.ReplaceAll(uniqueSuffix("ns"), "-", "")
	staged := promoteConceptThroughTheRealStore(t, eng, ctx, owner, stagedDataDBConceptSrc(ns), WithConceptDataStaged())

	row := readBackConstructRow(t, eng, ctx, owner, staged.bundleId)
	require.True(t, row.ConceptDataStaged,
		"pre-condition: the staged promote's stamp did not land, so there is nothing for the transition to clear")

	// THE SUBJECT: the production store method, running the real mutation through
	// the real engine, under the row owner's envelope.
	store := &engineConceptDataStore{engine: eng}
	require.NoError(t,
		store.TrainConstructConceptData(ctx, owner, staged.constructId),
		"clearing the staged-data stamp failed on a live engine -- either the call form no longer parses or the mutation names a field the concept does not declare")

	cleared := readBackConstructRow(t, eng, ctx, owner, staged.bundleId)
	require.False(t, cleared.ConceptDataStaged,
		"the stamp survived a successful transition: the update wrote nothing (a read-merge that drops a falsey value looks exactly like this), so the boot fold brings the staging back after the operator was told the concept was trained")
	require.Equal(t, string(BundleActive), cleared.Status,
		"the transition moved the row off active; the boot walk skips a row that is not active, so the concept would stop re-registering and its rows would become UNREADABLE rather than live")
	require.False(t, cleared.ConceptRetired,
		"the transition also cleared the memql#3756 write retirement: the two concept-only flags are aliasing on the row, and publishing data re-opened the concept to writes")

	// IDEMPOTENT ON A REAL ROW. The transition's retry path runs this against
	// rows it has already cleared, so a second clear must be a successful no-op
	// rather than a read-merge that fails to match anything and errors.
	require.NoError(t,
		store.TrainConstructConceptData(ctx, owner, staged.constructId),
		"a second clear of an already-clear row failed: a partial transition would not be retryable")
	again := readBackConstructRow(t, eng, ctx, owner, staged.bundleId)
	require.False(t, again.ConceptDataStaged, "the second clear re-stamped the row")
}
