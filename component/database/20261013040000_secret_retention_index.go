package database

import (
	"context"
	"log/slog"
	"slices"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

func registerSecretRetentionIndex(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, "20261013040000_secret_retention_index") {
		return
	}
	const name = "secret_memory_nodes_retention_time_idx"
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureNamedWorkIndex(ctx, db, logger, secretMemoryNodesTableName, name, `(concept,"createdAt",id)`, "", func(f indexFacts) bool {
			return f.Method == "btree" && !f.Partial && f.Predicate == "" && !f.Expression && slices.Equal(f.KeyColumns, []string{"concept", "createdAt", "id"}) && slices.Equal(f.KeyDescending, []bool{false, false, false}) && slices.Equal(f.KeyPlain, []bool{true, true, true})
		})
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(name))
		return err
	})
}
