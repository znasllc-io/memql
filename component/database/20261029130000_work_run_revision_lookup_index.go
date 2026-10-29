package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

const (
	workRunRevisionLookupMigrationName = "20261029130000_work_run_revision_lookup_index"
	workRunRevisionLookupIndexName     = "memory_nodes_work_run_revision_lookup_idx"
)

// The Library's document revision reads (workDocumentRevisionRequest and its
// two siblings) name the reviseLibraryDocument automation. Narrowed by the
// owner alone they cost the owner's run history, which for the owner of every
// pipeline run is all of it (2026-10-29). This range is that one automation's
// runs. It is partial on the automation name as well as the concept: an index
// over every automation would let a common one -- a pipeline's -- drive some
// other read with every owner's history, and this one holds only the
// revisions. The engine uses it only for a read that requires that name
// (component/memql payloadLookup.partial).
func ensureWorkRunRevisionLookupIndex(ctx context.Context, db *bun.DB, logger *slog.Logger, table string) error {
	return ensurePayloadLookupIndex(ctx, db, logger, table, workRunRevisionLookupIndexName, "automationName", []string{"v1:work:run"},
		lookupEquality{"automationName", "reviseLibraryDocument"})
}

func registerWorkRunRevisionLookupIndex(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, workRunRevisionLookupMigrationName) {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureWorkRunRevisionLookupIndex(ctx, db, logger, memoryNodesTableName)
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(workRunRevisionLookupIndexName))
		return err
	})
}
