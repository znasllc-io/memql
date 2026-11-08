package memql

// This opt-in evidence harness is intentionally compatible with the baseline
// engine. Copy this file into a detached baseline checkout, run both revisions
// against the SAME disposable database, and retain their JSON/EXPLAIN outputs.
// Timing is evidence, never a flaky wall-clock assertion in the regular suite.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

type osListEvidenceContextKey struct{}
type osListEvidenceCapture struct {
	mu      sync.Mutex
	queries []string
}

func (h *osListEvidenceCapture) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}
func (h *osListEvidenceCapture) AfterQuery(ctx context.Context, event *bun.QueryEvent) {
	if ctx.Value(osListEvidenceContextKey{}) != true || event.Err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.queries = append(h.queries, event.Query)
}

func TestOSListPerformanceEvidence(t *testing.T) {
	output := os.Getenv("MEMQL_OS_LIST_EVIDENCE_FILE")
	if output == "" {
		t.Skip("opt-in: set MEMQL_OS_LIST_EVIDENCE_FILE against a disposable database")
	}
	eng, db, _ := sharedReadMergeEngine(t)
	ctx := context.Background()
	const owner = "v1:identity:user:os-list-evidence-owner"
	const prefix = "os-list-evidence-20261107-"
	const rows = 2000
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	// Fixed IDs/timestamps and conflict-ignore keep fixtures identical between
	// runs, including the per-row 14 KiB incompressible policy snapshot.
	var nodes []memorynodes.MemoryNode
	for i := -1; i < rows; i++ {
		concept := "v1:pipelines:run"
		id := fmt.Sprintf("%s:%s%04d", concept, prefix, i)
		var policy strings.Builder
		for n := 0; n < 224; n++ {
			sum := sha256.Sum256([]byte(fmt.Sprintf("%s%d/%d", prefix, i, n)))
			policy.WriteString(hex.EncodeToString(sum[:]))
		}
		name := fmt.Sprintf("Routine build %04d", i)
		if i < 100 {
			name = fmt.Sprintf("Earlier NEEDLE%%_ build %04d", i)
		}
		payload := map[string]any{
			"ownerUserId": owner, "pipelineId": "evidence-pipeline", "repository": "example/evidence", "headBranch": "main",
			"sha": fmt.Sprintf("%040d", i), "title": name, "status": "completed", "conclusion": "success",
			"queuedAt": base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano),
			"mode":     "full", "event": "manual", "runKey": id, "attempt": 1, "trigger": "manual",
			"policySnapshot": map[string]any{"workflow": policy.String()},
		}
		if i == -1 {
			concept, id = conceptIdentityUser, owner
			payload = map[string]any{"active": true, "displayName": "Evidence Owner", "primaryEmail": "evidence@example.test", "role": "owner"}
		}
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		schema, err := json.Marshal(map[string]any{"$id": concept})
		require.NoError(t, err)
		nodes = append(nodes, memorynodes.MemoryNode{ID: id, Concept: concept, CreatedBy: owner, CreatedAt: base.Add(time.Duration(i) * time.Microsecond), Type: memorynodes.NodeTypeObject, Payload: raw, Schema: schema, Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)})
	}
	_, err := db.NewInsert().Model(&nodes).On(`CONFLICT (id, "createdAt") DO NOTHING`).Exec(ctx)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `ANALYZE "MemoryNodes"`)
	require.NoError(t, err)
	hook := &osListEvidenceCapture{}
	db.AddQueryHook(hook)
	readCtx := context.WithValue(ContextWithFreshRead(shareDirActor(ctx, owner, auth.RoleOwner)), osListEvidenceContextKey{}, true)
	calls := []struct{ name, call string }{{"existing_full_50", `query pipelineRunsForOwner()`}}
	if os.Getenv("MEMQL_OS_LIST_BASELINE") != "1" {
		calls = append(calls, struct{ name, call string }{"browse_summary_10", `query pipelineRunsBrowse()`}, struct{ name, call string }{"search_summary_10", `query pipelineRunsBrowse(search:"needle%_")`})
	}
	report := map[string]any{"revision": os.Getenv("MEMQL_OS_LIST_REVISION"), "go": runtime.Version(), "platform": runtime.GOOS + "/" + runtime.GOARCH, "fixture_rows": rows, "policy_snapshot_bytes_per_row": 14 * 1024, "samples_per_query": 9, "cache": "fresh database reads; database warm; GC before each sample"}
	var results []map[string]any
	for _, call := range calls {
		_, err := eng.Execute(readCtx, call.call)
		require.NoError(t, err) // warm planner/schema paths
		var durations []float64
		var allocated, mallocs []uint64
		var record map[string]any
		for sample := 0; sample < 9; sample++ {
			runtime.GC()
			hook.mu.Lock()
			hook.queries = nil
			hook.mu.Unlock()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			result, err := eng.Execute(readCtx, call.call)
			elapsed := time.Since(start)
			runtime.ReadMemStats(&after)
			require.NoError(t, err)
			durations = append(durations, float64(elapsed)/float64(time.Millisecond))
			allocated = append(allocated, after.TotalAlloc-before.TotalAlloc)
			mallocs = append(mallocs, after.Mallocs-before.Mallocs)
			if sample != 0 {
				continue
			}
			payload, err := json.Marshal(result.OutputPayload())
			require.NoError(t, err)
			hook.mu.Lock()
			queries := append([]string(nil), hook.queries...)
			hook.mu.Unlock()
			record = map[string]any{"name": call.name, "query": call.call, "rows": len(MaterializeRows(result.OutputPayload())), "shaped_payload_bytes": len(payload), "has_cursor": result.GetMeta().Cursor != "", "sql_statements": len(queries)}
			for _, query := range queries {
				if !strings.Contains(query, "AS latest_keys") || !strings.HasPrefix(query, "SELECT") {
					continue
				}
				var raw []byte
				require.NoError(t, db.QueryRowContext(ctx, `EXPLAIN (ANALYZE,BUFFERS,VERBOSE,FORMAT JSON) `+query).Scan(&raw))
				var plan []map[string]any
				require.NoError(t, json.Unmarshal(raw, &plan))
				record["primary_sql"] = query
				record["primary_explain"] = plan
				record["primary_sql_rows_returned"] = plan[0]["Plan"].(map[string]any)["Actual Rows"]
				break
			}
		}
		ordered := append([]float64(nil), durations...)
		sort.Float64s(ordered)
		record["latency_ms_samples"], record["median_ms"], record["p95_ms"] = durations, ordered[len(ordered)/2], ordered[len(ordered)-1]
		record["allocated_bytes_samples"], record["mallocs_samples"] = allocated, mallocs
		results = append(results, record)
		t.Logf("%s: %.2f ms median, %v rows, %v bytes, cursor=%v, SQL materialized=%v", call.name, record["median_ms"], record["rows"], record["shaped_payload_bytes"], record["has_cursor"], record["primary_sql_rows_returned"])
	}
	report["queries"] = results
	raw, err := json.MarshalIndent(report, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(output, append(raw, '\n'), 0644))
}
