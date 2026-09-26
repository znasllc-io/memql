package memql

import (
	"context"
	"encoding/json"
	"fmt"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/common"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
)

func TestAskConversationOwnershipAndServerTranscript(t *testing.T) {
	e, _, _ := sharedReadMergeEngine(t)
	actor := func(user string) context.Context {
		ctx := auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: user})
		return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: user, Role: auth.RoleWriter})
	}
	mine := actor("v1:identity:user:" + id.NewShortId())
	theirs := actor("v1:identity:user:" + id.NewShortId())
	result, err := e.Execute(mine, `mutation createAskConversation(requestId: "private", title: "Private question")`)
	require.NoError(t, err)
	rows := MaterializeRows(result.OutputPayload())
	require.Len(t, rows, 1)
	conversationID := fmt.Sprint(rows[0]["id"])
	_, err = e.askRead(mine, conversationID)
	require.NoError(t, err)
	_, err = e.askRead(theirs, conversationID)
	require.Error(t, err)
	transcript := askTranscript{Turns: []AskTurn{{ID: "turn", Prompt: "Hello", Answer: "Private answer", State: "done", StartedAt: time.Now()}}}
	require.NoError(t, e.askSave(mine, conversationID, "Private question", transcript))
	row, err := e.askRead(mine, conversationID)
	require.NoError(t, err)
	require.Contains(t, fmt.Sprint(row), "Private answer")
	_, err = e.Execute(mine, fmt.Sprintf(`mutation saveAskConversation(id: %s, title: "Forged", transcript: {})`, parser.QuoteString(conversationID)))
	require.Error(t, err)
	_, err = e.RunAsk(theirs, conversationID, "new-turn", "show private data", "", nil, nil)
	require.Error(t, err)
	release, err := e.lockAskConversation(mine, conversationID)
	require.NoError(t, err)
	_, err = e.lockAskConversation(mine, conversationID)
	require.Error(t, err)
	release()
	release, err = e.lockAskConversation(mine, conversationID)
	require.NoError(t, err)
	release()
	// The real declarations must reach their registries, not merely parse.
	_, err = e.functions.Get("work.workCapabilities")
	require.NoError(t, err)
	_, err = e.tools.Get("work.discoverCapabilities")
	require.NoError(t, err)
	_, present := e.prompts.Get("askMemql")
	require.False(t, present, "Ask must not retain a separate model prompt")
}

func TestAskConversationCreationSeparatesHistoriesAndPreservesRetries(t *testing.T) {
	e, _, _ := sharedReadMergeEngine(t)
	actor := func() context.Context {
		user := "v1:identity:user:" + id.NewShortId()
		ctx := auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: user})
		return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: user, Role: auth.RoleWriter})
	}
	mine, theirs := actor(), actor()
	create := func(ctx context.Context, requestID string) string {
		call, err := parser.RenderCall("createAskConversation", map[string]any{"requestId": requestID, "title": "New conversation"})
		require.NoError(t, err)
		result, err := e.Execute(ctx, "mutation "+call)
		require.NoError(t, err)
		rows := MaterializeRows(result.OutputPayload())
		require.Len(t, rows, 1)
		return fmt.Sprint(rows[0]["id"])
	}
	first := create(mine, "first")
	transcript := askTranscript{Turns: []AskTurn{{ID: "saved-turn", Prompt: "Keep this", Answer: "Private history", State: "done", StartedAt: time.Now()}}}
	require.NoError(t, e.askSave(mine, first, "Original conversation", transcript))
	second := create(mine, "second")
	require.NotEqual(t, first, second, "identical titles must not identify the same conversation")
	require.Equal(t, first, create(mine, "first"), "a retried creation keeps its original identity")
	row, err := e.askRead(mine, first)
	require.NoError(t, err)
	require.Equal(t, "Original conversation", row["title"])
	require.Contains(t, fmt.Sprint(row), "Private history", "new conversations and retries must not reset a saved transcript")
	row, err = e.askRead(mine, second)
	require.NoError(t, err)
	require.NotContains(t, fmt.Sprint(row), "Private history")
	foreign := create(theirs, "first")
	require.NotEqual(t, first, foreign, "request identities are scoped to their authenticated owner")
	_, err = e.askRead(theirs, first)
	require.Error(t, err)
}

func TestAskEstimateUsesOnlyMatchingSuccessfulRoute(t *testing.T) {
	turns := []AskTurn{{Activity: []WorkEvent{{Kind: "model", Phase: "completed", Provider: "fleet:local", Model: "local", ElapsedMS: 8000}, {Kind: "model", Phase: "failed", Provider: "fleet:local", Model: "local", ElapsedMS: 50000}, {Kind: "model", Phase: "completed", Provider: "remote", Model: "other", ElapsedMS: 500}}}}
	ms, source := askExpected(turns, "fleet:local", "local")
	require.EqualValues(t, 8000, ms)
	require.Equal(t, "recent calls", source)
	ms, source = askExpected(turns, "fleet:new", "new")
	require.EqualValues(t, 60000, ms)
	require.Equal(t, "initial estimate", source)
}

func TestAskUsesWorkIntakeAndPreservesTurnIdentity(t *testing.T) {
	e, _, _ := readMergeTestEngine(t)
	previousCatalog := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previousCatalog) })
	user := "v1:identity:user:" + id.NewShortId()
	ctx := auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: user}), &auth.AccessContext{UserId: user, Role: auth.RoleWriter})
	calls := 0
	e.builtinExecutorHandlers["integration.work.createGoal"] = func(callCtx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		calls++
		require.Equal(t, "ask", args["requestedVia"])
		input := args["input"].(map[string]any)
		require.Contains(t, fmt.Sprint(input), "app:todos")
		runID := "v1:work:run:" + id.NewShortId()
		runCtx := common.ContextWithRun(callCtx, common.RunContext{RunId: runID, GoalId: "goal", OwnerUserId: user, StepKey: "reason"})
		// A separate engine execution writes the durable result, with no Ask
		// callback or original stream state on its context.
		require.NoError(t, e.RecordWorkProgress(runCtx, WorkEvent{ID: "reply", Kind: "response", Phase: "completed", Text: "Your workspace."}))
		call, err := parser.RenderCall("createWorkRun", map[string]any{"runId": runID, "goalId": "goal", "automationName": "shared", "templateFingerprint": "test", "triggeredBy": "manual", "status": "succeeded", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339)})
		require.NoError(t, err)
		_, err = e.Execute(auth.ContextWithInternalOrigin(callCtx), "mutation "+call)
		require.NoError(t, err)
		raw, _ := json.Marshal(map[string]any{"goalId": "goal", "runId": runID})
		return []memorynodes.MemoryNode{{ID: "intake", Payload: raw}}, nil
	}
	result, err := e.Execute(ctx, `mutation createAskConversation(requestId: "shared", title: "New conversation")`)
	require.NoError(t, err)
	conversationID := fmt.Sprint(MaterializeRows(result)[0]["id"])
	var text string
	answer, err := e.RunAsk(ctx, conversationID, "turn", "What can you do?", "app:todos", func(delta string) { text += delta }, nil)
	require.NoError(t, err)
	require.Equal(t, "Your workspace.", answer)
	require.Equal(t, answer, text)
	_, err = e.RunAsk(ctx, conversationID, "turn", "Do it again", "", nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, calls, "a retried turn must never submit a second goal")
	row, err := e.askRead(ctx, conversationID)
	require.NoError(t, err)
	require.Contains(t, fmt.Sprint(row), "runId")
}

func TestAskCannotExecuteCapabilityOutsideCallerAuthority(t *testing.T) {
	installDeployPartFake(t)
	e, reached := installBuiltinGateProbe(t)
	_, err := e.workExecuteBuiltin(asCaller("writer"), map[string]any{"name": gatedProbeBuiltin, "arguments": map[string]any{}}, 0)
	require.Error(t, err)
	require.Zero(t, *reached, "Ask may not elevate the caller through a named builtin")
	fn, _ := e.functions.Get(gatedProbeBuiltin)
	require.False(t, e.workCapabilityAllowed(asCaller("writer"), fn))
	require.True(t, e.workCapabilityAllowed(asCaller("developer"), fn))
	fn.ServerOnly = true
	require.False(t, e.workCapabilityAllowed(asCaller("owner"), fn), "internal-only is not owner authority")
}

func TestAskExecutesQualifiedCapabilitiesWithCallerRowScope(t *testing.T) {
	e, _, _ := sharedReadMergeEngine(t)
	actor := func() context.Context {
		user := "v1:identity:user:" + id.NewShortId()
		return auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: user}), &auth.AccessContext{UserId: user, Role: auth.RoleWriter})
	}
	mine, theirs := actor(), actor()
	todoID := id.NewShortId()
	runID := "v1:work:run:" + id.NewShortId()
	ac, _ := auth.AccessFromContext(mine)
	mine = common.ContextWithRun(mine, common.RunContext{RunId: runID, OwnerUserId: ac.UserId, StepKey: "capability"})
	_, err := e.workExecuteBuiltin(mine, map[string]any{"name": "todos.createTodo", "arguments": map[string]any{"todoId": todoID, "title": "Ask capability regression"}}, 0)
	require.NoError(t, err)
	read := map[string]any{"name": "todos.todoById", "arguments": map[string]any{"todoId": "v1:todos:todo:" + todoID}}
	rows, err := e.workExecuteBuiltin(mine, read, 0)
	require.NoError(t, err)
	require.Contains(t, string(rows[0].Payload), "Ask capability regression")
	rows, err = e.workExecuteBuiltin(theirs, read, 0)
	require.NoError(t, err)
	require.NotContains(t, string(rows[0].Payload), "Ask capability regression")
	events, err := e.workRows(mine, "workObservationsForOwnerRun", runID)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Contains(t, fmt.Sprint(events), todoID)

}

func TestWorkDiscoveryFindsNavigationByAppArgument(t *testing.T) {
	e, _, _ := sharedReadMergeEngine(t)
	rows, err := e.workCapabilitiesBuiltin(asCaller("owner"), map[string]any{"search": "open fleet"}, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Contains(t, string(rows[0].Payload), "workNavigate")
	require.Contains(t, string(rows[0].Payload), `"enum":["users","fleet"`)
	fn, err := e.functions.Get("worker.agentworkerDispatchHost")
	require.NoError(t, err)
	var found bool
	for _, field := range workCapabilityFields(fn) {
		if field.Name == "ownerUserId" {
			found = true
		}
	}
	require.True(t, found, "Fleet identity injection must use the builtin contract")
}
