package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

type formatRecoveryEngine struct {
	replyReviewEngine
	formats    int
	answer     string
	formatErr  error
	sourceErr  error
	unfinished bool
	legacy     *memql.WorkDelegatedOutcome
	input      memql.WorkDelegatedOutcome
}

func (e *formatRecoveryEngine) Execute(ctx context.Context, query string) (any, error) {
	if e.unfinished {
		e.calls++
		return map[string]any{"reply": "The saved result has not been verified.", "completion": map[string]any{"kind": "operation", "disposition": "unfinished", "gap": "The saved receipt needs review.", "sourceRefs": []string{}}}, nil
	}
	return e.replyReviewEngine.Execute(ctx, query)
}

func (e *formatRecoveryEngine) ReadLegacyWorkOutcome(context.Context) (*memql.WorkDelegatedOutcome, error) {
	return e.legacy, nil
}
func (e *formatRecoveryEngine) WorkDelegatedOutcomeSource(_ context.Context, _ string, m []common.ChatMessage) ([]common.ChatMessage, error) {
	return append([]common.ChatMessage(nil), m...), e.sourceErr
}
func (e *formatRecoveryEngine) FormatWorkOutcome(_ context.Context, in memql.WorkDelegatedOutcome, schema json.RawMessage) (string, error) {
	e.formats++
	e.input = in
	var pending pendingWorkReply
	if json.Unmarshal(e.replyCheckpoint.Pending, &pending) != nil || pending.OutcomeFormat == nil || pending.OutcomeFormat.Attempts != 1 || !json.Valid(schema) {
		return "", errors.New("format call preceded its durable attempt")
	}
	return e.answer, e.formatErr
}

func formatFixture(t *testing.T) (*formatRecoveryEngine, context.Context, []common.ChatMessage) {
	t.Helper()
	e := &formatRecoveryEngine{answer: `{"status":"complete","response":"The recorded observation does not establish a cause.","question":null}`}
	e.prompts = newProgressRecoveryEngine(t).prompts
	ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", StepKey: "finish", OwnerUserId: "owner"})
	ctx = memql.WithActingAgentRole(ctx, "assistant")
	messages := []common.ChatMessage{{Role: "system", Content: "Current owner instructions"}, {Role: "user", Content: "Save one result"}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: "saved", Name: "saveFile", Arguments: `{}`}}}, {Role: "tool", Name: "saveFile", ToolCallId: "saved", Content: `{"saved":"one"}`}}
	e.savedContinuation = append([]common.ChatMessage(nil), messages...)
	return e, ctx, messages
}

func TestDelegatedFormatRecoveryUsesOneToollessAttemptAndSurvivesReplica(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			e, ctx, messages := formatFixture(t)
			p := &outcomeProvider{delegated: true, steps: [][]common.ToolCall{nil}, texts: []string{"Saved one result."}}
			executor := &questionExecutor{}
			produce := func(callCtx context.Context, held *replyReviewSink) (*TurnResult, error) {
				r := newTestReplier(e)
				r.stamper = newToolRecorder(executor, r.logger)
				turn := turnContext{AgentId: "assistant", OwnerUserId: "owner", RunId: "run", IsWorkExecution: true}
				if streaming {
					return r.runStreamingToolLoop(callCtx, p, messages, []common.ToolDefinition{workResponseToolDefinition()}, held, time.Now(), "format", turn)
				}
				return r.runNonStreamingToolLoop(callCtx, p, nil, messages, []common.ToolDefinition{workResponseToolDefinition()}, held, time.Now(), "format", turn)
			}
			// Failed evidence review happens after successful protocol recovery.
			e.fail = true
			_, err := newTestReplier(e).runReviewedWork(ctx, &captureSink{}, produce)
			require.ErrorContains(t, err, "review unavailable")
			require.True(t, work.IsOutcomeRecoveryFailure(err.Error()), "Retry must preserve the completed execution after a review failure")
			e.fail = false
			result, err := newTestReplier(e).runReviewedWork(ctx, &captureSink{}, produce)
			require.NoError(t, err)
			require.NotEmpty(t, result.FinalText)
			require.Equal(t, 1, p.calls)
			require.Equal(t, 1, e.formats)
			require.Zero(t, executor.calls)
			require.Contains(t, e.input.Messages, messages[3], "completed receipt survives conversion")
		})
	}
}

func TestFormatRecoveryUnverifiedResultCannotReplanOrRepeatProduction(t *testing.T) {
	e, ctx, messages := formatFixture(t)
	e.unfinished = true
	ctx = memql.ContextWithWorkReplyTask(ctx, "Save the approved result")
	e.legacy = &memql.WorkDelegatedOutcome{SessionID: "old-session", Draft: `{}`, Messages: messages, AgentID: "assistant", AgentRole: "assistant"}
	for range 2 {
		sink := &captureSink{}
		_, err := newTestReplier(e).runReviewedWork(ctx, sink, func(context.Context, *replyReviewSink) (*TurnResult, error) {
			t.Fatal("inconclusive format review repeated production")
			return nil, nil
		})
		require.ErrorContains(t, err, "work_outcome_format_unverified")
		require.NotErrorIs(t, err, memql.ErrWorkCheckpointRequired, "review uncertainty must still record a failed-step receipt")
		require.True(t, work.IsOutcomeRecoveryFailure(err.Error()))
		_, replan := work.UnfinishedStepGap(err.Error())
		require.False(t, replan, "review uncertainty must not request operational replanning")
		require.Empty(t, sink.texts)
		require.Empty(t, sink.toolCalls)
	}
	require.Equal(t, 1, e.formats)
	require.Equal(t, 2, e.calls)
}

func TestFormatRecoveryFailureCannotResetAttemptOrReplayProduction(t *testing.T) {
	for _, kind := range []string{"transport", "malformed", "question-in-complete"} {
		t.Run(kind, func(t *testing.T) {
			e, ctx, messages := formatFixture(t)
			if kind == "transport" {
				e.formatErr = errors.New("transport lost")
			} else if kind == "malformed" {
				e.answer = `{`
			} else {
				e.answer = `{"status":"complete","response":"done","question":{"text":"Which?","kind":"text"}}`
			}
			productions := 0
			produce := func(context.Context, *replyReviewSink) (*TurnResult, error) {
				productions++
				return nil, &delegatedOutcomeError{input: memql.WorkDelegatedOutcome{SessionID: "session", Draft: "saved", Messages: messages, AgentID: "assistant", AgentRole: "assistant"}, cause: errors.New("missing marker")}
			}
			_, err := newTestReplier(e).runReviewedWork(ctx, &captureSink{}, produce)
			require.Error(t, err)
			require.NotErrorIs(t, err, memql.ErrWorkCheckpointRequired)
			e.formatErr = nil
			e.answer = `{"status":"complete","response":"Saved","question":null}`
			_, err = newTestReplier(e).runReviewedWork(ctx, &captureSink{}, produce)
			require.ErrorContains(t, err, "work_outcome_format_exhausted")
			require.NotErrorIs(t, err, memql.ErrWorkCheckpointRequired)
			require.Equal(t, 1, productions)
			require.Equal(t, 1, e.formats)
			require.Zero(t, e.calls)
		})
	}
}

func TestFormatRecoveryPreservesActualMandatoryCheckpointFailure(t *testing.T) {
	e, ctx, messages := formatFixture(t)
	e.legacy = &memql.WorkDelegatedOutcome{SessionID: "old-session", Draft: `{}`, Messages: messages, AgentID: "assistant", AgentRole: "assistant"}
	e.formatErr = fmt.Errorf("fixture admission receipt: %w", memql.ErrWorkCheckpointRequired)
	_, err := newTestReplier(e).runReviewedWork(ctx, &captureSink{}, func(context.Context, *replyReviewSink) (*TurnResult, error) {
		t.Fatal("mandatory checkpoint failure repeated production")
		return nil, nil
	})
	require.ErrorIs(t, err, memql.ErrWorkCheckpointRequired)
	require.True(t, work.IsOutcomeRecoveryFailure(err.Error()))
	require.Equal(t, 1, e.formats)
}

func TestLegacyFormatRecoveryAndQuestionNeverRunProduction(t *testing.T) {
	for _, question := range []bool{false, true} {
		t.Run(fmt.Sprint(question), func(t *testing.T) {
			e, ctx, messages := formatFixture(t)
			e.legacy = &memql.WorkDelegatedOutcome{SessionID: "old-session", Draft: `{}`, Messages: messages, AgentID: "assistant", AgentRole: "assistant"}
			if question {
				e.answer = `{"status":"needs_input","response":"","question":{"text":"Which reporting period?","kind":"text"}}`
			}
			r := newTestReplier(e)
			executor := &questionExecutor{}
			r.stamper = newToolRecorder(executor, r.logger)
			_, err := r.runReviewedWork(ctx, &captureSink{}, func(context.Context, *replyReviewSink) (*TurnResult, error) {
				t.Fatal("legacy recovery replayed production")
				return nil, nil
			})
			if question {
				var wait *work.HumanWait
				require.ErrorAs(t, err, &wait)
				require.Equal(t, 1, executor.calls)
			} else {
				require.NoError(t, err)
				require.Zero(t, executor.calls)
			}
			require.Equal(t, 1, e.formats)
		})
	}
}

func TestFormatRecoveryPreservesNewerSourcesAndRefusesLostSource(t *testing.T) {
	e, ctx, messages := formatFixture(t)
	e.sourceErr = errors.New("source lost")
	produce := func(context.Context, *replyReviewSink) (*TurnResult, error) {
		return nil, &delegatedOutcomeError{input: memql.WorkDelegatedOutcome{SessionID: "session", Messages: messages}, cause: errors.New("missing marker")}
	}
	_, err := newTestReplier(e).runReviewedWork(ctx, &captureSink{}, produce)
	require.ErrorContains(t, err, "source lost")
	require.Zero(t, e.formats)
	e.sourceErr = nil
	e.fail = true
	_, err = newTestReplier(e).runReviewedWork(ctx, &captureSink{}, produce)
	require.Error(t, err)
	e.savedContinuation = append(e.savedContinuation, common.ChatMessage{Role: "tool", Name: "newerReceipt", Content: "preserve me"})
	_, err = newTestReplier(e).runReviewedWork(ctx, &captureSink{}, produce)
	require.ErrorContains(t, err, "source changed")
	require.Equal(t, "preserve me", e.savedContinuation[len(e.savedContinuation)-1].Content)
	require.Equal(t, 1, e.formats)
}
