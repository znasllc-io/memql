package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

const (
	inboundFailedLookupMigrationName = "20261111120000_inbound_failed_lookup_index"
	inboundFailedLookupIndexName     = "memory_nodes_inbound_failed_lookup_idx"
)

// THE RETRY PASS READS THE FAILED DELIVERIES, NOT THE INBOX (store mirror, U4).
// Every webhook delivery is a v1:platform:inboundRequest row carrying its whole
// body, an installation keeps them, and an unfiltered read of the concept times
// out. The datasync retry pass asks which failed deliveries are due every
// minute (inboundRequestsDueForRetry), and without an index that read collapsed
// every delivery to its latest version and read its payload, body and all, to
// keep a handful. This range is the versions on which a delivery was failed:
// partial on the status as well as the concept, so a delivery that never
// failed is not in it at all, and one that failed is a few entries -- its
// failed tries and the pass's claims. The engine uses it only for a read that
// requires that status (component/memql payloadLookup.partial).
//
// A PARTIAL INDEX STILL SCANS THE WHOLE TABLE TO BUILD, as the site access
// indexes record: the build reads every chunk's heap once, at the rollout that
// brings it.
func ensureInboundFailedLookupIndex(ctx context.Context, db *bun.DB, logger *slog.Logger, table string) error {
	return ensurePayloadLookupIndex(ctx, db, logger, table, inboundFailedLookupIndexName, "status", []string{"v1:platform:inboundRequest"},
		lookupEquality{"status", "failed"})
}

func registerInboundFailedLookupIndex(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, inboundFailedLookupMigrationName) {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureInboundFailedLookupIndex(ctx, db, logger, memoryNodesTableName)
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(inboundFailedLookupIndexName))
		return err
	})
}
