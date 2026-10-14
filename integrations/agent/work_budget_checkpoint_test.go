package agent

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

type budgetBoundaryEngine struct {
	workPromptEngine
	ceiling     *memql.RunCeilingError
	beforeModel bool
	beforeTool  int
	tools       int
	parked      int
	failSave    bool
}

func (e *budgetBoundaryEngine) ContextWithWorkCallDeadline(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if e.beforeModel {
		return ctx, func() {}, e.ceiling
	}
	return ctx, func() {}, nil
}
func (e *budgetBoundaryEngine) PrepareWorkTool(context.Context) error {
	e.tools++
	if e.tools == e.beforeTool {
		return e.ceiling
	}
	return nil
}
func (e *budgetBoundaryEngine) PauseWorkForBudget(_ context.Context, c *memql.RunCeilingError, messages []common.ChatMessage) error {
	if e.failSave {
		return fmt.Errorf("checkpoint write refused")
	}
	if c != e.ceiling {
		panic("wrong ceiling")
	}
	e.savedContinuation = messages
	e.parked++
	return &work.HumanWait{ApprovalID: "budget"}
}

func TestBudgetBoundaryPausesBothToolLoopsWithExactCompletedEffects(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, boundary := range []string{"before-model", "between-tools", "provider-refusal"} {
			t.Run(fmt.Sprintf("stream=%v/%s", streaming, boundary), func(t *testing.T) {
				ceiling := &memql.RunCeilingError{RunId: "root", StepKey: "section", Breach: memql.RunCeilingBreach{Ceiling: "wallClock", Limit: "600000ms", Actual: "600001ms"}}
				engine := &budgetBoundaryEngine{ceiling: ceiling, beforeModel: boundary == "before-model"}
				if boundary == "between-tools" {
					engine.beforeTool = 2
				}
				provider := &outcomeProvider{steps: [][]common.ToolCall{{{ID: "first", Name: "saveFirst", Arguments: `{}`}, {ID: "second", Name: "saveSecond", Arguments: `{}`}}}}
				executor := &guidanceLoopExecutor{}
				r := testReplier()
				r.engine = engine
				r.stamper = newToolRecorder(executor, r.logger)
				ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "root", GoalId: "goal", OwnerUserId: "owner", StepKey: "section"})
				messages := []common.ChatMessage{{Role: "user", Content: "Synthetic bounded work"}}
				var err error
				if boundary == "provider-refusal" {
					if streaming {
						p := &errorSequenceStreamProvider{next: func(int) (<-chan common.StreamToolChunk, error) {
							return streamErrorChunks(common.StreamToolChunk{Error: ceiling}), nil
						}}
						_, err = r.runStreamingToolLoop(ctx, p, messages, nil, &captureSink{}, time.Now(), "budget", turnContext{})
						require.Equal(t, 1, p.calls)
					} else {
						p := &scriptedToolProvider{steps: []scriptStep{{err: ceiling}}}
						_, err = r.runNonStreamingToolLoop(ctx, p, nil, messages, nil, &captureSink{}, time.Now(), "budget", turnContext{})
						require.Equal(t, 1, p.calls)
					}
				} else if streaming {
					_, err = r.runStreamingToolLoop(ctx, provider, messages, nil, &captureSink{}, time.Now(), "budget", turnContext{})
				} else {
					_, err = r.runNonStreamingToolLoop(ctx, provider, nil, messages, nil, &captureSink{}, time.Now(), "budget", turnContext{})
				}
				var wait *work.HumanWait
				require.ErrorAs(t, err, &wait)
				require.Equal(t, "budget", wait.ApprovalID)
				require.Equal(t, 1, engine.parked)
				if boundary == "between-tools" {
					require.Equal(t, []string{"saveFirst"}, executor.calls)
					paired := map[string]string{}
					for _, m := range engine.savedContinuation {
						if m.Role == "tool" {
							paired[m.ToolCallId] = m.Content
						}
					}
					require.Contains(t, paired["first"], "completed-effect")
					require.Contains(t, paired["second"], "Not executed")
				} else {
					require.Empty(t, executor.calls)
					require.Equal(t, messages, engine.savedContinuation)
				}
			})
		}
	}
}

func TestBudgetBoundaryCannotTurnFailedCheckpointIntoAnApproval(t *testing.T) {
	e := &budgetBoundaryEngine{ceiling: &memql.RunCeilingError{}, failSave: true}
	r := testReplier()
	r.engine = e
	ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "root", GoalId: "goal", OwnerUserId: "owner"})
	err := r.pauseAtBudgetBoundary(ctx, e.ceiling, []common.ChatMessage{{Role: "user", Content: "work"}})
	require.ErrorContains(t, err, "checkpoint write refused")
	require.Zero(t, e.parked)
	require.ErrorIs(t, err, memql.ErrWorkCheckpointRequired)
	var wait *work.HumanWait
	require.NotErrorAs(t, err, &wait)
}
