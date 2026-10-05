package knowledge

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
)

func TestKnowledgeModelSpacesAndSourcePurge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "knowledge vector spaces", dbtest.DSN(), err)
		return
	}
	integration := New(slog.Default())
	integration.SetDBGetter(func() *sql.DB { return db })
	key := fmt.Sprintf("knowledge-space-%d", time.Now().UnixNano())
	id := "v1:knowledge:documentChunk:" + key
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE id=$1`, id) })
	_, err := db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,"createdAt","createdBy",schema,payload,metadata,type,concept,provenance) VALUES($1,NOW(),'test','{}',jsonb_build_object('domainId',$2::text,'sourceRef','source'),'{}','object','v1:knowledge:documentChunk','{}')`, id, key)
	require.NoError(t, err)
	vector := make([]float32, 1024)
	vector[0] = 1
	for _, provider := range []string{key + "-one", key + "-two"} {
		table, err := memql.EmbeddingVectorTable(provider, 1024)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+table) })
		require.NoError(t, integration.storeVector(ctx, provider, id, "v1:knowledge:documentChunk", vector))
		has, err := integration.hasVector(ctx, id, provider, 1024)
		require.NoError(t, err)
		require.True(t, has)
	}
	require.NoError(t, integration.purgeChunksForSource(ctx, key, "source"))
	for _, provider := range []string{key + "-one", key + "-two"} {
		has, err := integration.hasVector(ctx, id, provider, 1024)
		require.NoError(t, err)
		require.False(t, has, "all model spaces must retire the source")
	}
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE id=$1`, id).Scan(&count))
	require.Zero(t, count)
}
