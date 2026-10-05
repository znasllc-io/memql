package similarity

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
)

type cachedQueryProvider struct {
	calls      atomic.Int32
	dimensions int
}

func (p *cachedQueryProvider) Embed(context.Context, string) ([]float32, error) {
	p.calls.Add(1)
	v := make([]float32, p.Dimensions())
	v[0] = 1
	return v, nil
}
func (p *cachedQueryProvider) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for n, text := range texts {
		out[n], _ = p.Embed(ctx, text)
	}
	return out, nil
}
func (p *cachedQueryProvider) Dimensions() int {
	if p.dimensions > 0 {
		return p.dimensions
	}
	return 1536
}

func TestSimilarityQueryReusesSharedEmbeddingUntilExpiryOrBindingChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "similarity embedding cache", dbtest.DSN(), err)
		return
	}
	// CI initializes the same cache tables; no fixture may silently bypass
	// the durable cache and still pass just because two outputs look alike.
	providerName := fmt.Sprintf("similarity-cache-%d", time.Now().UnixNano())
	memql.SetActiveEmbedderBinding(memql.EmbedderBinding{ProviderRef: providerName, Dimensions: 1536})
	t.Cleanup(memql.ClearActiveEmbedderBinding)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM embedding_cache WHERE provider=$1`, providerName)
	})
	_, tableErr := memql.EnsureEmbeddingVectorTable(ctx, db, providerName, 1536)
	require.NoError(t, tableErr)
	p := &cachedQueryProvider{}
	makeReplica := func() *Integration {
		i := New(nil)
		i.SetDBGetter(func() *sql.DB { return db })
		i.SetStagedConceptPredicate(func(string) bool { return false })
		i.SetEmbeddingProvider(func(context.Context, string) (memql.EmbeddingAIProvider, error) { return p, nil })
		return i
	}
	a, b := makeReplica(), makeReplica()
	args := map[string]any{"text": "What report format did I ask for?", "concept": "v1:test:emptyMemoryCorpus"}
	_, err := a.similarToHandler(ctx, args, 0)
	require.NoError(t, err)
	_, err = b.similarToHandler(ctx, args, 0)
	require.NoError(t, err)
	require.Equal(t, int32(1), p.calls.Load(), "a second replica must reuse the stored query embedding")
	_, err = db.ExecContext(ctx, `UPDATE embedding_cache SET expires_at=NOW()-INTERVAL '1 second' WHERE provider=$1`, providerName)
	require.NoError(t, err)
	_, err = b.similarToHandler(ctx, args, 0)
	require.NoError(t, err)
	require.Equal(t, int32(2), p.calls.Load(), "an expired vector is recomputed")
	memql.SetActiveEmbedderBinding(memql.EmbedderBinding{ProviderRef: providerName, Dimensions: 768})
	_, err = a.similarToHandler(ctx, args, 0)
	require.ErrorContains(t, err, "expected 768", "runtime width drift must not create another active space")
	p.dimensions = 768
	_, tableErr = memql.EnsureEmbeddingVectorTable(ctx, db, providerName, 768)
	require.NoError(t, tableErr)
	_, err = a.similarToHandler(ctx, args, 0)
	require.NoError(t, err)
	require.Equal(t, int32(4), p.calls.Load(), "a binding change invalidates the vector before TTL expiry")
}
