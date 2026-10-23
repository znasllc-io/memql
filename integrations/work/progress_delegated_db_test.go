package work

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

func TestDelegatedSourceReadsRenewProgressAcrossReplicas(t *testing.T) {
	a, b := openActsDB(t), openActsDB(t)
	b.owner = a.owner
	runID := a.openRun(t, nil)
	rc := common.RunContext{RunId: runID, GoalId: rowString(a.runRow(t, runID), "goalId"), OwnerUserId: a.owner, StepKey: "research", Mode: common.RunModeLive}
	a.write(t, "updateWorkGoal", map[string]any{"goalId": rc.GoalId, "ceilings": map[string]any{"maxModelCalls": 96}})
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "classification": map[string]any{"workload": "lookup"}})
	ctx := memql.WithActingAgentRole(memql.WithActingAgentId(common.ContextWithRun(actorCtx(a.owner), rc), "research-agent"), "assistant")
	tool := "readMuseumSource"
	require.NoError(t, a.eng.Tools().Upsert(&memql.Tool{Name: tool, Description: "Read an owned museum research note.", InputSchema: json.RawMessage(`{"type":"object","properties":{"noteId":{"type":"string"}},"required":["noteId"]}`), Handler: &memql.ToolHandler{Type: "function", FunctionName: "notes.noteById"}}))
	kind, _, known := a.eng.ToolCallee(tool)
	require.True(t, known)
	require.Equal(t, "query", kind)
	noteID := newRowId("v1:notes:note")
	a.write(t, "notes.createNote", map[string]any{"noteId": noteID, "body": "The museum acquired the collection in 1924."})
	readAndRecord := func(id string, forged bool) string {
		args := map[string]any{"noteId": id}
		raw, err := a.eng.ExecuteToolByName(ctx, tool, args)
		require.NoError(t, err)
		if forged {
			args["nativeResultEvidence"] = workstate.ArtifactHash(map[string]any{"claimed": "A fabricated result cannot renew work."})
		}
		// These are the same executed-result and durable-record seams used by
		// Apps/MCP, not a synthetic tool_result observation or model assertion.
		require.NoError(t, a.eng.RecordDelegatedToolSource(ctx, tool, args, raw))
		return raw
	}
	require.Contains(t, readAndRecord(noteID, false), "The museum acquired")
	ok, err := b.i.ContinueWorkProgress(ctx, rc, workstate.Spent{ModelCalls: 12}, 0)
	require.NoError(t, err)
	require.True(t, ok, "the actual delegated source read must supply progress on another replica")
	readAndRecord(noteID, false)
	readAndRecord(newRowId("v1:notes:note"), true)
	require.NoError(t, a.eng.RecordDelegatedToolSource(ctx, tool, nil, `{"isError":true,"content":[{"type":"text","text":"{\"id\":\"forged\",\"body\":\"A failed request cannot establish a new finding.\"}"}]}`))
	ok, err = b.i.ContinueWorkProgress(ctx, rc, workstate.Spent{ModelCalls: 24}, 0)
	require.NoError(t, err)
	require.False(t, ok, "unchanged, failed and argument-forged results grant no new credit")
	nextID := newRowId("v1:notes:note")
	a.write(t, "notes.createNote", map[string]any{"noteId": nextID, "body": "The museum catalog identifies Alice Morgan as the donor."})
	readAndRecord(nextID, false)
	ok, err = b.i.ContinueWorkProgress(ctx, rc, workstate.Spent{ModelCalls: 24}, 0)
	require.NoError(t, err)
	require.True(t, ok)
}
