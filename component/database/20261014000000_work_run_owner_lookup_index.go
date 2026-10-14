package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

const (
	workRunOwnerLookupMigrationName = "20261014000000_work_run_owner_lookup_index"
	workRunOwnerLookupIndexName     = "memory_nodes_work_run_owner_lookup_idx"
)

// Owner-scoped run feeds must not hydrate every system run in the cluster
// before returning one person's page. The lookup narrows candidate IDs only;
// the engine still checks ownership on each ID's latest version.
func ensureWorkRunOwnerLookupIndex(ctx context.Context, db *bun.DB, logger *slog.Logger, table string) error {
	return ensurePayloadLookupIndex(ctx, db, logger, table, workRunOwnerLookupIndexName, "ownerUserId", []string{"v1:work:run"})
}

func registerWorkRunOwnerLookupIndex(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, workRunOwnerLookupMigrationName) {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureWorkRunOwnerLookupIndex(ctx, db, logger, memoryNodesTableName)
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(workRunOwnerLookupIndexName))
		return err
	})
}
