package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

type entryBudgetEngine struct {
	budgetBoundaryEngine
	t                 *testing.T
	prior             []common.ChatMessage
	restoreError      error
	beforeDefinitions func()
	promptPrepared    bool
}

func (e *entryBudgetEngine) WorkCallFailure(ctx context.Context) error {
	var ceiling *memql.RunCeilingError
	if errors.As(context.Cause(ctx), &ceiling) {
		return ceiling
	}
	return nil
}
func (e *entryBudgetEngine) ToolDefinitionsForNames([]string) []common.ToolDefinition {
	if e.beforeDefinitions != nil && e.promptPrepared {
		e.beforeDefinitions()
	}
	return nil
}
func (e *entryBudgetEngine) RenderPrompt(name string, data map[string]any) (string, error) {
	prompt, err := e.workPromptEngine.RenderPrompt(name, data)
	e.promptPrepared = err == nil
	return prompt, err
}

func (e *entryBudgetEngine) RestoreWorkContinuation(ctx context.Context, messages []common.ChatMessage) ([]common.ChatMessage, error) {
	e.t.Helper()
	require.NoError(e.t, ctx.Err(), "receipt read survives the expired native deadline")
	deadline, bounded := ctx.Deadline()
	require.True(e.t, bounded)
	require.LessOrEqual(e.t, time.Until(deadline), 5*time.Second)
	if e.restoreError != nil {
		return nil, e.restoreError
	}
	if e.prior != nil {
		return e.prior, nil
	}
	return messages, nil
}

func TestEntryBudgetRetainsPairedHistoryBeforeAnyPreparation(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(map[bool]string{false: "first turn", true: "saved tail"}[restore], func(t *testing.T) {
			failure := &memql.RunCeilingError{RunId: "run", StepKey: "section"}
			e := &entryBudgetEngine{t: t, budgetBoundaryEngine: budgetBoundaryEngine{ceiling: failure}}
			if restore {
				e.prior = []common.ChatMessage{{Role: "system", Content: "old installed prompt"}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: "done", Name: "saveOnce", Arguments: `{}`}}}, {Role: "tool", ToolCallId: "done", Name: "saveOnce", Content: "Completed effect receipt"}}
			}
			r := testReplier()
			r.engine = e
			ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner", StepKey: "section"})
			ctx, cancel := context.WithCancelCause(ctx)
			cancel(failure)
			msg := &memqlv1.AgentGenerateTurnMsg{History: []*memqlv1.AgentTurnMessage{{Role: "user", Content: "  Exact new section\n"}}}
			err := r.PauseWorkBeforeTurn(ctx, failure, msg)
			var wait *work.HumanWait
			require.ErrorAs(t, err, &wait)
			require.Equal(t, 1, e.parked)
			require.Zero(t, e.tools)
			if restore {
				require.Equal(t, e.prior, e.savedContinuation)
			} else {
				require.Equal(t, []common.ChatMessage{{Role: "system"}, {Role: "user", Content: "  Exact new section\n"}}, e.savedContinuation)
			}
		})
	}
}

func TestEntryBudgetRefusalsCannotBecomeResumableApproval(t *testing.T) {
	for _, kind := range []string{"caller cancel", "provider deadline", "missing owner", "missing message", "empty history", "nil history", "restore failure", "save failure"} {
		t.Run(kind, func(t *testing.T) {
			failure := error(&memql.RunCeilingError{RunId: "run", StepKey: "section"})
			e := &entryBudgetEngine{t: t, budgetBoundaryEngine: budgetBoundaryEngine{ceiling: failure.(*memql.RunCeilingError)}}
			r := testReplier()
			r.engine = e
			ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner", StepKey: "section"})
			msg := &memqlv1.AgentGenerateTurnMsg{History: []*memqlv1.AgentTurnMessage{{Role: "user", Content: "Work"}}}
			switch kind {
			case "caller cancel":
				failure = context.Canceled
			case "provider deadline":
				failure = context.DeadlineExceeded
			case "missing owner":
				ctx = context.Background()
			case "missing message":
				msg = nil
			case "empty history":
				msg.History = nil
			case "nil history":
				msg.History = []*memqlv1.AgentTurnMessage{nil}
			case "restore failure":
				e.restoreError = errors.New("checkpoint proof changed")
			case "save failure":
				e.failSave = true
			}
			err := r.PauseWorkBeforeTurn(ctx, failure, msg)
			var wait *work.HumanWait
			require.NotErrorAs(t, err, &wait)
			require.Zero(t, e.parked)
			require.Zero(t, e.tools)
			if kind == "caller cancel" || kind == "provider deadline" || kind == "missing owner" {
				require.ErrorIs(t, err, failure)
			} else {
				require.ErrorIs(t, err, memql.ErrWorkCheckpointRequired)
			}
		})
	}
}

func TestEntryBudgetExpiresDuringPreparationAndSelection(t *testing.T) {
	for _, background := range []bool{false, true} {
		for _, at := range []string{"viewer read", "provider selection"} {
			for _, native := range []bool{false, true} {
				t.Run(fmt.Sprintf("background=%v/%s/native=%v", background, at, native), func(t *testing.T) {
					base, ctx, msg := trainingPromptFixture(t)
					ctx, cancel := context.WithCancelCause(ctx)
					defer cancel(nil)
					ceiling := &memql.RunCeilingError{RunId: "run", StepKey: "section"}
					e := &entryBudgetEngine{t: t, budgetBoundaryEngine: budgetBoundaryEngine{workPromptEngine: *base, ceiling: ceiling}, prior: []common.ChatMessage{{Role: "system", Content: "installed"}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: "done", Name: "saveOnce", Arguments: `{}`}}}, {Role: "tool", ToolCallId: "done", Name: "saveOnce", Content: "Completed effect receipt"}}}
					stop := func() {
						if native {
							cancel(ceiling)
						} else {
							cancel(context.Canceled)
						}
					}
					if at == "viewer read" {
						e.execute = func(_ context.Context, q string) (any, error) {
							require.Equal(t, "builtin work.workViewerContext()", q)
							stop()
							return nil, ctx.Err()
						}
					} else {
						e.beforeDefinitions = stop
					}
					r := newTestReplier(e)
					r.router = &router.Router{}
					var err error
					if background {
						_, err = r.handleBackground(ctx, msg, &captureSink{})
					} else {
						_, err = r.handleStreaming(ctx, msg, &captureSink{})
					}
					var wait *work.HumanWait
					if native {
						require.ErrorAs(t, err, &wait)
						require.Equal(t, 1, e.parked)
						require.Equal(t, e.prior, e.savedContinuation)
					} else {
						require.ErrorIs(t, err, context.Canceled)
						require.Zero(t, e.parked)
					}
					require.Zero(t, e.tools)
					require.Equal(t, at == "provider selection", e.promptPrepared)
				})
			}
		}
	}
}

func TestEntryBudgetKeepsAnAlreadyDurablePreparationOutcome(t *testing.T) {
	_, ctx, msg := trainingPromptFixture(t)
	failure := &memql.RunCeilingError{RunId: "run", StepKey: "section"}
	ctx, cancel := context.WithCancelCause(ctx)
	cancel(failure)
	e := &entryBudgetEngine{t: t, budgetBoundaryEngine: budgetBoundaryEngine{ceiling: failure}}
	r := newTestReplier(e)
	for _, saved := range []error{&work.HumanWait{ApprovalID: "already-saved"}, fmt.Errorf("%w: persistence refused", memql.ErrWorkCheckpointRequired)} {
		require.Same(t, saved, r.PauseWorkBeforeTurn(ctx, saved, msg))
	}
	require.Zero(t, e.parked)
}
