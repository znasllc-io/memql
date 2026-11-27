package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLibraryChronologicalPagesKeepCurrentVersions(t *testing.T) {
	e, db, _ := sharedReadMergeEngine(t)
	owner := uniqueSuffix("library-current")
	ctx := clusterOwnerCtx(owner)
	background := context.Background()
	_, err := db.ExecContext(background, `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",payload,schema)
SELECT 'v1:library:artifact:'||?||'-'||n, 'v1:library:artifact', TIMESTAMPTZ '2026-02-01', ?,
jsonb_build_object('ownerUserId','v1:identity:user:'||?, 'lens','artifact','kind','file','title','File '||n,'folderId','v1:library:folder:before','producedByRunId','v1:work:run:before'), '{}'
FROM generate_series(0,34) n`, owner, owner, owner)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.ExecContext(background, `DELETE FROM "MemoryNodes" WHERE "createdBy"=?`, owner) })
	// Eleven ownership changes, a moved folder/run, an archive and a changed
	// concept leave historical matches that must not leak into any page.
	for n := 0; n < 14; n++ {
		id := fmt.Sprintf("v1:library:artifact:%s-%d", owner, n)
		payload := map[string]any{"ownerUserId": "v1:identity:user:" + owner, "lens": "artifact", "kind": "file"}
		concept := "v1:library:artifact"
		switch {
		case n < 11:
			payload["ownerUserId"] = "v1:identity:user:other"
		case n == 11:
			payload["folderId"], payload["producedByRunId"] = "v1:library:folder:after", "v1:work:run:after"
		case n == 12:
			payload["archived"] = true
		case n == 13:
			concept = "v1:library:folder"
		}
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		_, err = db.ExecContext(background, `INSERT INTO "MemoryNodes" (id,concept,"createdBy","createdAt",payload,schema) VALUES (?,?,?,?,?,'{}')`, id, concept, owner, time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC), string(body))
		require.NoError(t, err)
	}
	for _, query := range []string{`query libraryArtifactsBrowse()`, `query libraryArtifactsBrowseOldest()`} {
		seen := map[string]bool{}
		cursor := ""
		for n := 0; ; n++ {
			require.Less(t, n, 5)
			page, err := e.Execute(ContextWithCursor(ContextWithFreshRead(ctx), cursor), query)
			require.NoError(t, err)
			for _, id := range pageIDs(t, page) {
				require.False(t, seen[id], "duplicate continuation")
				seen[id] = true
			}
			cursor = page.GetMeta().Cursor
			if cursor == "" {
				break
			}
		}
		require.Len(t, seen, 22, query)
		for n := 0; n < 14; n++ {
			require.Equal(t, n == 11, seen[fmt.Sprintf("v1:library:artifact:%s-%d", owner, n)])
		}
	}
	for _, query := range []string{`query artifactsForRun(runId: "before")`, `query libraryArtifactsBrowse(folderId: "before")`} {
		result, err := e.Execute(ContextWithFreshRead(ctx), query)
		require.NoError(t, err)
		require.NotContains(t, pageIDs(t, result), fmt.Sprintf("v1:library:artifact:%s-11", owner))
	}
}
