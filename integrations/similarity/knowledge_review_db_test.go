package similarity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
)

// Publication is a property of the latest row, independent of the replica and
// of the cached query embedding. Excluded nearest hits must not consume top-K.
func TestKnowledgeReviewControlsLatestRetrievalAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "reviewed knowledge", dbtest.DSN(), err)
		return
	}
	prefix := fmt.Sprintf("knowledge-review-%d", time.Now().UnixNano())
	provider := &localWidthProvider{}
	table, err := memql.EnsureEmbeddingVectorTable(ctx, db, prefix, 1024)
	require.NoError(t, err)
	var ids []string
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE id=$1`, id)
		}
		_, _ = db.ExecContext(ctx, `DELETE FROM embedding_cache WHERE provider=$1`, prefix)
		_, _ = db.ExecContext(ctx, `DROP TABLE `+table)
	})
	at := time.Now().UTC().Add(-time.Hour)
	write := func(key, status, domain string, superseded bool, distance float32, concept string) string {
		t.Helper()
		id := concept + ":" + prefix + key
		payload := map[string]any{"text": key, "domainId": domain, "superseded": superseded}
		if status != "" {
			payload["validationStatus"] = status
		}
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		at = at.Add(time.Millisecond)
		_, err = db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,"createdAt","createdBy",schema,payload,metadata,type,concept,provenance) VALUES($1,$2,'test','{}',$3::jsonb,'{}','object',$4,'{}')`, id, at, string(raw), concept)
		require.NoError(t, err)
		vector := make([]float32, 1024)
		vector[0] = 1
		vector[1] = distance
		_, err = db.ExecContext(ctx, `INSERT INTO `+table+` (id,concept,vector_field,embedding) VALUES($1,$2,'content',$3::vector) ON CONFLICT (id,vector_field) DO UPDATE SET embedding=EXCLUDED.embedding`, id, concept, vectorLiteral(vector))
		require.NoError(t, err)
		ids = append(ids, id)
		return id
	}
	const concept = "v1:knowledge:documentChunk"
	for _, status := range []string{"", "unvalidated", "rejected"} {
		write(status, status, prefix, false, 0, concept)
	}
	write("superseded", "validated", prefix, true, 0, concept)
	first := write("first", "validated", prefix, false, 0.1, concept)
	second := write("second", "validated", prefix, false, 0.2, concept)
	write("old-approved", "validated", prefix, false, 0, concept)
	write("old-approved", "rejected", prefix, false, 0, concept)
	write("moved", "validated", prefix, false, 0, concept)
	write("moved", "validated", prefix+"-other", false, 0, concept)
	replica := func() *Integration {
		i := New(nil)
		i.SetDBGetter(func() *sql.DB { return db })
		i.SetEmbeddingProvider(func(context.Context, string) (memql.EmbeddingAIProvider, error) { return provider, nil })
		i.SetStagedConceptPredicate(func(string) bool { return false })
		return i
	}
	a, b := replica(), replica()
	args := map[string]any{"text": "reviewed guidance", "concept": concept, "provider": prefix, "domains": []string{prefix}, "limit": 2}
	rows, err := a.similarToHandler(ctx, args, 0)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, first, rows[0].ID)
	require.Equal(t, second, rows[1].ID)
	write("first", "rejected", prefix, false, 0.1, concept)
	rows, err = b.similarToHandler(ctx, args, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, second, rows[0].ID)
	write("second", "validated", prefix, true, 0.2, concept)
	rows, err = a.similarToHandler(ctx, args, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
	// The no-domain path has the same gate, and an unrelated vector concept
	// does not inherit knowledge's validation vocabulary.
	delete(args, "domains")
	rows, err = b.similarToHandler(ctx, args, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Contains(t, rows[0].ID, "moved")
	other := write("ordinary", "rejected", prefix, true, 0, "v1:test:memory")
	args["concept"] = "v1:test:memory"
	rows, err = b.similarToHandler(ctx, args, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, other, rows[0].ID)
}
