package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

// Folder and run reads must enumerate the matching IDs, not every artifact
// the installation has ever stored. These indexes retain all versions; the
// executor always checks the current row again after finding a candidate.
func registerLibraryLookupIndexes(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, "20261127010000_library_lookup_indexes") {
		return
	}
	indexes := []struct{ name, field string }{
		{"memory_nodes_library_folder_lookup_idx", "folderId"},
		{"memory_nodes_library_run_lookup_idx", "producedByRunId"},
	}
	m.MustRegister(func(ctx context.Context, db *bun.DB) error {
		for _, index := range indexes {
			if err := ensurePayloadLookupIndex(ctx, db, logger, memoryNodesTableName, index.name, index.field, []string{"v1:library:artifact"}); err != nil {
				return err
			}
		}
		// Match the Files value-key cursor's bytewise ID order, in both
		// directions. The generic run walk uses a different tie-break.
		return ensureNamedWorkIndex(ctx, db, logger, memoryNodesTableName, "memory_nodes_library_owner_page_idx",
			`((payload->>'ownerUserId'),concept,"createdAt" DESC,id COLLATE "C" DESC)`,
			`concept = 'v1:library:artifact'`, libraryPageIndexMatches)
	}, func(ctx context.Context, db *bun.DB) error {
		for _, index := range indexes {
			if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(index.name)); err != nil {
				return err
			}
		}
		_, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS memory_nodes_library_owner_page_idx`)
		return err
	})
}

func libraryPageIndexMatches(f indexFacts) bool {
	return f.Method == "btree" && f.Partial && f.Expression && (f.Predicate == "(concept = 'v1:library:artifact'::text)") &&
		f.Expressions == "(payload ->> 'ownerUserId'::text)" && len(f.KeyColumns) == 4 &&
		f.KeyColumns[0] == "" && f.KeyColumns[1] == "concept" && f.KeyColumns[2] == "createdAt" && f.KeyColumns[3] == "id" &&
		len(f.KeyDescending) == 4 && !f.KeyDescending[0] && !f.KeyDescending[1] && f.KeyDescending[2] && f.KeyDescending[3] &&
		len(f.KeyCollations) == 4 && f.KeyCollations[3] == "C" && len(f.KeyNullsFirst) == 4 && f.KeyNullsFirst[2] && f.KeyNullsFirst[3]
}
