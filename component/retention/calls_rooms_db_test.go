package retention

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
)

// calls_rooms_db_test.go -- the hourly sweep over v1:calls:room, whose superseded versions
// expire after a day (dsl/calls/concepts.memql; pinned by component/memql
// calls_concepts_test.go TestCallsRoomKeepsOnlyItsNewestVersionPastADay).
//
// WHY IT MATTERS (ruling final I3): every join, start and poll counts its media server's
// load under the server's write gate, starting from the open rooms of the server, and the
// lookup that read starts from holds only the versions written while a room was open. An
// ended room leaves that index only once its superseded open versions are gone, so this is
// what keeps the read to the server's open rooms and the rooms open on it within the last
// day. A tier that did not admit the sweeper would refuse the whole batch and keep every
// version for good, which a declaration-only test cannot see.

// TestTheSweepLeavesAnEndedRoomOnlyItsNewestVersion: past a day, a room keeps only its
// newest version -- the ended one, for a room that ended -- and an open room's newest
// version is never taken however old it is.
func TestTheSweepLeavesAnEndedRoomOnlyItsNewestVersion(t *testing.T) {
	f, integration := statusRowSweep(t, "v1:calls:room")
	ended, quiet := "v1:calls:room:ended", "v1:calls:room:quiet"
	room := func(status string) string {
		return `{"ownerUserId":"v1:identity:user:the-owner","siteId":"v1:platform:site:s","mediaServerId":"v1:calls:mediaServer:m1","mediaRoom":"r","status":"` + status + `"}`
	}

	// A room that ended: two open versions past a day, one younger, and its end.
	f.insert(t, "MemoryNodes", ended, 50*time.Hour, room("open"))
	f.insert(t, "MemoryNodes", ended, 30*time.Hour, room("open"))
	f.insert(t, "MemoryNodes", ended, 25*time.Hour, room("ended"))
	// A quiet call still open, its only version a week old.
	f.insert(t, "MemoryNodes", quiet, 7*24*time.Hour, room("open"))

	scheduled := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor(auth.AutomationOriginEngine, "retireDeclaredConceptVersions"))
	nodes, err := integration.sweep(scheduled, map[string]any{}, 0)
	require.NoError(t, err, "the scheduled sweep was refused a room: its tier does not admit the maintenance principal")
	report := sweepReport(t, nodes)
	require.Equal(t, 2, report.Deleted, "only the ended room's two superseded versions may go: %+v", report)
	require.Zero(t, report.Archived, "archive=\"none\": a superseded room version is bookkeeping, not evidence")
	require.Len(t, f.versionsOf(t, ended), 1, "an ended room kept a superseded version past a day")
	require.Len(t, f.versionsOf(t, quiet), 1, "keepLatest: an open room's newest version never expires, however old")
	require.Contains(t, f.invalidated, "v1:calls:room")
}
