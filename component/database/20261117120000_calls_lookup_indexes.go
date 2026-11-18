package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

// THE CALLS CAPABILITY'S DIGEST LOOKUPS. A caller presents a secret (a key, a
// pass, a host or guest link) and the integration finds the row by the secret's
// SHA-256 digest, so a stranger's request is one range read and never an
// enumeration of the concept. A digest names one row, so each range holds one
// row's versions. Only digests are indexed: no plain secret is ever stored.
//
// A PARTIAL INDEX STILL SCANS THE WHOLE TABLE TO BUILD, as the site access
// indexes record: the build reads every chunk's heap once, at the rollout that
// brings it, although no calls row exists before this release.
const callsLookupMigrationName = "20261117120000_calls_lookup_indexes"

var callsLookupIndexes = []struct {
	name, field string
	concepts    []string
}{
	{"memory_nodes_calls_key_digest_lookup_idx", "digest", []string{"v1:calls:key"}},
	{"memory_nodes_calls_session_digest_lookup_idx", "tokenDigest", []string{"v1:calls:session"}},
	{"memory_nodes_calls_guest_digest_lookup_idx", "guestDigest", []string{"v1:calls:room", "v1:calls:scheduledCall"}},
	{"memory_nodes_calls_host_digest_lookup_idx", "hostDigest", []string{"v1:calls:room", "v1:calls:scheduledCall"}},
}

func ensureCallsLookupIndexes(ctx context.Context, db *bun.DB, logger *slog.Logger, table string) error {
	for _, index := range callsLookupIndexes {
		if err := ensurePayloadLookupIndex(ctx, db, logger, table, index.name, index.field, index.concepts); err != nil {
			return err
		}
	}
	return nil
}

func registerCallsLookupIndexes(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, callsLookupMigrationName) {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureCallsLookupIndexes(ctx, db, logger, memoryNodesTableName)
	}, func(ctx context.Context, db *bun.DB) error {
		for _, index := range callsLookupIndexes {
			if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(index.name)); err != nil {
				return err
			}
		}
		return nil
	})
}
