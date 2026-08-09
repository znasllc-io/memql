package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// relationship_versioned_ids_3397_db_test.go covers the OTHER read path that
// scanned RAW rows and collapsed them afterwards: executeFilterQuery
// (memql#3397), the sibling of the executeCombinedFilterQuery path memql#3388
// fixed.
//
// The two are reached differently, and that difference is why this was filed
// separately rather than folded in:
//
//   - executeCombinedFilterQuery is the FILTER path -- concept==, payload
//     predicates, the paginated concept browser. The keyset cursor lives
//     there, and memql#3388 is its story.
//   - executeFilterQuery is what the RELATIONSHIP path resolves ids through.
//     fetchNodesByIds / fetchNodesByJSONFieldValues / fetchNodesByNodeFieldValues
//     (executor_mutation.go) each hand it a bounded `id IN (...)` or
//     `<field> IN (...)` lookup on behalf of parentOf / childOf / contains /
//     owns / interactsWith / createdBy. No cursor ever reaches it.
//
// The defect it carried had Defect A's shape. The SQL LIMIT was `target * 2`
// RAW rows -- an assumption of at most ~2 versions per id -- and the scan then
// collapsed to latest-per-id, so a window whose consecutive rows share an id
// yields fewer rows than were asked for.
//
// A short page is bad; a short ID LOOKUP is worse, because the caller does not
// merely under-report a count. fetchNodesByIds walks the ids it asked for,
// finds the ones the scan never reached absent from the result, and treats
// each as a dangling reference -- "memql reference missing; skipping node", a
// Warn log and a silently smaller graph bundle. The traversal then answers
// "this row has no parent" about a parent that exists.
//
// CLUSTERED SEEDING, and why it matters (the note carried over from
// memql#3388): every version of an id is written in a contiguous createdAt
// run, so consecutive raw rows share an id. That is the shape a
// repeatedly-updated row actually has. Interleaving instead (id0.v0, id1.v0,
// ... id0.v1) hands consecutive raw rows to DIFFERENT ids, lets the raw window
// collapse to a full result by luck, and hides the defect completely -- which
// is what made memql#3388 take three rounds to find.
//
// Postgres-gated: skips when no DB is reachable, reusing readMergeTestEngine.

const (
	participantConcept = "v1:cognition:participant"
	// presence is @namespace("cognition:participant"), so its concept id
	// nests under the participant namespace rather than under cognition.
	presenceConcept = "v1:cognition:participant:presence"
)

// seedRawRow inserts ONE append-only row at a fixed createdAt, bypassing the
// mutation validators. The relationship resolvers read `concept`, `id` and the
// payload field naming the related row, so a minimal payload is enough and a
// validator-shaped one would only obscure the fixture.
func seedRawRow(
	t *testing.T,
	ctx context.Context,
	db *bun.DB,
	conceptName string,
	id string,
	createdAt time.Time,
	owner string,
	payload map[string]any,
) {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	node := &memorynodes.MemoryNode{
		ID:         id,
		Concept:    conceptName,
		CreatedBy:  owner,
		CreatedAt:  createdAt.UTC(),
		Payload:    raw,
		Provenance: json.RawMessage(`{"kind":"direct","name":"relationship-3397-test"}`),
	}
	_, err = db.NewInsert().Model(node).
		On(`CONFLICT (id, "createdAt") DO NOTHING`).
		Exec(ctx)
	require.NoError(t, err, "seed %s row %s", conceptName, id)
}

// TestRelationshipParentOf_ClusteredVersionsReachesEveryParent is the headline
// memql#3397 regression, end to end through Execute.
//
// 10 presence rows, each naming a DISTINCT participant as its parent, each
// participant carrying 10 clustered versions. `parentOf(...)` must return all
// 10 parents.
//
// Against the raw-scan engine it returned 2: resolveParentOf collected the 10
// parent ids and asked fetchNodesByIds for them, the scan took `10 * 2 = 20`
// RAW rows ordered `createdAt DESC` over `id IN (<the 10>)` -- the 20 newest
// versions, all belonging to the two most-recently-written participants -- and
// the other 8 were logged as missing references and dropped.
func TestRelationshipParentOf_ClusteredVersionsReachesEveryParent(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-rel-3397-parent")
	sfx := uniqueSuffix("rel-3397-parent")
	owner := "kb:" + sfx

	const (
		parents  = 10
		versions = 10
	)

	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	wantParents := make([]string, 0, parents)
	tick := 0
	for i := 0; i < parents; i++ {
		participantId := fmt.Sprintf("%s:%s-%02d", participantConcept, sfx, i)
		wantParents = append(wantParents, participantId)
		for v := 0; v < versions; v++ {
			// Clustered: every version of participant i occupies a
			// contiguous createdAt run.
			seedRawRow(t, ctx, db, participantConcept, participantId,
				base.Add(time.Duration(tick)*time.Second), owner,
				map[string]any{"displayName": fmt.Sprintf("p-%02d-v%d", i, v)})
			tick++
		}
		// One presence row per participant, single version, pointing at it.
		seedRawRow(t, ctx, db, presenceConcept,
			fmt.Sprintf("%s:%s-%02d", presenceConcept, sfx, i),
			base.Add(time.Duration(tick)*time.Second), owner,
			map[string]any{"participantId": participantId})
		tick++
	}

	res, err := eng.Execute(ctx, fmt.Sprintf(
		`parentOf(concept==%s;createdBy==%q)`, presenceConcept, owner))
	require.NoError(t, err)

	require.ElementsMatch(t, wantParents, pageIDs(t, res),
		"parentOf must resolve every parent; a raw-row window of target*2 collapses onto the "+
			"most-recently-written ids and the rest are dropped as dangling references")
}

// TestExecuteFilterQuery_ClusteredVersionsFillsTheTarget pins the seam itself,
// at the exact call shape fetchNodesByIds uses: a nil comparison, an
// `id IN (...)` filter, `target` = the number of ids asked about, no sorter.
//
// Kept alongside the traversal test rather than instead of it. That one proves
// the user-visible loss; this one localises it to executeFilterQuery, so a
// future regression names the function instead of sending the next reader
// through the relationship resolvers to find it -- which is the hour memql#3388
// records losing in the other direction.
func TestExecuteFilterQuery_ClusteredVersionsFillsTheTarget(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-rel-3397-seam")
	sfx := uniqueSuffix("rel-3397-seam")
	owner := "kb:" + sfx

	const (
		distinct = 8
		versions = 12
	)

	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	ids := make([]string, 0, distinct)
	tick := 0
	for i := 0; i < distinct; i++ {
		id := fmt.Sprintf("%s:%s-%02d", participantConcept, sfx, i)
		ids = append(ids, id)
		for v := 0; v < versions; v++ {
			seedRawRow(t, ctx, db, participantConcept, id,
				base.Add(time.Duration(tick)*time.Second), owner,
				map[string]any{"displayName": fmt.Sprintf("s-%02d-v%d", i, v)})
			tick++
		}
	}

	filter := compiledExpression{sql: "(id IN (?))", args: []any{bun.In(ids)}}
	nodes, err := eng.executeFilterQuery(ctx, nil, filter, nil, len(ids), nil)
	require.NoError(t, err)

	got := make([]string, 0, len(nodes))
	for _, n := range nodes {
		got = append(got, n.ID)
	}
	require.ElementsMatch(t, ids, got,
		"a lookup of %d ids must return all %d; the raw window collapsed to %d",
		len(ids), len(ids), len(got))
}

// TestExecuteFilterQuery_LatestVersionWinsUnderAsOf guards the two semantics
// the SQL collapse has to preserve while it removes the raw window.
//
// The row returned for an id must be its LATEST version, and under an asOf
// timestamp the latest version AS OF that instant -- so the collapse has to
// ride INSIDE the asOf predicate, not outside it. Seeded with clustered
// versions so the fix's DISTINCT ON subquery is what is under test, not a
// single-version happy path.
func TestExecuteFilterQuery_LatestVersionWinsUnderAsOf(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-rel-3397-asof")
	sfx := uniqueSuffix("rel-3397-asof")
	owner := "kb:" + sfx

	const (
		distinct = 4
		versions = 5
	)

	base := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	ids := make([]string, 0, distinct)
	tick := 0
	for i := 0; i < distinct; i++ {
		id := fmt.Sprintf("%s:%s-%02d", participantConcept, sfx, i)
		ids = append(ids, id)
		for v := 0; v < versions; v++ {
			seedRawRow(t, ctx, db, participantConcept, id,
				base.Add(time.Duration(tick)*time.Second), owner,
				map[string]any{"displayName": fmt.Sprintf("%s-v%d", id, v)})
			tick++
		}
	}

	filter := compiledExpression{sql: "(id IN (?))", args: []any{bun.In(ids)}}

	// No asOf: every id resolves to its final version.
	nodes, err := eng.executeFilterQuery(ctx, nil, filter, nil, len(ids), nil)
	require.NoError(t, err)
	require.Len(t, nodes, distinct)
	for _, n := range nodes {
		var payload map[string]any
		require.NoError(t, json.Unmarshal(n.Payload, &payload))
		require.Equal(t, fmt.Sprintf("%s-v%d", n.ID, versions-1), payload["displayName"],
			"without asOf, %s must resolve to its latest version", n.ID)
	}

	// asOf pinned to the instant id 0 wrote its v2: id 0 must read back v2,
	// and the ids seeded after it must not appear at all.
	asOf := base.Add(2 * time.Second)
	nodes, err = eng.executeFilterQuery(ctx, nil, filter, &asOf, len(ids), nil)
	require.NoError(t, err)
	require.Len(t, nodes, 1, "only the id written before the asOf instant exists yet")

	var payload map[string]any
	require.NoError(t, json.Unmarshal(nodes[0].Payload, &payload))
	require.Equal(t, ids[0], nodes[0].ID)
	require.Equal(t, fmt.Sprintf("%s-v2", ids[0]), payload["displayName"],
		"under asOf, the collapse must pick the latest version AS OF that instant")
}
