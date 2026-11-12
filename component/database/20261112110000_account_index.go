package database

import (
	"context"
	"log/slog"
	"slices"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

const (
	accountIndexMigrationName = "20261112110000_account_index"
	accountIndexName          = "idx_memorynodes_account"
	// accountIndexPredicate is what the build writes. The rows with no account
	// are left out on purpose: before the assignment job has run they are most
	// of the table, and nothing looks a row up by "no account" -- the job walks
	// a concept's ids, and the inventory and export ask for one account.
	accountIndexPredicate = `(payload->>'accountId') IS NOT NULL AND (payload->>'accountId') <> ''`
	// accountIndexDeparsed is the same predicate as PostgreSQL prints it back
	// from the catalog, which is what an existing index is compared against.
	accountIndexDeparsed   = `(((payload ->> 'accountId'::text) IS NOT NULL) AND ((payload ->> 'accountId'::text) <> ''::text))`
	accountIndexExpression = `(payload ->> 'accountId'::text)`
)

// ensureAccountIndex builds the index that looks rows up by account
// (design record docs/superpowers/specs/2026-11-12-account-row-ownership-design.md,
// section F): ((payload->>'accountId'), concept), partial on a present,
// non-empty account. An account's inventory, and later its export, read "every
// row of this account, concept by concept", which is this index's leading
// range.
//
// Built and verified through ensureNamedWorkIndex, as every other node-table
// index is, and for its reasons (latest_row_index.go): the session build lock,
// so two nodes migrating at once build it once; on a hypertable one
// transaction per chunk, so a write waits on one chunk at a time and never on
// the whole table; and a re-inspection of every chunk before the migration
// reports success, because an interrupted per-chunk build leaves an index of
// the right name that the planner cannot use.
func ensureAccountIndex(ctx context.Context, db *bun.DB, logger *slog.Logger, table string) error {
	return ensureNamedWorkIndex(ctx, db, logger, table, accountIndexName,
		`((payload->>'accountId'),concept)`, accountIndexPredicate, accountIndexMatches)
}

func accountIndexMatches(f indexFacts) bool {
	return f.Method == "btree" && f.Partial && f.Expression &&
		f.Predicate == accountIndexDeparsed && f.Expressions == accountIndexExpression &&
		slices.Equal(f.KeyColumns, []string{"", "concept"}) &&
		slices.Equal(f.KeyDescending, []bool{false, false})
}

func registerAccountIndex(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, accountIndexMigrationName) {
		return
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return ensureAccountIndex(ctx, db, logger, memoryNodesTableName)
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(accountIndexName))
		return err
	})
}
