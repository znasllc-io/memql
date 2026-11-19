package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// THE CALLS CAPACITY READS START FROM THEIR LOOKUP INDEXES. Every join, start and
// queue poll counts a media server's load under the server's write gate
// (integrations/calls rooms.go loadOn): the open rooms on the server, then the seats
// and the queue entries of all of them, one read each. A read that does not start from
// a lookup enumerates every row of its concept while that lock is held, so this holds
// each to the index that serves it, and the two list reads to comparing the room ids
// CANONICAL, as the rows store them: a bare id in the list would match no row, and the
// capacity count would read an empty server.
//
// Postgres-gated: skips when no DB is reachable, like the other lookup reads' tests.
func TestCallsCapacityReadsStartFromTheirLookups(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return
	}
	hook := &latestPlanCapture{}
	db.AddQueryHook(hook)
	// The cluster acting, with internal origin: every one of these is @serverOnly.
	reader := auth.ContextWithInternalOrigin(clusterOwnerCtx("v1:identity:user:calls-capacity-reader"))
	for _, c := range []struct {
		name, query, field, value string
		// partial is the further equality the lookup spells out when the index serving it
		// holds only the versions that carry it.
		partial string
	}{
		{"the open rooms of one media server", `query callsOpenRoomsOnServer(mediaServerId: "capacity-server")`,
			"mediaServerId", "v1:calls:mediaServer:capacity-server", "(payload->>'status') = 'open'"},
		{"the seats of those rooms", `query callsSeatsForRooms(roomIds: ["capacity-a", "capacity-b"])`,
			"roomId", "v1:calls:room:capacity-b", ""},
		{"their queue entries", `query callsQueueEntriesForRooms(roomIds: ["capacity-a", "capacity-b"])`,
			"roomId", "v1:calls:room:capacity-a", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, scans := capturedScans(t, eng, hook, reader, c.query)
			require.NotEmpty(t, scans, "the read issued no concept scan")
			lookup := lookupSubquery(t, scans[0])
			require.Contains(t, lookup, "(payload->>'"+c.field+"')", "the read does not start from the %s lookup index", c.field)
			require.Contains(t, lookup, c.value, "the read does not compare the ids canonical, as the rows store them")
			if c.partial != "" {
				require.Contains(t, lookup, c.partial, "the read does not start from the lookup index of the versions that carry %s", c.partial)
			}
		})
	}
}

// THE OPEN ROOMS READ ENUMERATES ONLY ROOMS WITH AN OPEN VERSION KEPT (ruling final I3). The
// capacity count every join, start and poll makes under its media server's gate reads the
// server's open rooms, and the lookup it starts from holds only the versions of a room
// written while it was open (memory_nodes_calls_open_room_lookup_idx, partial on status).
// An ended room's newest version is its ended one, and once its superseded versions have
// expired (v1:calls:room keeps only its newest version past a day, @retention) nothing of it
// is left in that index, so the read never enumerates it. Before that, an ended room is
// enumerated and the filter drops it at its latest version, as every lookup read does: the
// read's cost is the server's open rooms plus the rooms open on it within the last day,
// never every room it ever carried.
//
// The rows are written as the sweep leaves them (component/retention proves the sweep): an
// open room with two versions, a room that ended a day ago with only its ended version, and
// a room that ended an hour ago with its open versions still kept.
func TestTheOpenRoomsReadEnumeratesOnlyRoomsWithAnOpenVersionKept(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return
	}
	hook := &latestPlanCapture{}
	db.AddQueryHook(hook)
	ctx := context.Background()
	sfx := uniqueSuffix("calls-open-rooms")
	marker := "calls-open-rooms:" + sfx
	server := "open-rooms-" + sfx
	open, endedLong, endedRecent := "v1:calls:room:open-"+sfx, "v1:calls:room:ended-long-"+sfx, "v1:calls:room:ended-recent-"+sfx
	now := time.Now().UTC().Truncate(time.Microsecond)
	var nodes []memorynodes.MemoryNode
	for _, v := range []struct {
		id, status string
		at         time.Time
	}{
		{open, "open", now.Add(-2 * time.Hour)}, {open, "open", now.Add(-time.Hour)},
		{endedLong, "ended", now.Add(-26 * time.Hour)},
		{endedRecent, "open", now.Add(-2 * time.Hour)}, {endedRecent, "ended", now.Add(-time.Hour)},
	} {
		payload, err := json.Marshal(map[string]any{
			"ownerUserId": "v1:identity:user:owner-" + sfx, "siteId": "v1:platform:site:site-" + sfx,
			"mediaServerId": "v1:calls:mediaServer:" + server, "mediaRoom": v.id, "status": v.status,
		})
		require.NoError(t, err)
		nodes = append(nodes, memorynodes.MemoryNode{ID: v.id, Concept: "v1:calls:room", CreatedAt: v.at, CreatedBy: marker, Payload: payload})
	}
	_, err := db.NewInsert().Model(&nodes).Exec(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where(`"createdBy" = ?`, marker).Exec(context.Background())
	})

	reader := auth.ContextWithInternalOrigin(clusterOwnerCtx("v1:identity:user:calls-open-rooms-reader"))
	result, scans := capturedScans(t, eng, hook, reader, fmt.Sprintf(`query callsOpenRoomsOnServer(mediaServerId: %q)`, server))
	require.Equal(t, []string{open}, pageIDs(t, result), "the read answered a room that is not open")
	require.NotEmpty(t, scans, "the read issued no concept scan")
	rows, err := db.QueryContext(ctx, lookupSubquery(t, scans[0]))
	require.NoError(t, err)
	defer rows.Close()
	var enumerated []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		enumerated = append(enumerated, id)
	}
	require.NoError(t, rows.Err())
	require.ElementsMatch(t, []string{open, endedRecent}, enumerated,
		"the read enumerated a room with no open version left, or missed one that still has one")
}

// THE KEY'S BOOKINGS READ STARTS FROM ITS LOOKUP INDEX, its status and end terms beside
// it. Every booking and move reads the bookings of its key under the key's write gate
// (integrations/calls schedule.go upcoming), filtered to those that may still hold a slot;
// a read that did not start from the keyId lookup would enumerate every booking of the
// concept while that lock is held.
func TestCallsKeyBookingsReadStartsFromItsLookup(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return
	}
	hook := &latestPlanCapture{}
	db.AddQueryHook(hook)
	reader := auth.ContextWithInternalOrigin(clusterOwnerCtx("v1:identity:user:calls-bookings-reader"))
	_, scans := capturedScans(t, eng, hook, reader, `query callsScheduledCallsForKey(keyId: "bookings-key")`)
	require.NotEmpty(t, scans, "the read issued no concept scan")
	lookup := lookupSubquery(t, scans[0])
	require.Contains(t, lookup, "(payload->>'keyId')", "the read does not start from the keyId lookup index")
	require.Contains(t, lookup, "v1:calls:key:bookings-key", "the read does not compare the key id canonical, as the rows store it")
}
