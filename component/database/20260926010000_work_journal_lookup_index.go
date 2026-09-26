package database

import (
	"context"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
	"log/slog"
)

const workJournalLookupIndexName = "memory_nodes_work_journal_lookup_idx"
const workJournalLookupPredicate = `(concept = ANY (ARRAY['v1:work:step'::text, 'v1:work:approval'::text, 'v1:work:modelCall'::text, 'v1:work:observation'::text]))`
const workJournalLookupFoldedPredicate = `(concept = ANY ('{v1:work:step,v1:work:approval,v1:work:modelCall,v1:work:observation}'::text[]))`

func workJournalLookupMatches(f indexFacts) bool {
	return f.Method == "btree" && f.Partial && f.Expression &&
		(f.Predicate == workJournalLookupPredicate || f.Predicate == workJournalLookupFoldedPredicate) &&
		f.Expressions == `(payload ->> 'runId'::text)` && len(f.KeyColumns) == 4 &&
		f.KeyColumns[0] == "" && f.KeyColumns[1] == "concept" && f.KeyColumns[2] == "id" && f.KeyColumns[3] == "createdAt" &&
		len(f.KeyDescending) == 4 && !f.KeyDescending[0] && !f.KeyDescending[1] && !f.KeyDescending[2] && f.KeyDescending[3]
}

func ensureWorkJournalLookupIndex(ctx context.Context, db *bun.DB, logger *slog.Logger, table string) error {
	return ensureNamedWorkIndex(ctx, db, logger, table, workJournalLookupIndexName, `((payload->>'runId'),concept,id,"createdAt" DESC)`, workJournalLookupPredicate, workJournalLookupMatches)
}
func registerWorkJournalLookupIndex(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, "20260926010000_work_journal_lookup_index") {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureWorkJournalLookupIndex(ctx, db, logger, memoryNodesTableName)
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(workJournalLookupIndexName))
		return err
	})
}
