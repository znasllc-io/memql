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
}

func (e *guidanceLoopEngine) ReadWorkGuidance(context.Context) ([]memql.WorkGuidance, error) {
	return append([]memql.WorkGuidance(nil), e.messages...), nil
}
func (e *guidanceLoopEngine) AcknowledgeWorkGuidance(_ context.Context, ids []string) error {
	e.acknowledged = append(e.acknowledged, ids...)
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
					require.Contains(t, messages[len(messages)-1].Content, "Stop the remaining action")
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
				require.Contains(t, messages[len(messages)-1].Content, "Preserve the prior result")
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
