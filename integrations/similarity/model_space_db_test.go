package similarity

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/embedding"
)

type localWidthProvider struct{ calls atomic.Int32 }

func (p *localWidthProvider) Embed(context.Context, string) ([]float32, error) {
	p.calls.Add(1)
	v := make([]float32, 1024)
	v[0] = 1
	return v, nil
}
func (p *localWidthProvider) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for n, text := range texts {
		out[n], _ = p.Embed(ctx, text)
	}
	return out, nil
}
func (*localWidthProvider) Dimensions() int { return 1024 }

func TestLocalWidthStoreSearchAndQueryCacheAcrossReplicas(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "local-width semantic search", dbtest.DSN(), err)
		return
	}
	provider := &localWidthProvider{}
	resolver := func(context.Context, string) (memql.EmbeddingAIProvider, error) { return provider, nil }
	prefix := fmt.Sprintf("local-memory-%d", time.Now().UnixNano())
	concept := "v1:test:" + prefix
	store := embedding.New(slog.Default())
	store.SetDBGetter(func() *sql.DB { return db })
	store.SetEmbeddingProvider(resolver)
	for _, model := range []string{prefix + "-one", prefix + "-two"} {
		id := concept + ":" + model
		_, err := db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,"createdAt","createdBy",schema,payload,metadata,type,concept,provenance) VALUES($1,NOW(),'test','{}',$2::jsonb,'{}','object',$3,'{}')`, id, `{"content":"The preferred format is short paragraphs","domainId":"mine"}`, concept)
		require.NoError(t, err)
		for _, capability := range store.Capabilities() {
			if capability.Name == "store" {
				_, err = capability.Handler(ctx, map[string]any{"nodeId": id, "text": "My preferred format is short paragraphs", "concept": concept, "provider": model}, 0)
				require.NoError(t, err)
			}
		}
		table, err := memql.EmbeddingVectorTable(model, 1024)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE id=$1`, id)
			_, _ = db.ExecContext(context.Background(), `DELETE FROM embedding_cache WHERE provider=$1`, model)
			_, _ = db.ExecContext(context.Background(), `DROP TABLE `+table)
		})
	}
	replica := func() *Integration {
		i := New(nil)
		i.SetDBGetter(func() *sql.DB { return db })
		i.SetEmbeddingProvider(resolver)
		i.SetStagedConceptPredicate(func(string) bool { return false })
		return i
	}
	a, b := replica(), replica()
	args := map[string]any{"text": "How should a report be formatted?", "concept": concept, "provider": prefix + "-one", "domains": []string{"mine"}}
	rows, err := a.similarToHandler(ctx, args, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, concept+":"+prefix+"-one", rows[0].ID, "equal-width models must not share a corpus")
	calls := provider.calls.Load()
	rows, err = b.similarToHandler(ctx, args, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, calls, provider.calls.Load(), "another replica must reuse the persisted 1024-wide query vector")
	args["domains"] = []string{"someone-else"}
	rows, err = b.similarToHandler(ctx, args, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
	_, err = db.ExecContext(ctx, `UPDATE embedding_cache SET expires_at=NOW()-INTERVAL '1 second' WHERE provider=$1`, prefix+"-one")
	require.NoError(t, err)
	_, err = b.similarToHandler(ctx, args, 0)
	require.NoError(t, err)
	require.Equal(t, calls+1, provider.calls.Load())
}
