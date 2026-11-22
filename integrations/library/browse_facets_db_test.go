package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
)

func TestLibraryBrowseFacetsWalksEveryAuthorizedPageDB(t *testing.T) {
	f := newRevisionDB(t)
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = db.Close() })
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE id LIKE $1`, "v1:library:artifact:"+f.owner+"-facet-%")
	})
	_, err := db.ExecContext(context.Background(), `INSERT INTO "MemoryNodes" (id,concept,type,"createdBy","createdAt",payload,schema,metadata,provenance) SELECT 'v1:library:artifact:'||$1||'-facet-'||n,'v1:library:artifact','object','test-list-rollup',clock_timestamp(),jsonb_build_object('ownerUserId','v1:identity:user:'||$1,'lens','artifact','folderId',CASE n%2 WHEN 0 THEN 'v1:library:folder:one' ELSE 'v1:library:folder:two' END,'labels',CASE n WHEN 0 THEN '["oldest-only"]'::jsonb ELSE '["shared"]'::jsonb END),'{"$id":"v1:library:artifact"}'::jsonb,'{}'::jsonb,'{}'::jsonb FROM generate_series(0,600) n`, f.owner)
	require.NoError(t, err)
	// A cluster owner's wider row admission must not widen the query's explicit owner scope.
	_, err = db.ExecContext(context.Background(), `INSERT INTO "MemoryNodes" (id,concept,type,"createdBy","createdAt",payload,schema,metadata,provenance) VALUES ($1,'v1:library:artifact','object','test-list-rollup',clock_timestamp(),jsonb_build_object('ownerUserId','other-owner','lens','artifact','folderId','foreign','labels','["foreign"]'::jsonb),'{"$id":"v1:library:artifact"}'::jsonb,'{}'::jsonb,'{}'::jsonb)`, "v1:library:artifact:"+f.owner+"-facet-foreign")
	require.NoError(t, err)
	start := time.Now()
	result, err := f.first.handleBrowseFacets(f.ctx, map[string]any{"label": "shared"}, 0)
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Len(t, result, 1)
	var got struct {
		Folders []struct {
			FolderID string `json:"folderId"`
			Count    int    `json:"count"`
		} `json:"folders"`
		LabelGroups [][]string `json:"labelGroups"`
	}
	require.NoError(t, json.Unmarshal(result[0].Payload, &got))
	require.Len(t, got.Folders, 2)
	require.Equal(t, "v1:library:folder:one", got.Folders[0].FolderID)
	require.Equal(t, 301, got.Folders[0].Count)
	require.Equal(t, "v1:library:folder:two", got.Folders[1].FolderID)
	require.Equal(t, 300, got.Folders[1].Count)
	require.Equal(t, [][]string{{"oldest-only", "shared"}}, got.LabelGroups)
	// Read via the second replica too; the aggregate has no replica-local state.
	second, err := f.second.handleBrowseFacets(f.ctx, map[string]any{"folderId": "one"}, 0)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(second[0].Payload, &got))
	require.Len(t, got.Folders, 1)
	require.Equal(t, 301, got.Folders[0].Count)
	_, err = f.first.handleBrowseFacets(context.Background(), nil, 0)
	require.ErrorContains(t, err, "authenticated")
	t.Logf("library facets fixture=601 owned+1 foreign rows complete_count=601 aggregate_wire_bytes=%d native_elapsed_us=%d", len(result[0].Payload), elapsed.Microseconds())
	// The generated builtin keeps the same API name when replacing the former logic.
	wire, err := f.engine.Execute(f.ctx, "builtin libraryBrowseFacets()")
	require.NoError(t, err)
	require.NotEmpty(t, memql.MaterializeRows(wire.OutputPayload()))
}

// Files sends an empty list when nothing can match: the Materializer place
// before anything was made, and the Desktop place's folder list whenever it is
// not searching. Each used to fail the read ("collection literal cannot be
// empty"); an empty list now matches nothing (executor_filter.go
// emptyListMembership), so the counts read answers with no rows instead.
func TestLibraryBrowseFacetsAcceptsEmptyListsDB(t *testing.T) {
	f := newRevisionDB(t)
	for name, args := range map[string]map[string]any{
		"materializer, nothing made":    {"archived": false, "sourceRefs": []any{}},
		"desktop, files but no folders": {"archived": false, "ids": []any{"v1:library:artifact:none"}, "folderIds": []any{}},
		"desktop, empty":                {"archived": false, "ids": []any{}, "folderIds": []any{}},
	} {
		result, err := f.first.handleBrowseFacets(f.ctx, args, 0)
		require.NoError(t, err, name)
		require.Len(t, result, 1, name)
		var got struct {
			Folders []any `json:"folders"`
		}
		require.NoError(t, json.Unmarshal(result[0].Payload, &got), name)
		require.Empty(t, got.Folders, name)
	}
}
