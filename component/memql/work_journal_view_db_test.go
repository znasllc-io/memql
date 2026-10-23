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

// Model usage arrives in completed execution observations, independently of
// replay modelCall rows. Project only bounded usage facts and owner/run isolation when a second replica resumes the cursor.
func TestWorkModelUsagePageIsOwnedBoundedAndPayloadFree(t *testing.T) {
	a, db, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	owner := "v1:identity:user:" + uniqueSuffix("usage-owner")
	foreign := "v1:identity:user:" + uniqueSuffix("usage-foreign")
	runID := "v1:work:run:" + uniqueSuffix("usage-run")
	otherRun := "v1:work:run:" + uniqueSuffix("usage-other-run")
	foreignRun := "v1:work:run:" + uniqueSuffix("usage-foreign-run")
	base := time.Now().UTC().Add(-time.Hour)
	secret := strings.Repeat("private prompt and checkpoint data", 4000)
	seed := func(rowOwner, run, kind, phase string, seq int) {
		t.Helper()
		usage := map[string]any{"id": fmt.Sprintf("call-%03d", seq), "model": "usage-model", "inputTokens": int64(17), "outputTokens": int64(5), "tokensEstimated": seq%2 == 0, "totalCost": 0.25, "pricingConfigured": true, "billing": "local", "cacheKind": "none", "prompt": secret, "response": secret}
		payload, err := json.Marshal(map[string]any{"ownerUserId": rowOwner, "runId": run, "stepKey": "review", "kind": "execution", "content": secret, "data": map[string]any{"execution": map[string]any{"kind": kind, "phase": phase, "call": usage, "input": secret, "output": secret}, "checkpoint": secret}})
		require.NoError(t, err)
		concept := "v1:work:observation"
		row := &memorynodes.MemoryNode{ID: concept + ":" + uniqueSuffix(fmt.Sprintf("usage-%03d", seq)), Concept: concept, CreatedBy: rowOwner, CreatedAt: base.Add(time.Duration(seq) * time.Second), Payload: payload, Provenance: json.RawMessage(`{"kind":"direct","name":"usage-view-test"}`)}
		_, err = db.NewInsert().Model(row).Exec(context.Background())
		require.NoError(t, err)
	}
	for n := 0; n < 53; n++ {
		seed(owner, runID, "model", "completed", n)
	}
	// Even a cluster owner must not receive another person's rows sharing the
	// requested run key, nor their own rows for a different run or phase.
	seed(foreign, runID, "model", "completed", 53)
	seed(foreign, foreignRun, "model", "completed", 54)
	seed(owner, otherRun, "model", "completed", 55)
	seed(owner, runID, "model", "started", 56)
	seed(owner, runID, "tool", "completed", 57)
	query := func(engine *MemQLEngine, actorID, requestedRun, cursor string) *ExecuteResult {
		t.Helper()
		call, err := parser.RenderCall("workModelUsageForOwnerRun", map[string]any{"runId": requestedRun})
		require.NoError(t, err)
		result, err := engine.Execute(ContextWithCursor(clusterOwnerCtx(actorID), cursor), "query "+call)
		require.NoError(t, err)
		return result
	}
	first := query(a, owner, runID, "")
	rows := MaterializeRows(first.OutputPayload())
	require.Len(t, rows, 50)
	require.NotEmpty(t, first.GetMeta().Cursor)
	second := query(b, owner, runID, first.GetMeta().Cursor)
	rest := MaterializeRows(second.OutputPayload())
	require.Len(t, rest, 3)
	require.Empty(t, second.GetMeta().Cursor)
	ids := map[string]bool{}
	for _, row := range append(rows, rest...) {
		key := fmt.Sprint(row["id"])
		require.False(t, ids[key], "cursor repeated a usage observation")
		ids[key] = true
		require.Equal(t, owner, row["ownerUserId"])
		require.Equal(t, runID, row["runId"])
		require.Equal(t, "review", row["stepKey"])
		require.NotContains(t, row, "content")
		require.NotContains(t, row, "response")
		require.ElementsMatch(t, []string{"id", "createdAt", "ownerUserId", "runId", "stepKey", "model", "inputTokens", "outputTokens", "tokensEstimated", "totalCost", "pricingConfigured", "billing", "cacheKind"}, mapKeysForUsageTest(row))
		require.Equal(t, "usage-model", row["model"])
		require.EqualValues(t, 17, row["inputTokens"])
		require.EqualValues(t, 5, row["outputTokens"])
		require.IsType(t, true, row["tokensEstimated"])
		require.Equal(t, true, row["pricingConfigured"])
		require.True(t, strings.HasPrefix(key, "call-"), "the public id must be the model call identity, not a memory-node envelope")
	}
	raw, err := json.Marshal(first.OutputPayload())
	require.NoError(t, err)
	require.Less(t, len(raw), 100000)
	require.NotContains(t, string(raw), "private prompt")
	require.NotContains(t, string(raw), "checkpoint")
	require.Empty(t, MaterializeRows(query(b, owner, foreignRun, "").OutputPayload()), "local owner role widened another owner's run")
	require.Empty(t, MaterializeRows(query(b, "v1:identity:user:"+uniqueSuffix("usage-stranger"), runID, "").OutputPayload()))
	foreignRows := MaterializeRows(query(b, foreign, runID, "").OutputPayload())
	require.Len(t, foreignRows, 1)
	require.Equal(t, foreign, foreignRows[0]["ownerUserId"])
}

func mapKeysForUsageTest(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}
