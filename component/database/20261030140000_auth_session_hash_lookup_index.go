package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

const (
	authSessionHashLookupMigrationName = "20261030140000_auth_session_hash_lookup_index"
	authSessionHashLookupIndexName     = "memory_nodes_auth_session_hash_lookup_idx"
)

// The revocation check must not cost every session. It runs
// authSessionByTokenHash for verified HTTP requests and user stream opens, and
// the identity service's own bearer lookups run the same query. Without an index
// that read collapsed every session to its latest version before comparing the
// digest: 474,699 buffers and 1.1 s at 120,000 sessions in 11 chunks, 4.8 s in 54,
// hit or miss, measured through a real bff, and a gateway request pays it twice. A
// digest names one session, so this range is that session's versions. The lookup
// narrows candidate ids only; the engine still takes each at its latest version,
// where revokedAt lives (component/memql latestLookupKeys).
func ensureAuthSessionHashLookupIndex(ctx context.Context, db *bun.DB, logger *slog.Logger, table string) error {
	return ensurePayloadLookupIndex(ctx, db, logger, table, authSessionHashLookupIndexName, "tokenHash", []string{"v1:identity:authSession"})
}

func registerAuthSessionHashLookupIndex(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, authSessionHashLookupMigrationName) {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureAuthSessionHashLookupIndex(ctx, db, logger, memoryNodesTableName)
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(authSessionHashLookupIndexName))
		return err
	})
}
