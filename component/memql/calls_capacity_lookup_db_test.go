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
