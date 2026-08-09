package memql

import (
	"context"
	"fmt"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// relationship_incoming_target_3432_db_test.go covers the ARITHMETIC half of
// the relationship-lookup story memql#3397 fixed the SCAN half of.
//
// memql#3397 made executeFilterQuery's SQL enumerate the result set, so its
// LIMIT bounds distinct ids rather than raw versions. That is a correct scan.
// This file is about the number that scan is bounded TO:
//
//	target = len(values)      // fetchNodesByJSONFieldValues
//	target = len(values)      // fetchNodesByNodeFieldValues
//
// `values` is the set of SOURCE ids being looked up. For an OUTGOING lookup
// that is right -- fetchNodesByIds asks `id IN (...)`, each id resolves to at
// most one row, and the two counts coincide. For an INCOMING lookup it is
// wrong by construction: the question is "which rows point AT these ids", and
// nothing bounds that answer by how many ids were asked about. One parent
// returns at most ONE child, because len(values) == 1.
//
// The two halves compose but neither substitutes for the other: a correct
// scan bounded to an incorrect target is still wrong, and the fix is in the
// CALLER, not in executeFilterQuery.
//
// WHY THIS NEEDED A FIXTURE CONCEPT. Every one of the 141 @relationship
// declarations in the shipped corpus is direction="outgoing", so childOf and
// the incoming branches of owns / interactsWith / createdBy are unreachable
// against it -- the defect is real but latent, and no test written against
// the corpus can fail. So the fixture below declares the missing shape: a
// `parent` relationship with direction="incoming", registered through
// memqldsl.RegisterTree -- the same path a product DSL bundle mounts through
// at MEMQL_DSL_PATH, and the convention the other bespoke-DSL tests in this
// package already use (builtin_enabled_flag_test.go,
// lifecycle_disabled_loaders_test.go).
//
// CLUSTERED SEEDING, carried over from memql#3388 and #3397: every version of
// an id occupies a contiguous createdAt run, which is the shape a repeatedly-
// updated row actually has. Interleaving hands consecutive raw rows to
// DIFFERENT ids and lets a short window collapse to a full result by luck --
// that is what made #3388 take three rounds to find. It does not rescue this
// defect (a LIMIT of 1 returns one row however the rows are ordered), but
// seeding the honest shape is what keeps the fixture a regression test for
// the whole class rather than for one arithmetic slip.
//
// Postgres-gated: skips when no DB is reachable, reusing readMergeTestEngine.

// The fixture domain and the two canonical ids it assembles to. Concept ids
// are `v<major>:<domain>:<name>` with no @version / @namespace annotation, so
// the domain name IS the namespace segment.
const (
	incomingFixtureDomain = "rel3432"
	hubConcept            = "v1:rel3432:hub"
	spokeConcept          = "v1:rel3432:spoke"
)

// incomingFixtureConcepts is the shape the shipped corpus does not have.
//
// The incoming declaration lives on the HUB and names a field on the SPOKE:
// "spoke rows point at me through spoke.hubId". That is what makes
// childOf(<a hub>) resolvable -- resolveChildOf filters the source concept's
// relationships for type=parent with direction incoming/bidirectional, and
// queries the named target concept by the named field.
const incomingFixtureConcepts = `/// Fixture parent for memql#3432. Declares the INCOMING side of a parent
/// relationship, which no concept in the shipped corpus does, so childOf and
/// the incoming lookup branches become reachable.
concept hub {
  label string @description("Human label, so the fixture rows are legible in a failure dump.")

  @relationship(type="parent", field="hubId", target=spoke, direction="incoming")
}

/// Fixture child for memql#3432. Many spokes point at one hub through hubId,
/// which is precisely the case len(sourceIds) cannot bound.
concept spoke {
  hubId string @description("The v1:rel3432:hub.id this spoke hangs off.")
  label string @description("Human label, so the fixture rows are legible in a failure dump.")
}
`

// mountIncomingFixture registers the fixture domain and restores the global
// concept registry afterwards.
//
// LoadUnifiedConcepts merges into the process-wide default registry and there
// is no per-concept removal, so the snapshot/ReplaceAll pair is what keeps a
// fixture concept from outliving its test and appearing in whatever later
// test enumerates concepts. UnregisterTree alone would not do it: the tree is
// the SOURCE, the registry is where the loaded concept lands.
func mountIncomingFixture(t *testing.T) {
	t.Helper()
	before := memorynodes.All()
	memqldsl.RegisterTree(incomingFixtureDomain, fstest.MapFS{
		"concepts.memql": {Data: []byte(incomingFixtureConcepts)},
	})
	t.Cleanup(func() {
		memqldsl.UnregisterTree(incomingFixtureDomain)
		memorynodes.ReplaceAll(before)
	})
}

// seedHubWithSpokes writes one hub and `spokes` spoke rows that all point at
// it, every id carrying `versions` CLUSTERED versions. Returns the hub id and
// the spoke ids in seeding order.
func seedHubWithSpokes(
	t *testing.T,
	ctx context.Context,
	db *bun.DB,
	sfx string,
	owner string,
	base time.Time,
	spokes int,
	versions int,
) (string, []string) {
	t.Helper()

	hubId := fmt.Sprintf("%s:%s", hubConcept, sfx)
	tick := 0
	for v := 0; v < versions; v++ {
		seedRawRow(t, ctx, db, hubConcept, hubId,
			base.Add(time.Duration(tick)*time.Second), owner,
			map[string]any{"label": fmt.Sprintf("hub-v%d", v)})
		tick++
	}

	spokeIds := make([]string, 0, spokes)
	for i := 0; i < spokes; i++ {
		spokeId := fmt.Sprintf("%s:%s-%02d", spokeConcept, sfx, i)
		spokeIds = append(spokeIds, spokeId)
		for v := 0; v < versions; v++ {
			// Clustered: every version of spoke i is contiguous in
			// createdAt before spoke i+1's first version is written.
			seedRawRow(t, ctx, db, spokeConcept, spokeId,
				base.Add(time.Duration(tick)*time.Second), owner,
				map[string]any{
					"hubId": hubId,
					"label": fmt.Sprintf("spoke-%02d-v%d", i, v),
				})
			tick++
		}
	}

	return hubId, spokeIds
}

// TestRelationshipChildOf_IncomingRelationshipReachesEveryChild is the
// headline memql#3432 regression, end to end through Execute.
//
// ONE hub, TEN spokes pointing at it, every id at 10 clustered versions.
// childOf(<the hub>) must return all ten.
//
// Against the unfixed engine it returned ONE. resolveChildOf collected the
// single hub id, handed it to fetchNodesByJSONFieldValues, and that function
// set target = len(values) = 1 -- so the lookup asked the database for one
// row and the other nine spokes were never scanned. No error, no warning: a
// traversal that answers "this hub has one child" about a hub with ten.
func TestRelationshipChildOf_IncomingRelationshipReachesEveryChild(t *testing.T) {
	mountIncomingFixture(t)
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-rel-3432-childof")
	sfx := uniqueSuffix("rel-3432-childof")
	owner := "kb:" + sfx

	// The fixture must actually have reached the engine, or this test would
	// pass by resolving nothing. resolveChildOf errors when the source
	// concept declares no incoming parent relationship, so a silently
	// unmounted fixture fails loudly rather than vacuously -- but assert it
	// directly so the failure names the cause.
	require.Contains(t, eng.relationships.ByConcept, hubConcept,
		"the memql#3432 fixture concept did not reach the engine; without it the incoming branch is unreachable and this test proves nothing")
	require.Len(t, filterRelationshipDefinitions(
		eng.relationships.ByConcept[hubConcept],
		relationshipTypeParent,
		[]string{relationshipDirectionIncoming, relationshipDirectionBidirectional},
	), 1, "the fixture hub must declare exactly one incoming parent relationship")

	const (
		spokes   = 10
		versions = 10
	)

	base := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	_, wantSpokes := seedHubWithSpokes(t, ctx, db, sfx, owner, base, spokes, versions)

	res, err := eng.Execute(ctx, fmt.Sprintf(
		`childOf(concept==%s;createdBy==%q)`, hubConcept, owner))
	require.NoError(t, err)

	require.ElementsMatch(t, wantSpokes, pageIDs(t, res),
		"childOf must reach every child; a target of len(sourceIds) caps one parent at one child")
}

// TestFetchNodesByJSONFieldValues_OneSourceIdManyTargets pins the seam
// itself, at the exact call shape resolveChildOf uses: one source id, a
// payload-field IN (...) filter, and the traversal's limit as the only real
// bound.
//
// Kept alongside the traversal test rather than instead of it. That one
// proves the user-visible loss and needs the fixture concept to be reachable
// at all; this one localises the arithmetic to the function that owns it, so
// a future regression names fetchNodesByJSONFieldValues instead of sending
// the next reader through the relationship resolvers to find it.
func TestFetchNodesByJSONFieldValues_OneSourceIdManyTargets(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-rel-3432-json")
	sfx := uniqueSuffix("rel-3432-json")
	owner := "kb:" + sfx

	const (
		spokes   = 10
		versions = 10
	)

	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	hubId, wantSpokes := seedHubWithSpokes(t, ctx, db, sfx, owner, base, spokes, versions)

	// ONE source id, ten matching rows. The engine's MaxResults is the
	// traversal limit the relationship resolvers pass down.
	nodes, err := eng.fetchNodesByJSONFieldValues(
		ctx, spokeConcept, []string{"hubId"}, []string{hubId}, nil, eng.config.MaxResults)
	require.NoError(t, err)

	got := make([]string, 0, len(nodes))
	for _, n := range nodes {
		got = append(got, n.ID)
	}
	require.ElementsMatch(t, wantSpokes, got,
		"an incoming lookup of 1 source id must return all %d matching rows; target = len(sourceIds) returned %d",
		len(wantSpokes), len(got))
}

// TestFetchNodesByNodeFieldValues_OneSourceValueManyTargets is the same
// arithmetic on the table-column sibling, which resolveCreatedBy uses for a
// FieldSourceTable relationship: "which rows did this identity create".
//
// createdBy is the only column mapNodeFieldToColumn admits, and it is a
// textbook one-source-to-many-targets question -- an identity that created
// one row is indistinguishable from one that created a thousand when the
// answer is capped at the number of identities asked about.
func TestFetchNodesByNodeFieldValues_OneSourceValueManyTargets(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-rel-3432-nodefield")
	sfx := uniqueSuffix("rel-3432-nodefield")
	owner := "kb:" + sfx

	const (
		spokes   = 10
		versions = 10
	)

	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	_, wantSpokes := seedHubWithSpokes(t, ctx, db, sfx, owner, base, spokes, versions)

	// ONE creator, ten rows it created.
	nodes, err := eng.fetchNodesByNodeFieldValues(
		ctx, spokeConcept, "createdBy", []string{owner}, nil, eng.config.MaxResults)
	require.NoError(t, err)

	got := make([]string, 0, len(nodes))
	for _, n := range nodes {
		got = append(got, n.ID)
	}
	require.ElementsMatch(t, wantSpokes, got,
		"an incoming createdBy lookup of 1 source value must return all %d rows it created; target = len(sourceIds) returned %d",
		len(wantSpokes), len(got))
}

// TestFetchNodesByJSONFieldValues_HonoursTheCallersLimit guards the other
// direction: deriving the target from the query must not mean ignoring the
// bound the query actually stated. A traversal that asks for 4 gets 4, not
// the whole matching set.
func TestFetchNodesByJSONFieldValues_HonoursTheCallersLimit(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-rel-3432-limit")
	sfx := uniqueSuffix("rel-3432-limit")
	owner := "kb:" + sfx

	const (
		spokes   = 10
		versions = 5
		limit    = 4
	)

	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	hubId, wantSpokes := seedHubWithSpokes(t, ctx, db, sfx, owner, base, spokes, versions)

	nodes, err := eng.fetchNodesByJSONFieldValues(
		ctx, spokeConcept, []string{"hubId"}, []string{hubId}, nil, limit)
	require.NoError(t, err)
	require.Len(t, nodes, limit, "the caller's limit is the query's bound and must still be honoured")

	seeded := make(map[string]struct{}, len(wantSpokes))
	for _, id := range wantSpokes {
		seeded[id] = struct{}{}
	}
	for _, n := range nodes {
		require.Contains(t, seeded, n.ID, "a limited lookup must still return rows from the matching set")
	}
}
