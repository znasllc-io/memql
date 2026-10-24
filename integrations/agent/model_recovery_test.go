package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

const modelCeilingFailure = "worker: model call exceeded its 10m0s ceiling"

type modelRecoveryEngine struct {
	workPromptEngine
	decisions, parked int
	fail              bool
	limits            []int
}

func (e *modelRecoveryEngine) RecoverWorkModelCall(ctx context.Context, failure string, attempt, maximum int, messages []common.ChatMessage) (bool, error) {
	e.decisions++
	e.limits = append(e.limits, maximum)
	if e.fail {
		return false, fmt.Errorf("synthetic failed checkpoint")
	}
	value, err := workflowhost.Run(ctx, "workModelCallRecovery", map[string]any{"retriesSpent": attempt, "maxRetries": maximum}, workflowhost.Options{})
	if err != nil {
		return false, err
	}
	if value.(map[string]any)["act"] == "retry" {
		return true, nil
	}
	e.savedContinuation = messages
	e.parked++
	return false, &work.HumanWait{ApprovalID: "model-timeout"}
}

// A capacity refusal before the next model call is a suspension of this exact
// exchange, not an automation failure that can restart already completed work.
type contextCapacityEngine struct {
	modelRecoveryEngine
	compactions int
}

func (e *contextCapacityEngine) CompactWorkContext(_ context.Context, messages []common.ChatMessage, _ []common.ToolDefinition, _ int) ([]common.ChatMessage, error) {
	e.compactions++
	if e.compactions > 1 {
		return nil, fmt.Errorf("context checkpoint budget exhausted; originals were retained")
	}
	return messages, nil
}

func TestContextCapacitySuspendsExactPreparedWorkInBothLoops(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%v", streaming), func(t *testing.T) {
			engine := &contextCapacityEngine{}
			provider := &modelCeilingProvider{}
			executor := &guidanceLoopExecutor{}
			r := testReplier()
			r.engine = engine
			r.stamper = newToolRecorder(executor, r.logger)
			ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner", StepKey: "section"})
			prepared := common.ToolCall{ID: "prepared", Name: "requestComputerUseScope", Arguments: `{"command":"` + strings.Repeat("exact prepared input ", 1500) + `"}`}
			messages := []common.ChatMessage{{Role: "user", Content: "Continue the approved work"}, {Role: "assistant", ToolCalls: []common.ToolCall{prepared}}, {Role: "tool", ToolCallId: "prepared", Content: "Approval recorded"}}
			var err error
			if streaming {
				_, err = r.runStreamingToolLoop(ctx, provider, messages, nil, &captureSink{}, time.Now(), "fixture", turnContext{})
			} else {
				_, err = r.runNonStreamingToolLoop(ctx, provider, nil, messages, nil, &captureSink{}, time.Now(), "fixture", turnContext{})
			}
			var wait *work.HumanWait
			require.ErrorAs(t, err, &wait)
			require.Equal(t, 1, provider.calls, "do not resend identical context after a capacity refusal")
			require.Equal(t, []string{"saveFirst"}, executor.calls)
			require.Equal(t, []int{0}, engine.limits, "capacity exhaustion never buys another automatic retry")
			var found, receipts int
			for _, message := range engine.savedContinuation {
				for _, call := range message.ToolCalls {
					if call.ID == prepared.ID {
						require.Equal(t, prepared, call)
						found++
					}
				}
				if message.Role == "tool" && message.ToolCallId == "done-effect" {
					require.Contains(t, message.Content, "completed-effect")
					receipts++
				}
			}
			require.Equal(t, 1, found)
			require.Equal(t, 1, receipts)
		})
	}
}

type modelCeilingProvider struct {
	calls    int
	timeouts int
	partial  string
	messages []common.ChatMessage
}

func (p *modelCeilingProvider) CallChatWithTools(ctx context.Context, m []common.ChatMessage, _ []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	p.calls++
	p.messages = append([]common.ChatMessage(nil), m...)
	if p.calls == 1 {
		return &common.ToolCallingChatResult{ToolCalls: []common.ToolCall{{ID: "done-effect", Name: "saveFirst", Arguments: `{}`}}}, nil
	}
	if p.calls <= 1+p.timeouts {
		return nil, fmt.Errorf("remote provider: %s", modelCeilingFailure)
	}
	return &common.ToolCallingChatResult{ToolCalls: outcomeCall(`{"status":"complete","response":"Finished from saved progress"}`)}, nil
}
func (p *modelCeilingProvider) CallChatStreamWithTools(ctx context.Context, m []common.ChatMessage, tools []common.ToolDefinition) (<-chan common.StreamToolChunk, error) {
	result, err := p.CallChatWithTools(ctx, m, tools)
	ch := make(chan common.StreamToolChunk, 3)
	if err != nil {
		if p.partial == "text" {
			ch <- common.StreamToolChunk{Content: "Unfinished response"}
		}
		if p.partial == "tool" {
			ch <- common.StreamToolChunk{ToolCalls: []common.ToolCallDelta{{Index: 0, ID: "unstarted", Name: "saveSecond", Arguments: `{}`}}}
		}
		ch <- common.StreamToolChunk{Error: err}
	} else {
		ds := []common.ToolCallDelta{}
		for n, c := range result.ToolCalls {
			ds = append(ds, common.ToolCallDelta{Index: n, ID: c.ID, Name: c.Name, Arguments: c.Arguments})
		}
		ch <- common.StreamToolChunk{ToolCalls: ds, Done: true}
	}
	close(ch)
	return ch, nil
}

func TestModelCeilingRetriesOnlyTheProviderAndCheckpointsExhaustion(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, mode := range []string{"recover", "exhausted", "failed-checkpoint", "partial-text", "partial-tool"} {
			if !streaming && (mode == "partial-text" || mode == "partial-tool") {
				continue
			}
			t.Run(fmt.Sprintf("streaming=%v/%s", streaming, mode), func(t *testing.T) {
				engine := &modelRecoveryEngine{fail: mode == "failed-checkpoint"}
				p := &modelCeilingProvider{timeouts: 2}
				if mode == "recover" {
					p.timeouts = 1
				}
				if mode == "partial-text" {
					p.partial = "text"
				}
				if mode == "partial-tool" {
					p.partial = "tool"
				}
				executor := &guidanceLoopExecutor{}
				r := testReplier()
				r.engine = engine
				r.stamper = newToolRecorder(executor, r.logger)
				ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner", StepKey: "section"})
				var err error
				if streaming {
					_, err = r.runStreamingToolLoop(ctx, p, []common.ChatMessage{{Role: "user", Content: "Complete the bounded section"}}, nil, &captureSink{}, time.Now(), "fixture", turnContext{})
				} else {
					_, err = r.runNonStreamingToolLoop(ctx, p, nil, []common.ChatMessage{{Role: "user", Content: "Complete the bounded section"}}, nil, &captureSink{}, time.Now(), "fixture", turnContext{})
				}
				require.Equal(t, []string{"saveFirst"}, executor.calls, "never replay completed or partial proposed effects")
				if mode == "recover" {
					require.NoError(t, err)
					require.Equal(t, 3, p.calls)
					require.Zero(t, engine.parked)
				} else if mode == "failed-checkpoint" {
					require.ErrorIs(t, err, memql.ErrWorkCheckpointRequired)
					require.Equal(t, 2, p.calls)
				} else {
					var wait *work.HumanWait
					require.ErrorAs(t, err, &wait)
					require.Equal(t, 1, engine.parked)
					if p.partial != "" {
						require.Equal(t, 2, p.calls)
						require.Equal(t, []int{0}, engine.limits)
					} else {
						require.Equal(t, 3, p.calls)
					}
				}
				var receipts int
				for _, m := range p.messages {
					if m.Role == "tool" && m.ToolCallId == "done-effect" {
						receipts++
						require.Contains(t, m.Content, "completed-effect")
					}
				}
				require.Equal(t, 1, receipts)
			})
		}
	}
}

func TestModelCeilingDoesNotRecoverAnInFlightToolOrCancelledCall(t *testing.T) {
	engine := &modelRecoveryEngine{}
	r := testReplier()
	r.engine = engine
	ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner", StepKey: "section"})
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	handled, err := r.recoverModelCall(ctx, fmt.Errorf(modelCeilingFailure), []common.ChatMessage{{Role: "user", Content: "work"}}, 0, true)
	require.True(t, handled)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, engine.decisions)
	// An arbitrary tool error does not pass through recoverModelCall. Its
	// ordinary bounded failure path must not create a native suspension.
	executor := &ceilingToolExecutor{}
	r.stamper = newToolRecorder(executor, r.logger)
	live := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner", StepKey: "section"})
	p := &outcomeProvider{steps: [][]common.ToolCall{{{ID: "uncertain", Name: "saveFirst", Arguments: `{}`}}, outcomeCall(`{"status":"complete","response":"No effect was confirmed"}`)}}
	_, _ = r.runNonStreamingToolLoop(live, p, nil, []common.ChatMessage{{Role: "user", Content: "work"}}, nil, &captureSink{}, time.Now(), "fixture", turnContext{})
	require.Zero(t, engine.decisions)
	require.Equal(t, 1, executor.calls)
}

func TestContextSuspensionDoesNotHideUnrelatedFailuresOrAuthorizeUnownedWork(t *testing.T) {
	engine := &modelRecoveryEngine{}
	r := testReplier()
	r.engine = engine
	owned := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner", StepKey: "section"})
	for _, failure := range []string{"checkpoint storage permission denied", "context deadline exceeded", "context checkpoint database unavailable"} {
		err := fmt.Errorf("%s", failure)
		require.Same(t, err, r.pauseAfterContextRecovery(owned, err, []common.ChatMessage{{Role: "user", Content: "work"}}))
	}
	capacity := fmt.Errorf("context checkpoint budget exhausted; originals were retained")
	require.Same(t, capacity, r.pauseAfterContextRecovery(context.Background(), capacity, []common.ChatMessage{{Role: "user", Content: "work"}}))
	cancelled, cancel := context.WithCancel(owned)
	cancel()
	require.ErrorIs(t, r.pauseAfterContextRecovery(cancelled, capacity, []common.ChatMessage{{Role: "user", Content: "work"}}), context.Canceled)
	require.Zero(t, engine.decisions)
}

type ceilingToolExecutor struct{ calls int }

func (e *ceilingToolExecutor) ExecuteToolByName(context.Context, string, map[string]any) (string, error) {
	e.calls++
	return "", fmt.Errorf(modelCeilingFailure)
}
func (*ceilingToolExecutor) Execute(context.Context, string) (any, error) { return nil, nil }
