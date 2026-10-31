package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

// THE ACCESS SURFACE'S LOOKUP INDEXES (design record
// 2026-10-29-deployable-access-codes, section 6). A visitor's cookie finds its
// session by the token's digest and a typed code finds its row by the code's,
// and without these each of those reads enumerates every id of its concept.
// Measured on a session table of 300,000 rows: 1.37 s per lookup, and sessions
// stay 30 days, so one code holder could grow the cost at will while anybody at
// all could spend it. The third is what "last opened" reads, once per code in
// the Access codes panel.
//
// A digest names one row, so each range here holds one row's versions. `codeId`
// holds one code's sessions, which is what one code has opened in 30 days.
//
// A PARTIAL INDEX STILL SCANS THE WHOLE TABLE TO BUILD. The predicate decides
// what goes in, not what is read, so each of these reads every chunk's heap
// although neither concept has a row before this release. Writes to the chunk
// being built wait, and the hot chunk is the one that matters. See
// docs/public/operate/site-access-codes.md, "The indexes", for the rollout note.
const siteAccessLookupMigrationName = "20261030000000_site_access_lookup_indexes"

var siteAccessLookupIndexes = []struct{ name, field, concept string }{
	{"memory_nodes_access_tokenhash_lookup_idx", "tokenHash", "v1:platform:siteAccessSession"},
	{"memory_nodes_access_codeid_lookup_idx", "codeId", "v1:platform:siteAccessSession"},
	{"memory_nodes_access_codehash_lookup_idx", "codeHash", "v1:platform:siteAccessCode"},
}

func ensureSiteAccessLookupIndexes(ctx context.Context, db *bun.DB, logger *slog.Logger, table string) error {
	for _, index := range siteAccessLookupIndexes {
		if err := ensurePayloadLookupIndex(ctx, db, logger, table, index.name, index.field, []string{index.concept}); err != nil {
			return err
		}
	}
	return nil
}

func registerSiteAccessLookupIndexes(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, siteAccessLookupMigrationName) {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureSiteAccessLookupIndexes(ctx, db, logger, memoryNodesTableName)
	}, func(ctx context.Context, db *bun.DB) error {
		for _, index := range siteAccessLookupIndexes {
			if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(index.name)); err != nil {
				return err
			}
		}
		return nil
	})
}
