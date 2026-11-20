package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

const callsHistoryLookupMigrationName = "20261120140000_calls_history_lookup_index"
const callsHistoryLookupIndexName = "memory_nodes_calls_history_key_lookup_idx"

// History and its joined-seat repair are always scoped to one authenticated
// key. Room and booking source reads already have their own key lookup index.
func registerCallsHistoryLookupIndex(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, callsHistoryLookupMigrationName) {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensurePayloadLookupIndex(ctx, db, logger, memoryNodesTableName, callsHistoryLookupIndexName, "keyId", []string{"v1:calls:history", "v1:calls:seat"})
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(callsHistoryLookupIndexName))
		return err
	})
}
