package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

const (
	callsOpenRoomLookupMigrationName = "20261119120000_calls_open_room_lookup_index"
	callsOpenRoomLookupIndexName     = "memory_nodes_calls_open_room_lookup_idx"
)

// THE CAPACITY READ TAKES ONLY OPEN ROOMS. Every join, start and queue
// poll counts the occupied seats of a media server's OPEN rooms (callsOpenRoomsOnServer)
// under the server's write gate, and the lookup index 20261118120000 built for that read
// holds every version of every room ever placed on the server: the engine takes each id it
// finds at its latest version and the filter keeps the open ones, so the read was right,
// but what it probed grew with the server's whole history, under the lock every admission
// on the server waits for.
//
// THIS INDEX HOLDS ONLY THE VERSIONS WRITTEN WHILE A ROOM WAS OPEN: the mediaServerId
// lookup, partial on payload status = 'open', which the read requires (isOpen), so the
// engine reads its range through it (component/memql payloadLookups). An index alone cannot
// forget a room, because a room's open versions stay in it for as long as they are stored;
// v1:calls:room keeps only its newest version past a day (@retention, dsl/calls), and an
// ended room's newest version is its ended one, so a room that ended leaves this index
// once its superseded versions expire. The read then costs the server's open rooms and the
// rooms open on it within the last day.
//
// 20261118120000's index is left as it is: it is on main, a migration already applied is
// never edited, and dropping an index on the hypertable takes a lock on every chunk that
// would wait behind every running read. The registry lists this index first, so a read
// that requires an open room is served by it.
//
// A PARTIAL INDEX STILL SCANS THE WHOLE TABLE TO BUILD, as the calls lookup indexes
// record, although few calls rows exist before the first release that carries calls.
func ensureCallsOpenRoomLookupIndex(ctx context.Context, db *bun.DB, logger *slog.Logger, table string) error {
	return ensurePayloadLookupIndex(ctx, db, logger, table, callsOpenRoomLookupIndexName, "mediaServerId",
		[]string{"v1:calls:room"}, lookupEquality{field: "status", value: "open"})
}

func registerCallsOpenRoomLookupIndex(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, callsOpenRoomLookupMigrationName) {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureCallsOpenRoomLookupIndex(ctx, db, logger, memoryNodesTableName)
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(callsOpenRoomLookupIndexName))
		return err
	})
}
