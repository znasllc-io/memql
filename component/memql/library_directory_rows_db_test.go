package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLibraryDirectoryReductionAppliesStagedSourceAdmission(t *testing.T) {
	e, db, _ := readMergeTestEngine(t) // staging state belongs to this engine only
	owner := uniqueSuffix("directory-staged")
	ctx := clusterOwnerCtx(owner)
	_, err := db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,concept,"createdBy","createdAt",payload,schema)
VALUES (?,'v1:library:artifact',?,now(),jsonb_build_object('ownerUserId','v1:identity:user:'||?,'lens','artifact','kind','file','title','Visible.pdf'),'{}')`, "v1:library:artifact:"+owner, owner, owner)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE "createdBy"=?`, owner)
	})
	rows, err := e.LibraryDirectoryRows(ctx, "libraryArtifactsBrowse", nil)
	require.NoError(t, err)
	require.Len(t, rows, 1, "positive control before staging")
	e.markConceptDataStaged("v1:library:artifact")
	rows, err = e.LibraryDirectoryRows(ctx, "libraryArtifactsBrowse", nil)
	require.NoError(t, err)
	require.Empty(t, rows, "no staged rows may enter native counts or sorted pages")
	e.clearConceptDataStaging("v1:library:artifact")
	rows, err = e.LibraryDirectoryRows(ctx, "libraryArtifactsBrowse", nil)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	_, err = e.LibraryDirectoryRows(ctx, "allWorkersWithStatus", nil)
	require.ErrorContains(t, err, "unsupported")
	_, err = e.LibraryDirectoryRows(context.Background(), "libraryArtifactsBrowse", nil)
	require.ErrorContains(t, err, "authenticated")
}
