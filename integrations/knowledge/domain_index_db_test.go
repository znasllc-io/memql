package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// Actual engine/database pages cross the MaxResults=500 boundary. The reducer
// must not treat @unbounded or a full page as proof of complete totals.
func TestTrainingDomainRollupsDB(t *testing.T) {
	f := newSeedReplicas(t)
	prefix := fmt.Sprintf("training-count-%d", time.Now().UnixNano())
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: prefix, Role: auth.RoleOwner})
	var err error
	t.Cleanup(func() {
		_, _ = f.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE id LIKE $1`, "v1:knowledge:documentChunk:"+prefix+"%")
	})
	for domain := 0; domain < 3; domain++ {
		_, err = f.db.ExecContext(context.Background(), `INSERT INTO "MemoryNodes" (id,concept,type,"createdBy","createdAt",payload,schema,metadata,provenance) SELECT 'v1:knowledge:documentChunk:'||$1||'-'||n,'v1:knowledge:documentChunk','object','test-list-rollup',clock_timestamp(),jsonb_build_object('domainId',$1,'validationStatus',CASE n%4 WHEN 0 THEN 'validated' WHEN 1 THEN 'rejected' WHEN 2 THEN 'unvalidated' ELSE '' END),'{"$id":"v1:knowledge:documentChunk"}'::jsonb,'{}'::jsonb,'{}'::jsonb FROM generate_series(0,199) n`, fmt.Sprintf("%s-domain-%d", prefix, domain))
		require.NoError(t, err)
	}
	source, err := f.ea.Execute(ctx, "query allDocumentChunkDomains()")
	require.NoError(t, err)
	// Reproduce the prior metadata projection on the same500-row fixture;
	// the current source additionally carries IDs/times for bounded review joins.
	priorProjection := []map[string]any{}
	for _, row := range memql.MaterializeRows(source.OutputPayload()) {
		priorProjection = append(priorProjection, map[string]any{"domainId": row["domainId"], "validationStatus": row["validationStatus"]})
	}
	before, _ := json.Marshal(priorProjection)
	start := time.Now()
	result, err := f.a.trainingDomainRollups(ctx, nil, 0)
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Len(t, result, 1)
	var got struct {
		Rollups []domainCount `json:"rollups"`
	}
	require.NoError(t, json.Unmarshal(result[0].Payload, &got))
	found := 0
	for _, count := range got.Rollups {
		if !strings.HasPrefix(count.DomainID, prefix) {
			continue
		}
		found++
		require.Equal(t, 200, count.Total)
		require.Equal(t, 50, count.Validated)
		require.Equal(t, 50, count.Rejected)
		require.Equal(t, 100, count.Unvalidated)
	}
	require.Equal(t, 3, found)
	// Includes all counts, not just this fixture. The scratch DB has no product data.
	require.Less(t, len(result[0].Payload), len(before)/10)
	t.Logf("training domains fixture=600 chunks/3 domains old_first_page_rows=%d old_wire_bytes=%d complete_rollup_bytes=%d native_elapsed_us=%d", len(memql.MaterializeRows(source.OutputPayload())), len(before), len(result[0].Payload), elapsed.Microseconds())
	_, err = f.a.trainingDomainRollups(context.Background(), nil, 0)
	require.ErrorContains(t, err, "authenticated")
	reader := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: prefix, Role: auth.RoleReader})
	_, err = f.a.trainingDomainRollups(reader, nil, 0)
	require.Error(t, err, "source query must enforce writer floor under unchanged caller")
}
