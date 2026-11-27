package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

// Exercise the real installed query/shape/authorization path on both replicas.
// Old versions must never reappear when the current row leaves this directory.
func TestLibraryBrowsePageCurrentRowsAndReplicaContinuationDB(t *testing.T) {
	f := newRevisionDB(t)
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = db.Close() })
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM "MemoryNodes" WHERE "createdBy"=$1`, f.owner) })
	at := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	put := func(id, concept string, version time.Time, payload map[string]any) {
		t.Helper()
		if payload["ownerUserId"] == nil {
			payload["ownerUserId"] = "v1:identity:user:" + f.owner
		}
		if concept == "v1:library:artifact" {
			payload["lens"], payload["kind"] = "artifact", "file"
		}
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",payload,schema) VALUES ($1,$2,$3,$4,$5,'{}')`, id, concept, version, f.owner, string(body))
		require.NoError(t, err)
	}
	artifact := func(n string) string { return "v1:library:artifact:" + f.owner + "-" + n }
	file := func(n int) string { return fmt.Sprintf("v1:library:file:%s-%d", f.owner, n) }
	for n := 1; n <= 25; n++ {
		put(artifact(fmt.Sprint(n)), "v1:library:artifact", at, map[string]any{"title": fmt.Sprintf("Document %d.pdf", n), "sourceConceptRef": file(n)})
		put(file(n), "v1:library:file", at, map[string]any{"size": n * 100})
	}
	put(artifact("image"), "v1:library:artifact", at.Add(time.Hour), map[string]any{"title": "Image.png"})
	put(artifact("unknown"), "v1:library:artifact", at, map[string]any{"title": "Unknown.pdf", "sourceConceptRef": file(99)})
	put(file(99), "v1:library:file", at, map[string]any{"ownerUserId": "v1:identity:user:someone-else", "size": 999999})
	for _, name := range []string{"archived", "moved", "changed-concept"} {
		put(artifact(name), "v1:library:artifact", at, map[string]any{"title": "Old.pdf", "labels": []string{"old-only"}})
	}
	put(artifact("archived"), "v1:library:artifact", at.Add(time.Minute), map[string]any{"title": "Deleted.pdf", "archived": true})
	put(artifact("moved"), "v1:library:artifact", at.Add(time.Minute), map[string]any{"title": "Private.pdf", "ownerUserId": "v1:identity:user:someone-else"})
	put(artifact("changed-concept"), "v1:library:folder", at.Add(time.Minute), map[string]any{"name": "Changed"})

	type page struct {
		Rows   []map[string]any `json:"rows"`
		Cursor string           `json:"cursor"`
	}
	read := func(i *Integration, args map[string]any) page {
		t.Helper()
		nodes, err := i.handleBrowsePage(f.ctx, args, 0)
		require.NoError(t, err)
		require.Len(t, nodes, 1)
		var out page
		require.NoError(t, json.Unmarshal(nodes[0].Payload, &out))
		require.LessOrEqual(t, len(out.Rows), 10)
		return out
	}
	first := read(f.first, map[string]any{"sort": "name"})
	require.Len(t, first.Rows, 10)
	require.Equal(t, "Document 2.pdf", first.Rows[1]["title"], "natural, not lexical, name order")
	all := append([]map[string]any(nil), first.Rows...)
	token := first.Cursor
	for n := 0; token != ""; n++ {
		require.Less(t, n, 5, "continuation must terminate")
		next := read(f.second, map[string]any{"sort": "name", "cursor": token})
		all, token = append(all, next.Rows...), next.Cursor
	}
	require.Len(t, all, 27)
	seen := map[string]bool{}
	for _, row := range all {
		id := asString(row["id"])
		require.False(t, seen[id], "no duplicate on equal timestamps")
		seen[id] = true
	}
	for _, hidden := range []string{"archived", "moved", "changed-concept"} {
		require.False(t, seen[artifact(hidden)], hidden)
	}
	sizes := read(f.second, map[string]any{"sort": "size"})
	require.Equal(t, artifact("25"), sizes.Rows[0]["id"])
	require.Equal(t, float64(2500), sizes.Rows[0]["browseSize"])
	images := read(f.first, map[string]any{"sort": "newest", "fileType": "images"})
	require.Len(t, images.Rows, 1, "filter the population before taking the page")
	require.Equal(t, artifact("image"), images.Rows[0]["id"])
	recent := read(f.first, map[string]any{"sort": "name", "since": at.Add(30 * time.Minute).Format(time.RFC3339Nano)})
	require.Len(t, recent.Rows, 1)
	bin := read(f.first, map[string]any{"sort": "name", "archived": true})
	require.Len(t, bin.Rows, 1)
	require.Equal(t, artifact("archived"), bin.Rows[0]["id"])
	_, err := f.second.handleBrowsePage(f.ctx, map[string]any{"sort": "size", "cursor": first.Cursor}, 0)
	require.ErrorContains(t, err, "cursor")
	_, err = f.second.handleBrowsePage(revisionActor("someone-else", auth.RoleWriter), map[string]any{"sort": "name", "cursor": first.Cursor}, 0)
	require.ErrorContains(t, err, "cursor")
	_, err = f.second.handleBrowsePage(context.Background(), nil, 0)
	require.ErrorContains(t, err, "authenticated")
	canceled, cancel := context.WithCancel(f.ctx)
	cancel()
	_, err = f.first.handleBrowsePage(canceled, map[string]any{"sort": "name"}, 0)
	require.Error(t, err)
	// The public DSL builtin uses the same bounded envelope as direct dispatch.
	wire := f.query(f.engine, f.ctx, "builtin", "libraryBrowsePage", map[string]any{"sort": "name"})
	require.Len(t, wire, 1)
	require.Len(t, wire[0]["rows"], 10)
}
