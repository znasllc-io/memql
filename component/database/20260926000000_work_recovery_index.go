package database

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

const workRecoveryIndexName = "memory_nodes_work_recovery_idx"

// Keep this predicate identical to the recovery reader. It indexes version
// keys, not JSON payloads: old running versions are rejected using the existing
// latest-row index before a bounded page of current payloads is loaded.
const workRecoveryPredicate = `((concept = 'v1:work:run'::text) AND (COALESCE((payload ->> 'status'::text), ''::text) <> ALL (ARRAY['succeeded'::text, 'failed'::text, 'cancelled'::text, 'abandoned'::text])))`

// Timescale folds constant arrays when copying a predicate to a chunk.
const workRecoveryFoldedPredicate = `((concept = 'v1:work:run'::text) AND (COALESCE((payload ->> 'status'::text), ''::text) <> ALL ('{succeeded,failed,cancelled,abandoned}'::text[])))`

func registerWorkRecoveryIndex(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, "20260926000000_work_recovery_index") {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureWorkRecoveryIndex(ctx, db, logger, memoryNodesTableName)
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(workRecoveryIndexName))
		return err
	})
}

func workRecoveryIndexMatches(f indexFacts) bool {
	return f.Method == "btree" && f.Partial && !f.Expression &&
		(f.Predicate == workRecoveryPredicate || f.Predicate == workRecoveryFoldedPredicate) &&
		len(f.KeyColumns) == 2 && f.KeyColumns[0] == "createdAt" && f.KeyColumns[1] == "id" &&
		len(f.KeyDescending) == 2 && !f.KeyDescending[0] && !f.KeyDescending[1] &&
		len(f.KeyPlain) == 2 && f.KeyPlain[0] && f.KeyPlain[1]
}

func workRecoveryIndexComplete(st latestRowIndexState) bool {
	root := false
	for _, f := range st.Root {
		if f.Name == workRecoveryIndexName && workRecoveryIndexMatches(f) && f.usableProblem() == "" {
			root = true
		}
	}
	if !root {
		return false
	}
	for _, c := range st.Chunks {
		// The existing index inspector identifies compressed/foreign chunks.
		// Recovery does not require a physical index where storage cannot carry one.
		if c.Exempt != "" {
			continue
		}
		covered := false
		for _, f := range c.Indexes {
			if workRecoveryIndexMatches(f) && f.usableProblem() == "" {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

// Reuse the established session lock, orphan-build handling and bounded DROP
// wait. A Timescale build runs one transaction per chunk; PostgreSQL's plain
// table build is atomic. Re-inspect every chunk instead of trusting an index
// name left by an interrupted build.
func ensureWorkRecoveryIndex(ctx context.Context, db *bun.DB, logger *slog.Logger, table string) error {
	if logger == nil {
		logger = slog.Default()
	}
	s := &latestRowIndexSession{db: db, logger: logger, table: table, key: latestRowIndexLockKey(table)}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.close()
	var st latestRowIndexState
	for {
		var err error
		st, err = readLatestRowIndexState(ctx, s.conn, table)
		if err != nil {
			return err
		}
		if workRecoveryIndexComplete(st) {
			return nil
		}
		builds, err := otherIndexBuilds(ctx, s.conn, st)
		if err != nil {
			return err
		}
		if len(builds) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(latestRowIndexPollMax):
		}
	}
	for _, f := range st.Root {
		if f.Name != workRecoveryIndexName {
			continue
		}
		if !workRecoveryIndexMatches(f) {
			return fmt.Errorf("work recovery index %s has an unexpected definition: %s", f.Name, f.Definition)
		}
		if err := s.drop(ctx, st, workRecoveryIndexName); err != nil {
			return err
		}
	}
	stmt := `CREATE INDEX ` + quoteIdentifier(workRecoveryIndexName) + ` ON ` + st.qualifiedTable() + ` ("createdAt", id)`
	if st.Hypertable {
		stmt += ` WITH (timescaledb.transaction_per_chunk)`
	}
	stmt += ` WHERE ` + workRecoveryPredicate
	if err := s.ddl(ctx, stmt); err != nil {
		return err
	}
	after, err := readLatestRowIndexState(ctx, s.conn, table)
	if err != nil {
		return err
	}
	if !workRecoveryIndexComplete(after) {
		return fmt.Errorf("work recovery index %s is incomplete after its build", workRecoveryIndexName)
	}
	logger.Info("work recovery index verified", "index", workRecoveryIndexName, "chunks", len(after.Chunks))
	return nil
}
