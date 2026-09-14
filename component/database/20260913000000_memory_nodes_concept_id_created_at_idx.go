package database

import (
	"context"
	"log/slog"
	"strings"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

// Index MemoryNodes on (concept, id, "createdAt" DESC) so a concept read stops
// walking every id (PR #5314; made a verified, bounded build by memql#5252).
//
// ===========================================================================
// WHAT IT FIXES
// ===========================================================================
// Every concept read the executor emits is "the latest row per id WITHIN one
// concept":
//
//	SELECT DISTINCT ON (id) ... FROM "MemoryNodes"
//	WHERE concept = $1 ORDER BY id ASC, "createdAt" DESC
//
// With only (id, "createdAt" DESC) and (concept) to choose from, TimescaleDB
// plans that as a SkipScan over the id index and filters concept per row, so a
// read of one small concept walks EVERY id in the table. On a production
// instance (2026-09-13) the 274-row staleClusterNodes read touched 1,006,847
// rows and 6.9 GB of buffers per call, took 178 s, was issued on every node
// heartbeat by every pod, and exhausted max_connections -- which the edge
// reported as "internal error" on every cold host resolution. The incident
// record is docs/internal/ops/2026-09-13-skipscan-connection-exhaustion.md.
//
// With the composite index the read is bounded to the concept's own rows. The
// planner then reads them either through the composite index, already in
// (id, "createdAt" DESC) order, or through the concept index plus a sort, and
// which of the two turns on random_page_cost: at 1.1 (what timescaledb-tune
// writes for SSDs) it takes the composite index, at the Postgres default of 4
// (which the CNPG presets leave) it takes the other -- both measured on
// TimescaleDB 2.29.2 in latest_row_index_db_test.go, which also pins the plan
// this replaced. TimescaleDB 2.29.2 does NOT SkipScan the composite index
// itself (measured: not even when it is the only index on the table), so
// "the plan skips within one concept" is not what happens; "the plan reads one
// concept, not the table" is.
//
// ===========================================================================
// WHY THIS IS A GO MIGRATION, AND WHY IT KEPT ITS VERSION
// ===========================================================================
// It shipped as SQL twice. First with `WITH (timescaledb.transaction_per_chunk)`,
// which fails the whole migration on a database where the extension is not
// loaded (PR #5353); then as a plain `CREATE INDEX IF NOT EXISTS`, which
// succeeds on both layouts -- and also "succeeds" over an interrupted build of
// the same name, and on a live hypertable holds the write lock on every chunk
// for the whole build. memql#5252 asked for the index the deployment owner
// actually built in production: per chunk on a hypertable, verified rather than
// assumed, on both layouts. SQL cannot choose its statement by whether the
// table is a hypertable without a DO block, and a DO block is exactly where
// that statement must never run: `EXECUTE 'CREATE INDEX ... WITH
// (timescaledb.transaction_per_chunk)'` inside one SEGFAULTS the backend on
// TimescaleDB 2.29.2 (measured: signal 11, "transaction left non-empty SPI
// stack", and the postmaster then reinitializes every connection on the
// server). Nor can SQL report what it found, because pgdriver discards a
// NOTICE. So the migration is Go: ensureLatestRowIndex, in
// latest_row_index.go, which says what it inspects and why.
//
// The SAME 14-digit version, deliberately. bun matches applied migrations on
// the version alone, so a cluster that already applied the SQL form keeps it
// recorded as applied and this Up never runs there, while a cluster that has
// not gets the verified build. The clusters the SQL form already ran on are
// what 20260914000000_memory_nodes_concept_index_verified.go is for.
//
// bun names a Go migration after the FILE its Register call sits in, so the
// call must stay in this file.

const (
	// memoryNodesConceptIndexMigrationName is this file's name, which is the
	// version and comment bun derives for the migration.
	memoryNodesConceptIndexMigrationName = "20260913000000_memory_nodes_concept_id_created_at_idx"

	// memoryNodesConceptIndexName is the canonical latest-row index both
	// concept-index migrations ensure on MemoryNodes.
	memoryNodesConceptIndexName = "memory_nodes_concept_id_created_at_desc_idx"
)

// registerMemoryNodesConceptIndex adds the migration to the set the SQL files
// are discovered into. It MUST be called from this file; see above.
func registerMemoryNodesConceptIndex(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, memoryNodesConceptIndexMigrationName) {
		return
	}
	m.MustRegister(
		func(ctx context.Context, db *bun.DB) error {
			_, err := ensureLatestRowIndex(ctx, db, logger, memoryNodesTableName, memoryNodesConceptIndexName)
			return err
		},
		func(ctx context.Context, db *bun.DB) error {
			// On a hypertable the root's DROP INDEX takes every chunk's index
			// with it.
			_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(memoryNodesConceptIndexName))
			return err
		},
	)
}

// goMigrationRegistered reports whether m already holds the migration named
// by a migration file name. bun's Register appends without looking, so a set
// registered twice would carry -- and run -- a Go migration twice.
func goMigrationRegistered(m *migrate.Migrations, fileName string) bool {
	version := strings.SplitN(fileName, "_", 2)[0]
	for _, existing := range m.Sorted() {
		if existing.Name == version {
			return true
		}
	}
	return false
}
