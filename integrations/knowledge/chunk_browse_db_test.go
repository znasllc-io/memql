package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

func TestTrainingReviewNativePagesAndDomainBoundaryDB(t *testing.T) {
	f := newSeedReplicas(t)
	prefix := fmt.Sprintf("training-page-%d", time.Now().UnixNano())
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: prefix, Role: auth.RoleWriter})
	t.Cleanup(func() {
		_, _ = f.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE id LIKE $1`, "v1:knowledge:documentChunk:"+prefix+"%")
	})
	_, err := f.db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,concept,type,"createdBy","createdAt",payload,schema,metadata,provenance) SELECT 'v1:knowledge:documentChunk:'||$1||'-'||n,'v1:knowledge:documentChunk','object','test',clock_timestamp(),jsonb_build_object('domainId',$1,'text',$1||CASE WHEN n<12 THEN ' old NEEDLE%_' ELSE ' recent' END,'source','fileUpload','validationStatus','unvalidated'),'{"$id":"v1:knowledge:documentChunk"}'::jsonb,'{}'::jsonb,'{}'::jsonb FROM generate_series(0,24) n`, prefix)
	require.NoError(t, err)
	walk := func(search string, sizes []int) {
		cursor := ""
		seen := map[string]bool{}
		for n, size := range sizes {
			result, err := f.a.trainingChunksBrowse(ctx, map[string]any{"pending": true, "search": search, "cursor": cursor}, 0)
			require.NoError(t, err)
			require.Len(t, result, 1)
			var page struct {
				Rows   []map[string]any `json:"rows"`
				Cursor string           `json:"nextCursor"`
			}
			require.NoError(t, json.Unmarshal(result[0].Payload, &page))
			require.Len(t, page.Rows, size)
			for _, row := range page.Rows {
				id := fmt.Sprint(row["id"])
				require.False(t, seen[id])
				seen[id] = true
				require.NotEmpty(t, row["text"])
			}
			cursor = page.Cursor
			if n == len(sizes)-1 {
				require.Empty(t, cursor)
			} else {
				require.NotEmpty(t, cursor)
			}
		}
	}
	walk(prefix, []int{10, 10, 5})
	walk(prefix+" old needle%_", []int{10, 2})
	reader := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: prefix, Role: auth.RoleReader})
	_, err = f.a.trainingChunksBrowse(reader, map[string]any{}, 0)
	require.ErrorContains(t, err, "writer")
	_, err = f.ea.Execute(reader, `query documentChunksForDomain(pending:true)`)
	require.Error(t, err, "direct callers still require a domain")
	_, err = f.ea.Execute(reader, fmt.Sprintf(`query documentChunksForDomain(domainId:%q)`, prefix))
	require.NoError(t, err, "existing domain-scoped reader admission is unchanged")
}

func TestTrainerCorpusWalkPreservesPromptBudgetAndRejectsPartialDB(t *testing.T) {
	f := newSeedReplicas(t)
	prefix := fmt.Sprintf("training-corpus-%d", time.Now().UnixNano())
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: prefix, Role: auth.RoleWriter})
	t.Cleanup(func() {
		_, _ = f.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE id LIKE $1`, "v1:knowledge:documentChunk:"+prefix+"%")
	})
	_, err := f.db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,concept,type,"createdBy","createdAt",payload,schema,metadata,provenance) SELECT 'v1:knowledge:documentChunk:'||$1||'-'||n,'v1:knowledge:documentChunk','object','test',clock_timestamp(),jsonb_build_object('domainId',$1,'text',repeat('x',900),'source','fileUpload','validationStatus',CASE WHEN n<40 THEN 'validated' ELSE 'rejected' END),'{"$id":"v1:knowledge:documentChunk"}'::jsonb,'{}'::jsonb,'{}'::jsonb FROM generate_series(0,99) n`, prefix)
	require.NoError(t, err)
	call := fmt.Sprintf(`query documentChunksForDomain(domainId:%q)`, prefix)
	count := 0
	rows, err := collectExistingCorpus(ctx, func(ctx context.Context, q string) (*memql.ExecuteResult, error) {
		count++
		return f.ea.Execute(ctx, q)
	}, call)
	require.NoError(t, err)
	require.Len(t, rows, 40)
	require.Equal(t, 10, count)
	for _, row := range rows {
		require.Len(t, row["text"].(string), existingCorpusTextMaxLen+3)
	}
	count = 0
	failure := errors.New("lost continuation")
	rows, err = collectExistingCorpus(ctx, func(ctx context.Context, q string) (*memql.ExecuteResult, error) {
		count++
		if count == 8 {
			return nil, failure
		}
		return f.ea.Execute(ctx, q)
	}, call)
	require.ErrorIs(t, err, failure)
	require.Nil(t, rows, "ten accepted rows before failure must not become a partial prompt")
	count = 0
	rows, err = collectExistingCorpus(ctx, func(ctx context.Context, q string) (*memql.ExecuteResult, error) {
		count++
		r, e := f.ea.Execute(ctx, q)
		if r != nil {
			r.SetMeta(&memql.ResultMeta{Cursor: "repeated", HasMore: true})
		}
		return r, e
	}, call)
	require.Error(t, err)
	require.Nil(t, rows)
	require.LessOrEqual(t, count, 2)
}
