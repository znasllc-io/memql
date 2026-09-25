package memql

import (
	"context"
	"fmt"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
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
	result, err := e.Execute(mine, `mutation createAskConversation(title: "Private question")`)
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
	_, err = e.Execute(mine, fmt.Sprintf(`mutation saveAskConversation(id: %q, title: "Forged", transcript: {})`, conversationID))
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
	_, err = e.functions.Get("os.askCapabilities")
	require.NoError(t, err)
	_, err = e.tools.Get("os.askDiscover")
	require.NoError(t, err)
	_, present := e.prompts.Get("askMemql")
	require.True(t, present)
}

func TestAskEstimateUsesOnlyMatchingSuccessfulRoute(t *testing.T) {
	turns := []AskTurn{{Activity: []AskEvent{{Kind: "model", Phase: "completed", Provider: "fleet:local", Model: "local", ElapsedMS: 8000}, {Kind: "model", Phase: "failed", Provider: "fleet:local", Model: "local", ElapsedMS: 50000}, {Kind: "model", Phase: "completed", Provider: "remote", Model: "other", ElapsedMS: 500}}}}
	ms, source := askExpected(turns, "fleet:local", "local")
	require.EqualValues(t, 8000, ms)
	require.Equal(t, "recent calls", source)
	ms, source = askExpected(turns, "fleet:new", "new")
	require.EqualValues(t, 60000, ms)
	require.Equal(t, "initial estimate", source)
}

type askScriptedModel struct {
	calls    int
	messages []common.ChatMessage
}

func (p *askScriptedModel) CallChatStreamWithTools(ctx context.Context, messages []common.ChatMessage, tools []common.ToolDefinition) (<-chan common.StreamToolChunk, error) {
	p.calls++
	p.messages = messages
	out := make(chan common.StreamToolChunk, 2)
	if p.calls == 1 {
		out <- common.StreamToolChunk{ToolCalls: []common.ToolCallDelta{{Index: 0, ID: "call-1", Name: "askDiscover", Arguments: `{"search":"todos"}`}}}
	} else {
		out <- common.StreamToolChunk{Content: "Here is your workspace."}
	}
	out <- common.StreamToolChunk{Done: true}
	close(out)
	return out, nil
}
func TestAskRunsTheRealDSLToolLoopAndPersistsHistory(t *testing.T) {
	e, _, _ := readMergeTestEngine(t)
	previousCatalog := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previousCatalog) })
	user := "v1:identity:user:" + id.NewShortId()
	ctx := auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: user}), &auth.AccessContext{UserId: user, Role: auth.RoleWriter})
	model := &askScriptedModel{}
	e.SetAIResolver(testAIResolver{fn: func(ctx context.Context, req airoute.ResolveRequest) (ResolvedProvider, error) {
		return ResolvedProvider{Client: model, Resolution: airoute.Resolution{ProviderName: "fleet:test", Model: "test-model"}}, nil
	}})
	result, err := e.Execute(ctx, `mutation createAskConversation(title: "New conversation")`)
	require.NoError(t, err)
	rows := MaterializeRows(result)
	require.Len(t, rows, 1)
	conversationID := fmt.Sprint(rows[0]["id"])
	var text string
	var events []AskEvent
	_, err = e.RunAsk(ctx, conversationID, "one", "What can you do with my todos?", "app:todos", func(delta string) { text += delta }, func(event AskEvent) { events = append(events, event) })
	require.NoError(t, err)
	require.Equal(t, "Here is your workspace.", text)
	require.Equal(t, 2, model.calls)
	require.Empty(t, events, "a test provider without the router must not invent call metadata")
	row, err := e.askRead(ctx, conversationID)
	require.NoError(t, err)
	require.Contains(t, fmt.Sprint(row), "Here is your workspace.")
	_, err = e.RunAsk(ctx, conversationID, "two", "What did I ask?", "", nil, nil)
	require.NoError(t, err)
	require.Contains(t, fmt.Sprint(model.messages), "What can you do with my todos?")
	calls := model.calls
	_, err = e.RunAsk(ctx, conversationID, "two", "Repeat the action", "", nil, nil)
	require.NoError(t, err)
	require.Equal(t, calls, model.calls, "replayed turn IDs must not run a second time")
}

func TestAskCannotExecuteCapabilityOutsideCallerAuthority(t *testing.T) {
	installDeployPartFake(t)
	e, reached := installBuiltinGateProbe(t)
	_, err := e.askExecuteBuiltin(asCaller("writer"), map[string]any{"name": gatedProbeBuiltin, "arguments": map[string]any{}}, 0)
	require.Error(t, err)
	require.Zero(t, *reached, "Ask may not elevate the caller through a named builtin")
	fn, _ := e.functions.Get(gatedProbeBuiltin)
	require.False(t, e.askAllowed(asCaller("writer"), fn))
	require.True(t, e.askAllowed(asCaller("developer"), fn))
	fn.ServerOnly = true
	require.False(t, e.askAllowed(asCaller("owner"), fn), "internal-only is not owner authority")
}
