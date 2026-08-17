package memql

// authoring_concept_staged_db_test.go -- the half of the staged-DATA tier (epic
// memql#3974) that a fake store cannot vouch for, against a REAL engine and a
// REAL Postgres: that the stamp actually LANDS on the row and comes back off it.
//
// Everything about the decision -- the default, the fold, the restart -- is
// unit-tested against fakes in authoring_concept_staged_test.go. What is left
// here is precisely what those fakes stand in for, and it is two distinct things
// that present identically when broken (the promote succeeds and the flag is
// silently absent):
//
//   - the MUTATION CALL FORM parses. The `name({...})` object-literal wrapper
//     was removed from the grammar in memql#2335, and the neighbouring durable
//     promote's persist calls were written that way and failed on every live
//     engine for months while every fake-store test passed -- the one line that
//     had to change when the grammar moved was the one line no test executed;
//   - the MUTATION WRITES the field and the boot walk's own read gets it back,
//     rather than the call being well-formed against a property the concept does
//     not declare, or the write landing somewhere the re-hydration cannot see.
//
// The read-back goes through enginePromoteRehydrateStore.LoadConstructsForBundle
// -- the production per-bundle load the boot walk uses -- rather than through
// its LoadPromotedBundles enumeration, and the difference is worth stating
// because it is not a convenience. That enumeration matches the durable-promote
// bundle PREFIX against `node.GetId()`, which over the in-process Execute path is
// the CANONICAL id (`v1:authoring:bundle:mcp-promote-...`), so the prefix never
// matches and it returns nothing at all. That is a PRE-EXISTING defect in the
// promote re-hydration path, older than this epic and not this task's to fix;
// pinning the staged-data stamp on top of it would have made this test fail for
// a reason that has nothing to do with what it is testing.
//
// Postgres-gated via readMergeTestEngine: skips when no DB is reachable, like
// every other _db_ test in this package. The fixture carries a per-process
// unique namespace so concurrent runs never collide, and nothing here truncates
// anything.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// recordingPromoteStore is the REAL enginePromoteStore with the generated ids
// captured on the way past. It delegates every call, so the mutations under test
// are the production ones; it exists only because the durable promote generates
// the bundle + construct ids internally and returns neither, and the read-back
// needs the bundle id.
type recordingPromoteStore struct {
	*enginePromoteStore
	bundleId    string
	constructId string
}

func (s *recordingPromoteStore) CreatePromoteBundle(ctx context.Context, bundleId, title, summary string) error {
	s.bundleId = bundleId
	return s.enginePromoteStore.CreatePromoteBundle(ctx, bundleId, title, summary)
}

func (s *recordingPromoteStore) CreatePromoteConstruct(ctx context.Context, constructId, bundleId, kind, name, targetNamespace, source, status string) error {
	s.constructId = constructId
	return s.enginePromoteStore.CreatePromoteConstruct(ctx, constructId, bundleId, kind, name, targetNamespace, source, status)
}

// stagedDataDBConceptSrc builds a uniquely-namespaced concept source so two runs
// (or two agents sharing a database) cannot claim the same canonical id.
func stagedDataDBConceptSrc(ns string) string {
	return fmt.Sprintf(`@version("1.0.0")
@namespace(%q)
@description("A concept taught to a running cluster, with its data staged")
concept stagedWidget {
  ownerUserId  string
  label        string
}`, ns)
}

// promoteConceptThroughTheRealStore authors a concept and durably promotes it
// through the production enginePromoteStore -- the point of this file. The
// helpers everywhere else deliberately avoid that store; here it is the subject.
func promoteConceptThroughTheRealStore(t *testing.T, eng *MemQLEngine, ctx context.Context, owner, source string, opts ...PromoteDurableOption) *recordingPromoteStore {
	t.Helper()
	reg := NewAuthoredRuntimeRegistry()
	res, err := AuthorSessionBundle(reg, owner, source, "")
	if err != nil {
		var detail []string
		for _, d := range res.Diagnostics {
			if !d.OK && strings.TrimSpace(d.Error) != "" {
				detail = append(detail, d.Error)
			}
		}
		t.Fatalf("author concept: %v: %s", err, strings.Join(detail, "; "))
	}
	c, ok := reg.Lookup(owner, "concept", "stagedWidget")
	require.True(t, ok, "session define did not register concept stagedWidget")

	store := &recordingPromoteStore{enginePromoteStore: &enginePromoteStore{engine: eng}}
	require.NoError(t,
		eng.promoteConstructDurableWithStore(ctx, store, nil, owner, c, opts...),
		"durable promote through the REAL store")
	require.NotEmpty(t, store.bundleId, "the promote persisted no bundle")
	return store
}

// readBackConstructRow loads the persisted construct row exactly as the boot walk
// does: the owner-scoped authoringConstructsForBundle query, projected through
// constructFull, parsed by parseConstructRow. A field this cannot see is a field
// the re-hydration cannot see either.
//
// It identifies the row by BEING THE ONLY ONE rather than by matching the id the
// promote generated, and that is the same canonical-versus-bare mismatch the
// file header records on the bundle side: parseConstructRow takes its Id from
// `node.GetId()`, which over the in-process Execute path is
// `v1:authoring:construct:mcp-promote-...`, while the id the promote generated
// and wrote is the bare `mcp-promote-...`. A durable promote of one construct
// writes exactly one construct row, so the count is an unambiguous identifier
// here and does not depend on which form wins.
func readBackConstructRow(t *testing.T, eng *MemQLEngine, ctx context.Context, owner, bundleId string) AuthoringConstructRow {
	t.Helper()
	rows, err := (&enginePromoteRehydrateStore{engine: eng}).LoadConstructsForBundle(ctx, owner, bundleId)
	require.NoError(t, err, "load constructs for bundle %s", bundleId)
	require.Len(t, rows, 1, "bundle %q should hold exactly the one promoted construct", bundleId)
	require.Equal(t, "concept", rows[0].Kind)
	return rows[0]
}

// TestConceptDataStagedDurable_StampLandsOnTheRowAndReadsBack: the acceptance
// that the durable half of the tier is real.
//
// Both directions are asserted in one test on purpose. "The staged promote's row
// comes back staged" would also pass against a shape that returned true for
// everything, or a mutation that stamped every row it could reach; the ordinary
// promote alongside it is what shows the instrument can move.
func TestConceptDataStagedDurable_StampLandsOnTheRowAndReadsBack(t *testing.T) {
	before := memoryNodes.All()
	t.Cleanup(func() { memoryNodes.ReplaceAll(before) })
	eng, _, ctx := readMergeTestEngine(t)

	stagedOwner := "owner-" + uniqueSuffix("stagedon")
	stagedNs := "staged3974" + strings.ReplaceAll(uniqueSuffix("on"), "-", "")
	staged := promoteConceptThroughTheRealStore(t, eng, ctx, stagedOwner, stagedDataDBConceptSrc(stagedNs), WithConceptDataStaged())
	stagedRow := readBackConstructRow(t, eng, ctx, stagedOwner, staged.bundleId)

	require.True(t, stagedRow.ConceptDataStaged,
		"the staged-data stamp did not survive the round trip through a real engine. Either the mutation call form no longer parses, or the mutation writes nothing, or constructFull does not project the field -- in all three the promote succeeds and every staged concept comes back LIVE at the next boot")
	require.Equal(t, string(BundleActive), stagedRow.Status,
		"the staged-data stamp moved the row off active; the boot walk skips a row that is not active, so the concept would not re-register and its rows would be unreadable rather than staged")
	require.False(t, stagedRow.ConceptRetired,
		"staging the DATA also stamped the memql#3756 write retirement: the two concept-only flags are aliasing on the row")

	// The control, in a fresh namespace under a fresh owner, so it is a
	// genuinely different concept and a genuinely different row.
	liveOwner := "owner-" + uniqueSuffix("stagedoff")
	liveNs := "staged3974" + strings.ReplaceAll(uniqueSuffix("off"), "-", "")
	live := promoteConceptThroughTheRealStore(t, eng, ctx, liveOwner, stagedDataDBConceptSrc(liveNs))
	liveRow := readBackConstructRow(t, eng, ctx, liveOwner, live.bundleId)

	require.False(t, liveRow.ConceptDataStaged,
		"an ORDINARY promote's row came back staged: the default is not live, and every installation's data would be withheld once a read seam consults it")
}
