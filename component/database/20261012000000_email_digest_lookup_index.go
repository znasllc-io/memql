package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

// THE ADDRESS-DIGEST LOOKUP INDEXES. Campaigns finds a subscriber by the
// digest of their normalized mailbox -- an audience's recipient row at signup,
// and the consent events that answer "status, date, source" -- and a digest
// never changes once written, so every version carrying one is a version of a
// row that still carries it. Without an index each of those reads enumerated
// every id of its concept (measured at 200,000 recipients: about a second per
// lookup, and a signup re-checks under its lock).
//
// One index PER CONCEPT rather than one over both: ensureNamedWorkIndex refuses
// an index whose definition differs from the one it builds, so a combined
// index could never take another concept without a new name, while a concept
// added later is one more index beside these.
const (
	emailDigestLookupMigrationName = "20261012000000_email_digest_lookup_index"
	recipientDigestLookupIndexName = "memory_nodes_recipient_digest_lookup_idx"
	consentDigestLookupIndexName   = "memory_nodes_consent_digest_lookup_idx"
)

var emailDigestLookupIndexes = []struct{ name, concept string }{
	{recipientDigestLookupIndexName, "v1:campaigns:recipient"},
	{consentDigestLookupIndexName, "v1:campaigns:consentEvent"},
}

func ensureEmailDigestLookupIndexes(ctx context.Context, db *bun.DB, logger *slog.Logger, table string) error {
	for _, index := range emailDigestLookupIndexes {
		if err := ensurePayloadLookupIndex(ctx, db, logger, table, index.name, "emailDigest", []string{index.concept}); err != nil {
			return err
		}
	}
	return nil
}

func registerEmailDigestLookupIndexes(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, emailDigestLookupMigrationName) {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureEmailDigestLookupIndexes(ctx, db, logger, memoryNodesTableName)
	}, func(ctx context.Context, db *bun.DB) error {
		for _, index := range emailDigestLookupIndexes {
			if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(index.name)); err != nil {
				return err
			}
		}
		return nil
	})
}
