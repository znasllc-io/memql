package database

import (
	"context"
	"log/slog"
	"slices"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

const retentionGoalIndexName = "memory_nodes_retention_goal_idx"
const retentionTimeIndexName = "memory_nodes_retention_time_idx"
const retentionGoalExpression = "regexp_replace((payload ->> 'goalId'::text), '^v1:work:goal:'::text, ''::text)"

// The same bounded, verified per-chunk build used by recovery. Neither an
// interrupted build nor a root index with missing chunk coverage is accepted.
func ensureRetentionIndexes(ctx context.Context, db *bun.DB, logger *slog.Logger, table string) error {
	if err := ensureNamedWorkIndex(ctx, db, logger, table, retentionGoalIndexName,
		`((regexp_replace(payload->>'goalId', '^v1:work:goal:', '')))`, `(concept = 'v1:work:run'::text)`,
		func(f indexFacts) bool {
			return f.Method == "btree" && f.Partial && f.Predicate == `(concept = 'v1:work:run'::text)` && f.Expression && f.Expressions == retentionGoalExpression && len(f.KeyColumns) == 1 && len(f.KeyDescending) == 1 && !f.KeyDescending[0]
		}); err != nil {
		return err
	}
	return ensureNamedWorkIndex(ctx, db, logger, table, retentionTimeIndexName,
		`(concept,"createdAt",id)`, "", func(f indexFacts) bool {
			return f.Method == "btree" && !f.Partial && f.Predicate == "" && !f.Expression && slices.Equal(f.KeyColumns, []string{"concept", "createdAt", "id"}) && slices.Equal(f.KeyDescending, []bool{false, false, false}) && slices.Equal(f.KeyPlain, []bool{true, true, true})
		})
}

func registerRetentionIndexes(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, "20261013000000_retention_indexes") {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureRetentionIndexes(ctx, db, logger, memoryNodesTableName)
	}, func(ctx context.Context, db *bun.DB) error {
		for _, name := range []string{retentionGoalIndexName, retentionTimeIndexName} {
			if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(name)); err != nil {
				return err
			}
		}
		return nil
	})
}
