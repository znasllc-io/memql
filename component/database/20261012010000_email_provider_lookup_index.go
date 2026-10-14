package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

const (
	emailProviderLookupMigrationName = "20261012010000_email_provider_lookup_index"
	emailProviderLookupIndexName     = "memory_nodes_email_provider_lookup_idx"
)

// Event Grid reports carry Azure's message UUID. Locate its original durable
// receipt without scanning every send operation retained by the installation.
func ensureEmailProviderLookupIndex(ctx context.Context, db *bun.DB, logger *slog.Logger, table string) error {
	return ensurePayloadLookupIndex(ctx, db, logger, table, emailProviderLookupIndexName, "providerMessageId", []string{"v1:email:sendOperation"})
}

func registerEmailProviderLookupIndex(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, emailProviderLookupMigrationName) {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureEmailProviderLookupIndex(ctx, db, logger, memoryNodesTableName)
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(emailProviderLookupIndexName))
		return err
	})
}
