package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

const (
	workRunOwnerWalkMigrationName = "20261029110000_work_run_owner_walk_index"
	workRunOwnerWalkIndexName     = "memory_nodes_work_run_owner_walk_idx"
)

// An owner's newest runs must cost the page, not the owner's history. The run
// owner lookup orders an owner's versions by id, so a newest-first feed --
// Nexus's run list, the Training app's analyses, the procedure learner's
// pages -- learned every id the owner ever had before it could take a page,
// and for the owner of every pipeline run that is the whole run history
// (2026-10-29). This range is the same versions newest first, which the
// engine walks to its page (component/memql latestScanWalk).
//
// It duplicates memory_nodes_work_run_owner_lookup_idx's entries in another
// order, about 100 bytes per run version (measured). The lookup index keeps
// serving the owner reads the walk does not take; dropping it, once this one
// has served production, is a later migration.
func ensureWorkRunOwnerWalkIndex(ctx context.Context, db *bun.DB, logger *slog.Logger, table string) error {
	return ensurePayloadWalkIndex(ctx, db, logger, table, workRunOwnerWalkIndexName, "ownerUserId", []string{"v1:work:run"})
}

func registerWorkRunOwnerWalkIndex(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, workRunOwnerWalkMigrationName) {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureWorkRunOwnerWalkIndex(ctx, db, logger, memoryNodesTableName)
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(workRunOwnerWalkIndexName))
		return err
	})
}
