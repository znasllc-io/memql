package memql

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// memql#5661, through the engine's write path. An update reads the row it is
// about to rewrite through Concept.Query (loadPriorPayload), and Concept.Query
// used to read a row whose stored schema names no $id as a deletion tombstone
// whenever the concept had no delete variant -- which is every concept the
// DSL builds. So an update of such a row failed with "no existing row" while
// a query of the same row returned it. No production writer stamps a schema
// without an $id today; the database test fixtures that insert `Schema: {}`
// rows directly are where it surfaced, and any future writer that forgot the
// stamp would make its rows silently unwritable.
//
// v1:cluster:node because it declares no row tier and updateNodeHealth is a
// read-merge update: a minimal health transition only works if the prior row
// is found, and the address it never restates proves the merge read it.
func TestUpdateReadsAPriorRowWhoseSchemaNamesNoId(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	const conceptName = "v1:cluster:node"
	nodeId := "node-" + uniqueSuffix("schemaless-prior-5661")
	storedId := conceptName + ":" + nodeId

	payload, err := json.Marshal(map[string]any{
		"nodeType": "bff",
		"address":  "10.0.0.9:50051",
		"health":   "healthy",
		"lastSeen": "2026-09-26T00:00:00Z",
	})
	require.NoError(t, err)
	row := memorynodes.MemoryNode{
		ID: storedId, CreatedAt: time.Now().UTC().Add(-time.Minute), CreatedBy: "system:test", Concept: conceptName,
		Type: memorynodes.NodeTypeObject, Schema: json.RawMessage(`{}`), Payload: payload,
		Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{"kind":"direct","name":"schemaless-prior-row-5661"}`),
	}
	_, err = db.NewInsert().Model(&row).Exec(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).
			Where("concept = ?", conceptName).Where("id = ?", storedId).Exec(ctx)
		require.NoError(t, err)
	})

	runMutation(t, ctx, eng, "updateNodeHealth", map[string]any{
		"id":       nodeId,
		"health":   "degraded",
		"lastSeen": "2026-09-27T00:00:00Z",
	})

	p := latestPayload(t, ctx, db, conceptName, storedId)
	require.Equal(t, "degraded", p["health"], "health must update")
	require.Equal(t, "10.0.0.9:50051", p["address"],
		"address must be inherited from the schemaless prior row; an update that did not find that row could not have it")
	require.Equal(t, "bff", p["nodeType"], "nodeType inherited from the prior row")
}
