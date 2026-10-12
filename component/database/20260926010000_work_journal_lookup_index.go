package database

import (
	"context"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
	"log/slog"
)

const workJournalLookupMigrationName = "20260926010000_work_journal_lookup_index"
const workJournalLookupIndexName = "memory_nodes_work_journal_lookup_idx"

// The first payload lookup index (payload_lookup_index.go): the rendering there
// reproduces this index's original DDL and deparsed predicates exactly.
var workJournalLookupConcepts = []string{"v1:work:step", "v1:work:approval", "v1:work:modelCall", "v1:work:observation"}

func ensureWorkJournalLookupIndex(ctx context.Context, db *bun.DB, logger *slog.Logger, table string) error {
	return ensurePayloadLookupIndex(ctx, db, logger, table, workJournalLookupIndexName, "runId", workJournalLookupConcepts)
}
func registerWorkJournalLookupIndex(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, workJournalLookupMigrationName) {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureWorkJournalLookupIndex(ctx, db, logger, memoryNodesTableName)
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(workJournalLookupIndexName))
		return err
	})
}
