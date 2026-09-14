package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

// Verify the MemoryNodes latest-row index on every cluster, including the ones
// that applied 20260913000000 as SQL (memql#5252).
//
// ===========================================================================
// WHY A SECOND MIGRATION RUNS THE SAME ENSURE
// ===========================================================================
// 20260913000000 shipped as a plain `CREATE INDEX IF NOT EXISTS` before it
// became a Go migration, and bun records a migration applied by its version
// alone -- so on every cluster that ran the SQL form, the Go form never runs.
// That SQL form can have succeeded over exactly the index this pair exists to
// refuse: an interrupted transaction_per_chunk build of the same name, which
// IF NOT EXISTS skips with "already exists" (measured), or an index an
// operator put on the name with a different shape. So this runs the same
// ensure once more, on every cluster. A cluster whose index is complete pays
// one catalog inspection; one whose index is not gets it rebuilt; one where an
// operator's equivalent index already answers the read gets no duplicate.
//
// On a fresh install both run, in order, and this one finds complete the index
// 20260913000000 has just built.
//
// ===========================================================================
// THE DOWN IS DELIBERATELY EMPTY
// ===========================================================================
// This creates nothing 20260913000000 does not own: the only index it can
// build is that migration's canonical one, and rolling that migration back
// drops it. Dropping it here too would make rolling back THIS migration alone
// take away an index a still-applied earlier migration is responsible for.
// The down is a function that does nothing, rather than an absent one, so the
// set records that as a decision (bun reports an absent down and a deliberate
// no-op identically unless the function is there).
//
// bun names a Go migration after the FILE its Register call sits in, so the
// call must stay in this file.

// memoryNodesConceptIndexVerifiedMigrationName is this file's name, which is
// the version and comment bun derives for the migration.
const memoryNodesConceptIndexVerifiedMigrationName = "20260914000000_memory_nodes_concept_index_verified"

// registerMemoryNodesConceptIndexVerified adds the migration to the set. It
// MUST be called from this file; see above.
func registerMemoryNodesConceptIndexVerified(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, memoryNodesConceptIndexVerifiedMigrationName) {
		return
	}
	m.MustRegister(
		func(ctx context.Context, db *bun.DB) error {
			_, err := ensureLatestRowIndex(ctx, db, logger, memoryNodesTableName, memoryNodesConceptIndexName)
			return err
		},
		func(context.Context, *bun.DB) error {
			// Nothing to undo -- see "THE DOWN IS DELIBERATELY EMPTY".
			return nil
		},
	)
}
