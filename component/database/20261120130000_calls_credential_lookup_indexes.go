package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

const callsCredentialLookupMigrationName = "20261120130000_calls_credential_lookup_indexes"

// An anonymous assertion resolves one public credential, never every account.
// Credential management similarly reads only the authenticated key's rows.
var callsCredentialLookupIndexes = []struct {
	name, field string
	concepts    []string
}{
	{"memory_nodes_calls_credential_digest_lookup_idx", "credentialDigest", []string{"v1:calls:credential"}},
	{"memory_nodes_calls_credential_key_lookup_idx", "keyId", []string{"v1:calls:credential"}},
	{"memory_nodes_calls_seat_credential_lookup_idx", "credentialId", []string{"v1:calls:seat"}},
}

func registerCallsCredentialLookupIndexes(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, callsCredentialLookupMigrationName) {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		for _, index := range callsCredentialLookupIndexes {
			if err := ensurePayloadLookupIndex(ctx, db, logger, memoryNodesTableName, index.name, index.field, index.concepts); err != nil {
				return err
			}
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		for _, index := range callsCredentialLookupIndexes {
			if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(index.name)); err != nil {
				return err
			}
		}
		return nil
	})
}
