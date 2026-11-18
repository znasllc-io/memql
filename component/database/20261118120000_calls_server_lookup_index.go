package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

const (
	callsServerLookupMigrationName = "20261118120000_calls_server_lookup_index"
	callsServerLookupIndexName     = "memory_nodes_calls_media_server_lookup_idx"
)

// THE CALLS CAPABILITY'S CAPACITY READ. A media server's seats are shared by every call
// placed on it, so every join, start and queue poll counts the occupied seats of the
// server's OPEN ROOMS (callsOpenRoomsOnServer), under the server's write gate. Without an
// index that read enumerates every room of the concept at its latest version, on every
// one of those requests, while the server's lock is held. The seats and queue entries of
// the rooms it finds are read through the room lookup index the calls lookup migration
// already builds (20261117120000), so this one index is the whole of the change.
//
// THE RANGE IS EVERY ROOM EVER PLACED ON THE SERVER, not only the open ones: the index
// holds every version, and a room's versions all carry the server it was placed on. The
// engine takes each id at its latest version and the filter keeps the open ones
// (component/memql latestLookupKeys), so the read is right; what grows with the server's
// history is the number of ids it probes.
//
// A PARTIAL INDEX STILL SCANS THE WHOLE TABLE TO BUILD, as the calls lookup indexes
// record, although few calls rows exist before this release.
func ensureCallsServerLookupIndex(ctx context.Context, db *bun.DB, logger *slog.Logger, table string) error {
	return ensurePayloadLookupIndex(ctx, db, logger, table, callsServerLookupIndexName, "mediaServerId", []string{"v1:calls:room"})
}

func registerCallsServerLookupIndex(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, callsServerLookupMigrationName) {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureCallsServerLookupIndex(ctx, db, logger, memoryNodesTableName)
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(callsServerLookupIndexName))
		return err
	})
}
