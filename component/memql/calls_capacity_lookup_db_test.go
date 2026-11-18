package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
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
	}{
		{"the open rooms of one media server", `query callsOpenRoomsOnServer(mediaServerId: "capacity-server")`,
			"mediaServerId", "v1:calls:mediaServer:capacity-server"},
		{"the seats of those rooms", `query callsSeatsForRooms(roomIds: ["capacity-a", "capacity-b"])`,
			"roomId", "v1:calls:room:capacity-b"},
		{"their queue entries", `query callsQueueEntriesForRooms(roomIds: ["capacity-a", "capacity-b"])`,
			"roomId", "v1:calls:room:capacity-a"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, scans := capturedScans(t, eng, hook, reader, c.query)
			require.NotEmpty(t, scans, "the read issued no concept scan")
			lookup := lookupSubquery(t, scans[0])
			require.Contains(t, lookup, "(payload->>'"+c.field+"')", "the read does not start from the %s lookup index", c.field)
			require.Contains(t, lookup, c.value, "the read does not compare the ids canonical, as the rows store them")
		})
	}
}
