package agent

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"testing"
)

type completionReviewEngine struct {
	workPromptEngine
	reviews          int
	alwaysUnfinished bool
	information      bool
}

func (e *completionReviewEngine) RestoreWorkContinuation(_ context.Context, _ []common.ChatMessage) ([]common.ChatMessage, error) {
	return append([]common.ChatMessage(nil), e.savedContinuation...), nil
}
func (e *completionReviewEngine) Execute(ctx context.Context, q string) (any, error) {
	e.reviews++
	completion := map[string]any{"kind": "operation", "disposition": "unfinished", "gap": "Perform the remaining authorized save; preserve the verified first output.", "sourceRefs": []string{}}
	if e.information {
		completion = map[string]any{"kind": "information", "disposition": "satisfied", "gap": "", "sourceRefs": []string{}}
	} else if e.reviews > 1 && !e.alwaysUnfinished {
		completion = map[string]any{"kind": "operation", "disposition": "satisfied", "gap": "", "sourceRefs": []string{"source-1"}}
	}
	return map[string]any{"reply": "The current result is reported honestly.", "completion": completion}, nil
}
func TestOperationCompletionKeepsSpecialistEffectsAcrossBoundedRepairAndRestart(t *testing.T) {
	for _, staysUnfinished := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "unfinished"}[staysUnfinished], func(t *testing.T) {
			e := &completionReviewEngine{workPromptEngine: newProgressRecoveryEngine(t).workPromptEngine, alwaysUnfinished: staysUnfinished}
			ctx, _ := progressRecoveryContext()
			ctx = memql.ContextWithWorkReplyTask(ctx, "Retain two outputs.")
			productions, effects := 0, 0
			original := []common.ChatMessage{{Role: "system", Content: "Owned task"}, {Role: "user", Content: "Retain two outputs."}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: "first", Name: "saveFile", Arguments: `{}`}}}, {Role: "tool", ToolCallId: "first", Name: "saveFile", Content: `{"saved":"first"}`}}
			produce := func(_ context.Context, _ *replyReviewSink) (*TurnResult, error) {
				productions++
				if productions == 1 {
					effects++
					e.savedContinuation = append([]common.ChatMessage(nil), original...)
				} else {
					require.Equal(t, original, e.savedContinuation[:len(original)])
					require.Contains(t, e.savedContinuation[len(e.savedContinuation)-1].Content, "remaining authorized save")
				}
				// Specialist output has no respondToUser terminal envelope.
				return &TurnResult{FinalText: "Only the first output is saved."}, nil
			}
			sink := &captureSink{}
			result, err := newTestReplier(e).runReviewedWork(ctx, sink, produce)
			require.Equal(t, 2, productions)
			require.Equal(t, 1, effects)
			if staysUnfinished {
				require.Error(t, err)
				require.Nil(t, result)
				require.Empty(t, sink.texts)
				gap, ok := work.UnfinishedStepGap(err.Error())
				require.True(t, ok)
				require.Contains(t, gap, "remaining authorized save")
				_, err = newTestReplier(e).runReviewedWork(ctx, &captureSink{}, produce)
				require.Error(t, err)
				require.Equal(t, 2, productions, "a peer cannot reset the bounded repair or replay effects")
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, sink.texts, 1)
			}
		})
	}
}
func TestOperationCompletionAllowsInformationalSpecialistAndOwnerDeferral(t *testing.T) {
	for _, task := range []string{"Summarize the available evidence with conditional advice.", "Owner deferred publication; report the saved draft and keep it unpublished."} {
		t.Run(task, func(t *testing.T) {
			e := &completionReviewEngine{workPromptEngine: newProgressRecoveryEngine(t).workPromptEngine, information: true}
			e.savedContinuation = []common.ChatMessage{{Role: "system", Content: "Scope"}, {Role: "user", Content: task}}
			ctx, _ := progressRecoveryContext()
			ctx = memql.ContextWithWorkReplyTask(ctx, task)
			calls := 0
			_, err := newTestReplier(e).runReviewedWork(ctx, &captureSink{}, func(context.Context, *replyReviewSink) (*TurnResult, error) {
				calls++
				return &TurnResult{FinalText: "A bounded informational result."}, nil
			})
			require.NoError(t, err)
			require.Equal(t, 1, calls)
			require.Equal(t, 1, e.reviews)
		})
	}
}
func TestOperationCompletionRejectsMalformedDisposition(t *testing.T) {
	for _, completion := range []string{`null`, `{"kind":"operation","disposition":"satisfied","gap":"","sourceRefs":[]}`, `{"kind":"operation","disposition":"unfinished","gap":"","sourceRefs":[]}`, `{"kind":"information","disposition":"unfinished","gap":"do more","sourceRefs":[]}`} {
		var c *memql.WorkReplyCompletion
		require.NoError(t, json.Unmarshal([]byte(completion), &c))
		require.Error(t, c.Validate(nil))
	}
	require.False(t, memql.HasWorkReplyTask(context.Background()))
}

func TestOperationCompletionEmptyResultCannotFinishTheStep(t *testing.T) {
	for _, result := range []*TurnResult{nil, {}, {FinalText: "  "}} {
		e := &completionReviewEngine{workPromptEngine: newProgressRecoveryEngine(t).workPromptEngine}
		ctx, _ := progressRecoveryContext()
		ctx = memql.ContextWithWorkReplyTask(ctx, "Save the file")
		calls := 0
		_, err := newTestReplier(e).runReviewedWork(ctx, &captureSink{}, func(context.Context, *replyReviewSink) (*TurnResult, error) { calls++; return result, nil })
		require.Error(t, err)
		_, unfinished := work.UnfinishedStepGap(err.Error())
		require.True(t, unfinished)
		require.Equal(t, 1, calls, "an empty result must reconcile before any replay")
	}
}
