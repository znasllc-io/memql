package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

type guidanceLoopEngine struct {
	workPromptEngine
	messages     []memql.WorkGuidance
	acknowledged []string
	onSave       func([]common.ChatMessage)
}

func (e *guidanceLoopEngine) SaveWorkContinuation(ctx context.Context, messages []common.ChatMessage) error {
	if e.onSave != nil {
		e.onSave(messages)
	}
	return e.workPromptEngine.SaveWorkContinuation(ctx, messages)
}

func (e *guidanceLoopEngine) ReadWorkGuidancePage(_ context.Context, after memql.WorkGuidanceCursor) (memql.WorkGuidancePage, error) {
	return memql.WorkGuidancePageAfter(e.messages, after), nil
}
func (e *guidanceLoopEngine) AcknowledgeWorkGuidancePage(_ context.Context, through memql.WorkGuidanceCursor) error {
	for _, row := range e.messages[:through.Count] {
		e.acknowledged = append(e.acknowledged, row.ID)
	}
	return nil
}

type guidanceLoopExecutor struct {
	calls []string
	after func()
}

func (e *guidanceLoopExecutor) Execute(context.Context, string) (any, error) { return nil, nil }
func (e *guidanceLoopExecutor) ExecuteToolByName(_ context.Context, name string, _ map[string]any) (string, error) {
	e.calls = append(e.calls, name)
	if e.after != nil {
		e.after()
	}
	return `{"saved":"completed-effect"}`, nil
}

type guidanceLoopProvider struct {
	outcomeProvider
	before func(int, []common.ChatMessage)
}

func (p *guidanceLoopProvider) CallChatWithTools(ctx context.Context, messages []common.ChatMessage, tools []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	p.before(p.calls, messages)
	return p.outcomeProvider.CallChatWithTools(ctx, messages, tools)
}
func (p *guidanceLoopProvider) CallChatStreamWithTools(ctx context.Context, messages []common.ChatMessage, tools []common.ToolDefinition) (<-chan common.StreamToolChunk, error) {
	p.before(p.calls, messages)
	return p.outcomeProvider.CallChatStreamWithTools(ctx, messages, tools)
}

func TestRunningWorkGuidancePreemptsOnlyUnstartedCallsInBothLanes(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, arrival := range []string{"during-model", "between-tools"} {
			t.Run(fmt.Sprintf("stream=%v/%s", streaming, arrival), func(t *testing.T) {
				engine := &guidanceLoopEngine{}
				enqueue := func() {
					engine.messages = []memql.WorkGuidance{{ID: "human", Prompt: "Stop the remaining action; report the saved evidence."}}
				}
				executor := &guidanceLoopExecutor{}
				if arrival == "between-tools" {
					executor.after = enqueue
				}
				provider := &guidanceLoopProvider{outcomeProvider: outcomeProvider{steps: [][]common.ToolCall{
					{{ID: "first", Name: "saveFirst", Arguments: `{}`}, {ID: "second", Name: "saveSecond", Arguments: `{}`}},
					outcomeCall(`{"status":"complete","response":"Kept the saved evidence and stopped."}`),
				}}}
				provider.before = func(index int, messages []common.ChatMessage) {
					if index == 0 {
						if arrival == "during-model" {
							enqueue()
						}
						return
					}
					require.Empty(t, engine.acknowledged, "cannot claim received before model responds")
					require.Contains(t, lastGuidanceUser(messages), "Stop the remaining action")
					if arrival == "between-tools" {
						require.Contains(t, fmt.Sprint(messages), "completed-effect")
						paired := map[string]string{}
						for _, m := range messages {
							if m.Role == "tool" {
								paired[m.ToolCallId] = m.Content
							}
						}
						require.Contains(t, paired["first"], "completed-effect")
						require.Contains(t, paired["second"], "not_started")
					}
				}
				r := testReplier()
				r.engine = engine
				r.stamper = newToolRecorder(executor, r.logger)
				ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "root", GoalId: "goal", OwnerUserId: "owner", StepKey: "child-section"})
				turn := turnContext{AgentId: "assistant", RunId: "root", OwnerUserId: "owner", IsWorkExecution: true}
				tools := []common.ToolDefinition{workResponseToolDefinition()}
				var err error
				if streaming {
					_, err = r.runStreamingToolLoop(ctx, provider, nil, tools, &captureSink{}, time.Now(), "guidance", turn)
				} else {
					_, err = r.runNonStreamingToolLoop(ctx, provider, nil, nil, tools, &captureSink{}, time.Now(), "guidance", turn)
				}
				require.NoError(t, err)
				require.Equal(t, 2, provider.calls)
				require.Equal(t, []string{"human"}, engine.acknowledged)
				if arrival == "during-model" {
					require.Empty(t, executor.calls)
				} else {
					require.Equal(t, []string{"saveFirst"}, executor.calls)
				}
				require.Equal(t, 1, strings.Count(fmt.Sprint(provider.messages), "Stop the remaining action"))
			})
		}
	}
}

func TestWorkGuidanceRemainsQueuedWhenReasoningFails(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			engine := &guidanceLoopEngine{messages: []memql.WorkGuidance{{ID: "human", Prompt: "Preserve the prior result."}}}
			r := testReplier()
			r.engine = engine
			p := &guidanceLoopProvider{}
			p.before = func(_ int, messages []common.ChatMessage) {
				require.Contains(t, lastGuidanceUser(messages), "Preserve the prior result")
			}
			ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "root", GoalId: "goal", OwnerUserId: "owner", StepKey: "section"})
			var err error
			if streaming {
				_, err = r.runStreamingToolLoop(ctx, p, nil, nil, &captureSink{}, time.Now(), "failed-guidance", turnContext{})
			} else {
				_, err = r.runNonStreamingToolLoop(ctx, p, nil, nil, nil, &captureSink{}, time.Now(), "failed-guidance", turnContext{})
			}
			require.Error(t, err)
			require.Empty(t, engine.acknowledged)
		})
	}
}

func lastGuidanceUser(messages []common.ChatMessage) string {
	for n := len(messages) - 1; n >= 0; n-- {
		if messages[n].Role == "user" {
			return messages[n].Content
		}
	}
	return ""
}

func TestWorkGuidancePagesResumeSavedCursorBeforeAnotherOperation(t *testing.T) {
	engine := &guidanceLoopEngine{}
	for n := range 80 {
		engine.messages = append(engine.messages, memql.WorkGuidance{ID: fmt.Sprintf("note-%03d", n), Prompt: fmt.Sprintf("Instruction %d: %s", n, strings.Repeat("retain receipts; ", 50))})
	}
	r := testReplier()
	r.engine = engine
	ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner", StepKey: "section"})
	messages := []common.ChatMessage{{Role: "system", Content: "Current instructions"}, {Role: "user", Content: "Prepare the draft."}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: "done", Name: "saveDraft", Arguments: `{}`}, {ID: "pending", Name: "publishDraft", Arguments: `{}`}}}, {Role: "tool", ToolCallId: "done", Name: "saveDraft", Content: "saved exactly once"}}
	cursor := workGuidanceCursor{}
	out, changed, err := r.receiveWorkGuidance(ctx, &cursor, messages, []common.ToolCall{{ID: "pending", Name: "publishDraft", Arguments: `{}`}})
	require.NoError(t, err)
	require.True(t, changed)
	require.Contains(t, fmt.Sprint(out), "saved exactly once")
	require.Contains(t, fmt.Sprint(out), "not_started")
	require.Contains(t, lastGuidanceUser(out), "Instruction 79:")
	state, _, err := recoveryState(engine.savedContinuation)
	require.NoError(t, err)
	require.Equal(t, 80, state.GuidanceCursor.Count)
	require.Empty(t, engine.acknowledged, "saving a page is not a model acknowledgment")
	// A fresh worker reconstructs its cursor from the owned saved exchange.
	peer := testReplier()
	other := &guidanceLoopEngine{messages: engine.messages}
	peer.engine = other
	restored, changed, err := peer.receiveWorkGuidance(ctx, &workGuidanceCursor{}, engine.savedContinuation, nil)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, engine.savedContinuation, restored)
	other.messages = append(other.messages, memql.WorkGuidance{ID: "new-correction", Prompt: "Leave the draft unpublished."})
	restored, changed, err = peer.receiveWorkGuidance(ctx, &workGuidanceCursor{}, restored, nil)
	require.NoError(t, err)
	require.True(t, changed)
	require.Contains(t, lastGuidanceUser(restored), "Leave the draft unpublished.")
	require.NotContains(t, lastGuidanceUser(restored), "Instruction 0:", "old instructions are not redelivered as fresh guidance")
}

func TestWorkGuidanceMigrationDoesNotRearmKnownLaterPages(t *testing.T) {
	e := &guidanceLoopEngine{}
	state := workRecoveryState{Version: 1, ReviewActive: true, DiscoveryRounds: 3}
	for n := range 40 {
		id := fmt.Sprintf("old-%02d", n)
		e.messages = append(e.messages, memql.WorkGuidance{ID: id, Prompt: "Keep the completed receipt."})
		state.GuidanceIDs = append(state.GuidanceIDs, id)
	}
	saves := 0
	e.onSave = func(messages []common.ChatMessage) {
		saves++
		s, _, err := recoveryState(messages)
		require.NoError(t, err)
		require.True(t, s.ReviewActive, "regrouping historical guidance is not new progress")
		require.Equal(t, 3, s.DiscoveryRounds)
		if s.GuidanceCursor.Count < 40 {
			require.Len(t, s.GuidanceIDs, 40, "a peer needs the remaining migration identities")
		} else {
			require.Empty(t, s.GuidanceIDs)
		}
	}
	r := testReplier()
	r.engine = e
	ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner", StepKey: "section"})
	_, changed, err := r.receiveWorkGuidance(ctx, &workGuidanceCursor{}, recoveryMessages(t, state), nil)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, 3, saves)
}
