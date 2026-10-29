package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

const (
	workRunSignatureLookupMigrationName = "20261029120000_work_run_signature_lookup_index"
	workRunSignatureLookupIndexName     = "memory_nodes_work_run_signature_lookup_idx"
)

// The procedure corpus reads one goal signature's SUCCEEDED runs
// (workRunsForOwnerGoalSignature). Narrowed by the owner alone that read cost
// the owner's run history, which for the owner of every pipeline run is all of
// it (2026-10-29). This range is one signature's succeeded versions. It is
// partial on the status as well as the concept, so a run that finished once
// is one entry here rather than one per step boundary, and the index stays
// small. The engine uses it only for a read that requires that status
// (component/memql payloadLookup.partial).
func ensureWorkRunSignatureLookupIndex(ctx context.Context, db *bun.DB, logger *slog.Logger, table string) error {
	return ensurePayloadLookupIndex(ctx, db, logger, table, workRunSignatureLookupIndexName, "goalSignature", []string{"v1:work:run"},
		lookupEquality{"status", "succeeded"})
}

func registerWorkRunSignatureLookupIndex(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, workRunSignatureLookupMigrationName) {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureWorkRunSignatureLookupIndex(ctx, db, logger, memoryNodesTableName)
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(workRunSignatureLookupIndexName))
		return err
	})
}
