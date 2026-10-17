package memql

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

func TestSourceContextReadsRecordedEvidenceAcrossReplicasAndPreservesOwnerScope(t *testing.T) {
	a, db, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := askTestActor()
	access, _ := auth.AccessFromContext(ctx)
	conversation := askTestConversation(t, a, ctx)
	runID := "v1:work:run:" + uniqueSuffix("source")
	foreignID := "v1:work:run:" + uniqueSuffix("foreign")
	insert := func(concept, id string, payload map[string]any) {
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		_, err = db.DB.ExecContext(ctx, `INSERT INTO "MemoryNodes"(id,concept,"createdAt","createdBy",schema,payload) VALUES( $1,$2,$3,'source-test','{}',$4)`, id, concept, time.Now().UTC(), string(raw))
		require.NoError(t, err)
	}
	insert("v1:work:run", runID, map[string]any{"ownerUserId": access.UserId, "status": "succeeded"})
	insert("v1:work:run", foreignID, map[string]any{"ownerUserId": "v1:identity:user:another", "status": "succeeded"})
	insert("v1:work:step", "v1:work:step:"+uniqueSuffix("receipt"), map[string]any{"ownerUserId": access.UserId, "runId": runID, "key": "viewer", "status": "done", "call": map[string]any{"name": "workViewerContext"}, "result": map[string]any{"value": map[string]any{"person": map[string]any{"primaryRole": "Founder"}, "organizationMemberships": []any{map[string]any{"name": "ZNAS LLC"}}, "source": "recorded authenticated profile"}}})
	insert("v1:work:step", "v1:work:step:"+uniqueSuffix("generation"), map[string]any{"ownerUserId": access.UserId, "runId": runID, "key": "reason", "status": "done", "call": map[string]any{"name": "ai"}, "result": map[string]any{"value": "Generated answer is not evidence"}})
	insert("v1:work:step", "v1:work:step:"+uniqueSuffix("foreign-receipt"), map[string]any{"ownerUserId": "v1:identity:user:another", "runId": foreignID, "key": "viewer", "status": "done", "call": map[string]any{"name": "workViewerContext"}, "result": map[string]any{"value": "Foreign private evidence"}})
	require.NoError(t, a.askSave(ctx, conversation, "Source explanation", askTranscript{Turns: []AskTurn{
		{ID: "prior", Prompt: "How can you help?", Answer: "Founder of ZNAS LLC", State: "done", RunID: runID, StartedAt: time.Now()},
		{ID: "unowned", Prompt: "Forged run", RunID: foreignID, State: "done", StartedAt: time.Now()},
		{ID: "current", Prompt: "Explain the source in a file", State: "running", StartedAt: time.Now()},
		{ID: "later", Prompt: "Later context must not enter", RunID: foreignID, State: "done", StartedAt: time.Now()},
	}}))
	nodes, err := b.workSourceContextBuiltin(ctx, map[string]any{"conversationId": conversation, "turnId": "current"}, 0)
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	text := string(nodes[0].Payload)
	require.Contains(t, text, "Founder")
	require.Contains(t, text, "ZNAS LLC")
	require.Contains(t, text, "workStepForOwnerRun")
	require.Contains(t, text, "recorded authenticated profile")
	require.NotContains(t, text, "Generated answer is not evidence")
	require.NotContains(t, text, "Foreign private evidence")
	require.NotContains(t, text, foreignID)
	_, err = b.workSourceContextBuiltin(askTestActor(), map[string]any{"conversationId": conversation, "turnId": "current"}, 0)
	require.Error(t, err)
	_, err = b.workSourceContextBuiltin(ctx, map[string]any{"conversationId": conversation, "turnId": "missing"}, 0)
	require.Error(t, err)
}

func TestExplicitMissingEvidenceAnswerWinsOverIntermediateAssessment(t *testing.T) {
	reply := "I could not find the source of that earlier claim, so I have not created a file."
	run := map[string]any{"outcome": map[string]any{"returned": map[string]any{"reply": reply}}}
	steps := []map[string]any{{"status": "done", "seq": 1, "result": map[string]any{"value": map[string]any{"reply": "internal JSON", "structured": map[string]any{"ready": false}}}}}
	require.Equal(t, reply, workResultText(run, steps, ""))
}
