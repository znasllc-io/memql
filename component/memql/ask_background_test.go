package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func askTestActor() context.Context {
	user := "v1:identity:user:" + id.NewShortId()
	return auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: user}), &auth.AccessContext{UserId: user, Role: auth.RoleWriter})
}
func askTestConversation(t *testing.T, e *MemQLEngine, ctx context.Context) string {
	t.Helper()
	call, err := parser.RenderCall("createAskConversation", map[string]any{"requestId": id.NewShortId(), "title": "Testing"})
	require.NoError(t, err)
	result, err := e.Execute(ctx, "mutation "+call)
	require.NoError(t, err)
	return fmt.Sprint(MaterializeRows(result)[0]["id"])
}
func TestAskTurnWritersAcrossReplicasPreserveEachOtherAndClockSkew(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := askTestActor()
	conversation := askTestConversation(t, a, ctx)
	transcript := askTranscript{Turns: []AskTurn{{ID: "a", Prompt: "First", State: "queued"}, {ID: "b", Prompt: "Second", State: "queued"}}}
	require.NoError(t, a.askSave(ctx, conversation, "Testing", transcript))
	// A peer with a fast clock previously wrote this row. Both writers must
	// advance that persisted version, not stamp it with their own earlier now.
	row, err := a.askRead(ctx, conversation)
	require.NoError(t, err)
	future := time.Now().UTC().Add(time.Minute)
	call, err := parser.RenderCall("saveAskConversation", map[string]any{"id": conversation, "title": "Testing", "transcript": row["transcript"], "versionTime": future.Format(time.RFC3339Nano)})
	require.NoError(t, err)
	_, err = a.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
	require.NoError(t, err)
	failures := make(chan error, 2)
	for n, e := range []*MemQLEngine{a, b} {
		go func(n int, e *MemQLEngine) {
			for pass := 0; pass < 10; pass++ {
				turn := transcript.Turns[n]
				turn.Answer = fmt.Sprintf("%d:%d", n, pass)
				if err := e.askSaveTurn(ctx, conversation, turn); err != nil {
					failures <- err
					return
				}
			}
			failures <- nil
		}(n, e)
	}
	require.NoError(t, <-failures)
	require.NoError(t, <-failures)
	row, err = b.askRead(ContextWithFreshRead(ctx), conversation)
	require.NoError(t, err)
	require.Contains(t, fmt.Sprint(row), "0:9")
	require.Contains(t, fmt.Sprint(row), "1:9")
	got, valid := askTimestamp(row["createdAt"])
	require.True(t, valid)
	require.True(t, got.After(future))
}
func TestAskBackgroundResultReconstructedOnAnotherReplica(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	ctx, cancel := context.WithTimeout(askTestActor(), 10*time.Second)
	defer cancel()
	ac, _ := auth.AccessFromContext(ctx)
	conversation := askTestConversation(t, a, ctx)
	runID := "v1:work:run:" + id.NewShortId()
	calls := 0
	a.builtinExecutorHandlers["integration.work.createGoal"] = func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		calls++
		call, _ := parser.RenderCall("createWorkRun", map[string]any{"runId": runID, "goalId": "goal", "automationName": "background", "templateFingerprint": "test", "triggeredBy": "manual", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano), "classification": map[string]any{"workload": "research", "workTitle": "Bird research", "acknowledgement": "I’ll look into the birds you asked about."}})
		_, err := a.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
		if err != nil {
			return nil, err
		}
		update, _ := parser.RenderCall("updateWorkRun", map[string]any{"runId": runID, "classification": map[string]any{"workload": "research", "workTitle": "Bird research", "acknowledgement": "I’ll look into the birds you asked about."}})
		if _, err = a.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+update); err != nil {
			return nil, err
		}
		raw, _ := json.Marshal(map[string]any{"runId": runID, "goalId": "goal"})
		return []memorynodes.MemoryNode{{ID: "intake", Payload: raw}}, nil
	}
	answer, err := a.RunAsk(ctx, conversation, "first", "Research birds", "", AskRoute{}, nil, nil)
	require.NoError(t, err)
	require.NotEmpty(t, answer)
	runCtx := common.ContextWithRun(ctx, common.RunContext{RunId: runID, GoalId: "goal", OwnerUserId: ac.UserId, StepKey: "reason"})
	require.NoError(t, b.RecordWorkProgress(runCtx, WorkEvent{ID: "result", Kind: "response", Phase: "completed", Text: "The research is complete."}))
	ended := time.Now().UTC()
	call, _ := parser.RenderCall("updateWorkRun", map[string]any{"runId": runID, "status": "succeeded", "outcome": map[string]any{"returned": "terminal receipt"}, "finishedAt": ended.Format(time.RFC3339Nano)})
	_, err = b.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
	require.NoError(t, err)
	snapshot, err := b.askConversationSnapshotBuiltin(ctx, map[string]any{"conversationId": conversation}, 0)
	require.NoError(t, err)
	var result struct {
		Transcript askTranscript `json:"transcript"`
	}
	require.NoError(t, json.Unmarshal(snapshot[0].Payload, &result))
	require.Len(t, result.Transcript.Turns, 1)
	got := result.Transcript.Turns[0]
	require.Equal(t, "done", got.State)
	require.Equal(t, "The research is complete.", got.Answer)
	require.NotEmpty(t, got.Acknowledgement)
	require.NotNil(t, got.EndedAt)
	require.WithinDuration(t, ended, *got.EndedAt, time.Microsecond)
	_, err = b.askConversationSnapshotBuiltin(askTestActor(), map[string]any{"conversationId": conversation}, 0)
	require.Error(t, err)
	_, err = a.RunAsk(ctx, conversation, "first", "Duplicate", "", AskRoute{}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
}
func TestConversationRecallKeepsSpeakerAndOwner(t *testing.T) {
	e, _, _ := readMergeTestEngine(t)
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	mine, other := askTestActor(), askTestActor()
	for n, ctx := range []context.Context{mine, other} {
		conversation := askTestConversation(t, e, ctx)
		name := "Jose"
		if n == 1 {
			name = "PrivateOther"
		}
		require.NoError(t, e.askSave(ctx, conversation, "Name", askTranscript{Turns: []AskTurn{{ID: "name", Prompt: "My name is " + name, Answer: "Hello " + name, State: "done"}}}))
	}
	catalog, catalogErr := e.workCapabilitiesBuiltin(mine, map[string]any{"search": ""}, 0)
	require.NoError(t, catalogErr)
	require.Contains(t, string(catalog[0].Payload), "v1:os:askConversation")
	require.Contains(t, string(catalog[0].Payload), "workSearchConversations")
	ranked, err := e.workCapabilitiesBuiltin(mine, map[string]any{"search": "memory search conversations"}, 0)
	require.NoError(t, err)
	require.Contains(t, string(ranked[0].Payload), "workSearchConversations")
	nodes, err := e.workSearchConversationsBuiltin(mine, map[string]any{"search": "NAME"}, 0)
	require.NoError(t, err)
	require.Contains(t, string(nodes[0].Payload), "Jose")
	require.Contains(t, string(nodes[0].Payload), `"role":"user"`)
	require.NotContains(t, string(nodes[0].Payload), "PrivateOther")
}
func TestMemoryExcerptPreservesUnicodeMatchOffsets(t *testing.T) {
	original := strings.Repeat("İ", 3000) + "Jose" + strings.Repeat("Ⱥ", 3000)
	excerpt := memoryExcerpt(original, "jose")
	require.True(t, utf8.ValidString(excerpt))
	require.Contains(t, excerpt, "Jose")
	require.LessOrEqual(t, len(excerpt), 2400)
}
