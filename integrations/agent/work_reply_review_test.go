package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/common"
)

type replyReviewEngine struct {
	workPromptEngine
	calls int
	fail  bool
}

func (e *replyReviewEngine) Execute(ctx context.Context, query string) (any, error) {
	e.calls++
	run, _ := common.RunFromContext(ctx)
	actor, _ := auth.AccessFromContext(ctx)
	if run.RunId != "run" || actor == nil || actor.UserId != "owner" || !strings.HasPrefix(query, "builtin work.workReviewReply(") {
		return nil, errors.New("lost owned execution context")
	}
	if e.fail {
		return nil, errors.New("review unavailable")
	}
	// The real CognitionEngineAdapter retains the typed builtin node set under
	// data; a flat fake concealed the production result-wrapper boundary.
	return map[string]any{"data": map[string]memorynodes.MemoryNode{
		"work-reviewed-reply": {ID: "work-reviewed-reply", Payload: json.RawMessage(`{"reply":"The recorded observation does not establish a cause."}`)},
	}}, nil
}

func TestOwnedAnswerWaitsForEvidenceReviewBeforeDelivery(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "reviewed", true: "failed"}[fail], func(t *testing.T) {
			engine := &replyReviewEngine{fail: fail}
			r := newTestReplier(engine)
			ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner"})
			sink := &captureSink{}
			held := &replyReviewSink{DeltaSink: sink}
			held.ToolCall("read", "readStatus", `{}`)
			held.ToolResult("read", `{"status":"degraded"}`, "")
			held.TextDelta("An invented cause.")
			held.ToolCall("answer", RespondToUserToolName, `{"response":"An invented cause."}`)
			require.Empty(t, sink.texts)
			require.Equal(t, []string{"readStatus"}, sink.toolCalls)
			require.Equal(t, 1, sink.toolResults)
			result, err := r.runReviewedWork(ctx, sink, func(callCtx context.Context, h *replyReviewSink) (*TurnResult, error) {
				h.terminalID, h.terminalArguments = held.terminalID, held.terminalArguments
				return &TurnResult{FinalText: "An invented cause."}, nil
			})
			require.Equal(t, 1, engine.calls)
			if fail {
				require.Error(t, err)
				require.Nil(t, result)
				require.Empty(t, sink.texts)
				require.Equal(t, []string{"readStatus"}, sink.toolCalls)
			} else {
				require.NoError(t, err)
				require.Equal(t, []string{result.FinalText}, sink.texts)
				require.NotContains(t, result.FinalText, "invented")
				require.Equal(t, []string{"readStatus", RespondToUserToolName}, sink.toolCalls)
			}
		})
	}
}

type evidenceRepairEngine struct {
	workPromptEngine
	reviews  int
	failSave bool
}

func (e *evidenceRepairEngine) RestoreWorkContinuation(_ context.Context, _ []common.ChatMessage) ([]common.ChatMessage, error) {
	return append([]common.ChatMessage(nil), e.savedContinuation...), nil
}
func (e *evidenceRepairEngine) SaveWorkContinuation(ctx context.Context, m []common.ChatMessage) error {
	if e.failSave {
		return errors.New("checkpoint unavailable")
	}
	return e.workPromptEngine.SaveWorkContinuation(ctx, m)
}
func (e *evidenceRepairEngine) Execute(ctx context.Context, query string) (any, error) {
	run, _ := common.RunFromContext(ctx)
	if run.RunId != "run" || !strings.HasPrefix(query, "builtin work.workReviewReply(") {
		return nil, errors.New("lost original review scope")
	}
	e.reviews++
	reply, gap := "The exact item has not been identified.", "Resolve the requested item's identifier from a named source; reuse the recorded response."
	if e.reviews > 1 {
		reply = "The named source identifies the requested item and its current status."
		gap = ""
	}
	return map[string]any{"data": map[string]any{"review": map[string]any{"id": "review", "payload": map[string]any{"reply": reply, "missingEvidence": gap}}}}, nil
}
func TestEvidenceGapContinuesSameTaskOnceWithoutDeliveringPrematureAnswer(t *testing.T) {
	e := &evidenceRepairEngine{workPromptEngine: newProgressRecoveryEngine(t).workPromptEngine}
	e.savedContinuation = []common.ChatMessage{{Role: "system", Content: "Current instructions"}, {Role: "user", Content: "Check item A"}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: "source", Name: "readSource", Arguments: `{}`}}}, {Role: "tool", ToolCallId: "source", Name: "readSource", Content: `{"item":"B","status":"ready"}`}}
	original := append([]common.ChatMessage(nil), e.savedContinuation...)
	ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", StepKey: "reason", OwnerUserId: "owner", Continuation: &common.StepContinuation{StepKey: "reason", ObservationID: "original"}})
	sink := &captureSink{}
	productions := 0
	r := newTestReplier(e)
	result, err := r.runReviewedWork(ctx, sink, func(callCtx context.Context, h *replyReviewSink) (*TurnResult, error) {
		productions++
		current, _ := common.RunFromContext(callCtx)
		require.Equal(t, "run", current.RunId)
		require.Equal(t, "reason", current.StepKey)
		require.Equal(t, "owner", current.OwnerUserId)
		if productions == 1 {
			require.NotNil(t, current.Continuation)
		} else {
			require.Nil(t, current.Continuation, "a consumed resume proof must not reject the newer checkpoint")
		}
		if productions == 2 {
			require.Empty(t, sink.texts, "incomplete answer escaped before continuation")
			require.Equal(t, original, e.savedContinuation[:len(original)], "completed source was replayed or lost")
			state, _, err := recoveryState(e.savedContinuation)
			require.NoError(t, err)
			require.Equal(t, 1, state.EvidenceRepairs)
			require.Contains(t, state.EvidenceGap, "identifier")
		}
		h.ToolCall("answer", RespondToUserToolName, `{"status":"complete","response":"Unsupported draft"}`)
		return &TurnResult{FinalText: "Unsupported draft"}, nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, productions)
	require.Equal(t, 2, e.reviews)
	require.Equal(t, []string{result.FinalText}, sink.texts)
	require.Equal(t, []string{RespondToUserToolName}, sink.toolCalls)
	// A replacement replier has no local attempt state; the durable watermark
	// must still refuse another evidence repair even if asked to continue.
	other := newTestReplier(e)
	continued, err := other.continueWorkEvidence(ctx, e.savedContinuation, "Try another guessed identifier", true)
	require.NoError(t, err)
	require.False(t, continued)
}
func TestEvidenceRepairCannotProceedWithoutDurableCheckpoint(t *testing.T) {
	e := &evidenceRepairEngine{workPromptEngine: newProgressRecoveryEngine(t).workPromptEngine, failSave: true}
	r := newTestReplier(e)
	ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner"})
	continued, err := r.continueWorkEvidence(ctx, []common.ChatMessage{{Role: "system"}, {Role: "user", Content: "Check item"}}, "Resolve the item identifier", true)
	require.Error(t, err)
	require.False(t, continued)
	require.Empty(t, e.savedContinuation)
}

func TestEvidenceCompletionReachesBothProviderBoundariesAfterRestore(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstreaming", true: "streaming"}[streaming], func(t *testing.T) {
			e := &evidenceRepairEngine{workPromptEngine: newProgressRecoveryEngine(t).workPromptEngine}
			r := newTestReplier(e)
			ctx, turn := progressRecoveryContext()
			original := []common.ChatMessage{{Role: "system", Content: "Current instructions"}, {Role: "user", Content: "Check item A"}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: "source", Name: "readSource", Arguments: `{}`}}}, {Role: "tool", ToolCallId: "source", Name: "readSource", Content: `{"item":"B","status":"ready"}`}}
			continued, err := r.continueWorkEvidence(ctx, original, "Resolve item A's identifier", true)
			require.NoError(t, err)
			require.True(t, continued)
			// Fresh input simulates another host, with none of the correction's
			// local state. The provider must see the persisted explicit instruction.
			provider := &outcomeProvider{steps: [][]common.ToolCall{nil}, texts: []string{"Answer"}}
			fresh := []common.ChatMessage{{Role: "system", Content: "Refreshed instructions"}}
			if streaming {
				_, err = r.runStreamingToolLoop(ctx, provider, fresh, nil, &captureSink{}, time.Now(), "completion", turn)
			} else {
				_, err = r.runNonStreamingToolLoop(ctx, provider, nil, fresh, nil, &captureSink{}, time.Now(), "completion", turn)
			}
			require.NoError(t, err)
			require.Equal(t, 1, provider.calls)
			require.Equal(t, original, provider.messages[:len(original)])
			last := provider.messages[len(provider.messages)-1]
			require.Equal(t, "user", last.Role)
			require.Equal(t, "work_evidence_completion", last.Name)
			require.Contains(t, last.Content, "not a new user request")
			require.Contains(t, last.Content, "Resolve item A's identifier")
			require.Contains(t, last.Content, "authorized tool before")
			require.True(t, recoveryGuidance(last).IsZero(), "runtime correction must not rearm human authorization")
		})
	}
}
