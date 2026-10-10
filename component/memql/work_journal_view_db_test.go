package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
)

// The activity view must not ship the replay journal's multi-megabyte tool
// payloads. Exercise the actual DSL projections, ownership and cursor on two
// engines; no serving replica may need the first page's local state.
func TestWorkJournalViewPagesAcrossReplicasWithoutToolPayloads(t *testing.T) {
	a, db, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	owner := "v1:identity:user:" + uniqueSuffix("journal-owner")
	runID := "v1:work:run:" + uniqueSuffix("journal-run")
	ctx := clusterOwnerCtx(owner)
	base := time.Now().UTC().Add(-time.Hour)
	seed := func(concept, suffix, rowOwner, kind string, data map[string]any, seq int) {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"ownerUserId": rowOwner, "runId": runID, "stepKey": "research", "kind": kind, "content": suffix, "data": data, "model": "test", "served": "local", "response": map[string]any{"text": strings.Repeat("draft", 22000)}})
		require.NoError(t, err)
		row := &memorynodes.MemoryNode{ID: concept + ":" + uniqueSuffix(suffix), Concept: concept, CreatedBy: rowOwner, CreatedAt: base.Add(time.Duration(seq) * time.Second), Payload: payload, Provenance: json.RawMessage(`{"kind":"direct","name":"journal-view-test"}`)}
		_, err = db.NewInsert().Model(row).Exec(context.Background())
		require.NoError(t, err)
	}
	for i := 0; i < 53; i++ {
		seed("v1:work:observation", fmt.Sprintf("tool-%02d", i), owner, "tool_result", map[string]any{"output": strings.Repeat("evidence", 16000)}, i)
		seed("v1:work:modelCall", fmt.Sprintf("call-%02d", i), owner, "", nil, i)
	}
	seed("v1:work:observation", "private", "v1:identity:user:other", "feedback", map[string]any{"verdict": "bad"}, 54)
	seed("v1:work:observation", "human", owner, "feedback", map[string]any{"verdict": "good"}, 55)
	seed("v1:work:observation", "validator", owner, "decision", map[string]any{"validator": map[string]any{"verdict": "pass"}}, 56)
	seed("v1:work:observation", "other-decision", owner, "decision", map[string]any{"action": "continue"}, 57)
	query := func(engine *MemQLEngine, name, cursor string) *ExecuteResult {
		t.Helper()
		call, err := parser.RenderCall(name, map[string]any{"runId": runID})
		require.NoError(t, err)
		result, err := engine.Execute(ContextWithCursor(ctx, cursor), "query "+call)
		require.NoError(t, err)
		return result
	}
	for _, name := range []string{"workObservationSummariesForOwnerRun", "workModelCallsPageForOwnerRun"} {
		first := query(a, name, "")
		rows := MaterializeRows(first.OutputPayload())
		require.Len(t, rows, 50)
		require.NotEmpty(t, first.GetMeta().Cursor)
		ids := map[string]bool{}
		for _, row := range rows {
			ids[fmt.Sprint(row["id"])] = true
			require.NotContains(t, row, "data")
			require.NotContains(t, row, "response")
		}
		raw, err := json.Marshal(first.OutputPayload())
		require.NoError(t, err)
		require.Less(t, len(raw), 100000, "the transport page must stay small despite >5MB of stored tool data")
		next := query(b, name, first.GetMeta().Cursor)
		remaining := MaterializeRows(next.OutputPayload())
		want := 3
		if name == "workObservationSummariesForOwnerRun" {
			want = 6
		}
		require.Len(t, remaining, want)
		for _, row := range remaining {
			require.False(t, ids[fmt.Sprint(row["id"])])
			require.Equal(t, owner, row["ownerUserId"])
		}
		require.Empty(t, next.GetMeta().Cursor)
	}
	verdicts := MaterializeRows(query(b, "workVerdictsForOwnerRun", "").OutputPayload())
	require.Len(t, verdicts, 2)
	for _, row := range verdicts {
		require.Contains(t, []any{"human", "validator"}, row["content"])
		require.Contains(t, row, "data")
	}
	full := MaterializeRows(query(b, "workObservationsForOwnerRun", "").OutputPayload())
	raw, err := json.Marshal(full)
	require.NoError(t, err)
	require.Greater(t, len(raw), 5*1024*1024, "the harness still has the complete replay data")
}
