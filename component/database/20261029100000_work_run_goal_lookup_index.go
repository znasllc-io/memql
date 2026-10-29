package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

const (
	workRunGoalLookupMigrationName = "20261029100000_work_run_goal_lookup_index"
	workRunGoalLookupIndexName     = "memory_nodes_work_run_goal_lookup_idx"
)

// One goal's runs must not cost its owner's run history. The run owner lookup
// cannot bound workRunsForGoal: production's cluster owner owns every pipeline
// run, so that owner's range is the history, and reading goalId off each
// version in it ran past the eight-second statement limit (2026-10-29). This
// range is one goal's runs. The lookup narrows candidate ids only; the engine
// still checks the goal and the owner on each id's latest version
// (component/memql latestLookupKeys, which also keeps the owner term out of
// this lookup so the planner cannot fall back to the owner index).
func ensureWorkRunGoalLookupIndex(ctx context.Context, db *bun.DB, logger *slog.Logger, table string) error {
	return ensurePayloadLookupIndex(ctx, db, logger, table, workRunGoalLookupIndexName, "goalId", []string{"v1:work:run"})
}

func registerWorkRunGoalLookupIndex(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, workRunGoalLookupMigrationName) {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureWorkRunGoalLookupIndex(ctx, db, logger, memoryNodesTableName)
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(workRunGoalLookupIndexName))
		return err
	})
}
