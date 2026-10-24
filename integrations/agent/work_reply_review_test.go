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
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

type replyReviewEngine struct {
	workPromptEngine
	calls        int
	fail         bool
	restoreErr   error
	saveReplyErr error
}

func (e *replyReviewEngine) RestoreWorkContinuation(_ context.Context, _ []common.ChatMessage) ([]common.ChatMessage, error) {
	return append([]common.ChatMessage(nil), e.savedContinuation...), e.restoreErr
}

func (e *replyReviewEngine) SaveWorkReplyReview(ctx context.Context, binding memql.WorkReplyReviewBinding, value json.RawMessage) error {
	if e.saveReplyErr != nil {
		return e.saveReplyErr
	}
	return e.workPromptEngine.SaveWorkReplyReview(ctx, binding, value)
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
			engine.savedContinuation = []common.ChatMessage{{Role: "user", Content: "Explain the observed status."}}
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

type restartReplyReviewEngine struct{ replyReviewEngine }

func (e *restartReplyReviewEngine) RestoreWorkContinuation(_ context.Context, _ []common.ChatMessage) ([]common.ChatMessage, error) {
	return append([]common.ChatMessage(nil), e.savedContinuation...), nil
}

func TestReplyReviewRestartDoesNotRepeatCompletedProduction(t *testing.T) {
	e := &restartReplyReviewEngine{replyReviewEngine: replyReviewEngine{fail: true}}
	ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", StepKey: "finish", OwnerUserId: "owner"})
	productions, effects := 0, 0
	produce := func(_ context.Context, held *replyReviewSink) (*TurnResult, error) {
		productions++
		effects++
		e.savedContinuation = []common.ChatMessage{
			{Role: "system", Content: "Current instructions"},
			{Role: "user", Content: "Save the requested file once."},
			{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "save", Name: "saveFile", Arguments: `{}`}}},
			{Role: "tool", ToolCallId: "save", Name: "saveFile", Content: `{"fileId":"saved-file","verified":true}`},
		}
		held.ToolCall("answer", RespondToUserToolName, `{"status":"complete","response":"Saved the requested file."}`)
		return &TurnResult{FinalText: "Saved the requested file."}, nil
	}
	firstSink := &captureSink{}
	_, err := newTestReplier(e).runReviewedWork(ctx, firstSink, produce)
	require.ErrorContains(t, err, "review unavailable")
	require.Empty(t, firstSink.texts, "an unchecked draft must remain private")
	e.fail = false
	secondSink := &captureSink{}
	result, err := newTestReplier(e).runReviewedWork(ctx, secondSink, produce)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, e.calls, "the failed review is retried on the new replier")
	require.Equal(t, 1, productions, "resuming review must not rerun the completed operational App")
	require.Equal(t, 1, effects, "the completed effect must not be replayed")
	require.Equal(t, []string{result.FinalText}, secondSink.texts)
	require.Equal(t, []string{RespondToUserToolName}, secondSink.toolCalls, "terminal delivery resumes with the saved result")
	// Delivery and the workflow journal are separate commits. If delivery was
	// uncertain, another entry may review/deliver again but cannot replay effects.
	_, err = newTestReplier(e).runReviewedWork(ctx, &captureSink{}, produce)
	require.NoError(t, err)
	require.Equal(t, 1, productions)
	require.Equal(t, 3, e.calls)
}

func TestReplyReviewCheckpointFailureStopsBeforeReview(t *testing.T) {
	e := &replyReviewEngine{saveReplyErr: errors.New("store unavailable")}
	e.savedContinuation = []common.ChatMessage{{Role: "user", Content: "Complete work"}}
	ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner"})
	sink := &captureSink{}
	_, err := newTestReplier(e).runReviewedWork(ctx, sink, func(context.Context, *replyReviewSink) (*TurnResult, error) {
		return &TurnResult{FinalText: "Completed"}, nil
	})
	require.ErrorIs(t, err, memql.ErrWorkCheckpointRequired)
	require.Zero(t, e.calls)
	require.Empty(t, sink.texts)
}

func TestReplyReviewRestartPreservesNewerProgress(t *testing.T) {
	for _, change := range []string{"watermark", "receipts", "guidance", "helper", "helper-guidance", "proof", "missing", "corrupt", "cancelled"} {
		t.Run(change, func(t *testing.T) {
			e := &replyReviewEngine{fail: true}
			ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", StepKey: "finish", OwnerUserId: "owner"})
			original := []common.ChatMessage{{Role: "system", Content: "Instructions"}, {Role: "user", Content: "Save once"}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: "saved", Name: "saveFile", Arguments: `{}`}}}, {Role: "tool", ToolCallId: "saved", Name: "saveFile", Content: `{"saved":true}`}}
			e.savedContinuation = append([]common.ChatMessage(nil), original...)
			productions := 0
			produce := func(context.Context, *replyReviewSink) (*TurnResult, error) {
				productions++
				return &TurnResult{FinalText: "Saved once"}, nil
			}
			_, err := newTestReplier(e).runReviewedWork(ctx, &captureSink{}, produce)
			require.ErrorContains(t, err, "review unavailable")
			e.fail = false
			expectProduce, wantError := false, false
			switch change {
			case "watermark":
				state := workRecoveryState{Version: 1, DiscoveryRounds: 4, RepeatedRounds: 3, ReviewActive: true, StalledAfterReview: 2, ReviewedLimit: 10, GuidanceIDs: []string{"current-guidance"}, GuidanceCursor: memql.WorkGuidanceCursor{Count: 2, Hash: strings.Repeat("a", 64)}}
				raw, err := json.Marshal(state)
				require.NoError(t, err)
				e.savedContinuation = append(e.savedContinuation, common.ChatMessage{Role: "system", Name: workRecoveryMarker, Content: string(raw)})
			case "receipts":
				e.savedContinuation = append(e.savedContinuation, common.ChatMessage{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "new", Name: "readStatus", Arguments: `{}`}}}, common.ChatMessage{Role: "tool", ToolCallId: "new", Content: `{"ready":true}`})
				expectProduce = true
			case "guidance":
				e.replyCheckpoint.Binding.Guidance = "new-direction"
				expectProduce = true
			case "helper", "helper-guidance":
				e.savedContinuation = []common.ChatMessage{{Role: "system", Content: "Helper recovery checkpoint"}, {Role: "user", Content: "Review sources"}}
				if change == "helper-guidance" {
					e.replyCheckpoint.Binding.Guidance = "new-direction"
					wantError = true
				}
			case "proof":
				e.restoreErr = errors.New("exact answered checkpoint changed")
				wantError = true
			case "missing":
				e.savedContinuation = nil
				wantError = true
			case "corrupt":
				e.replyCheckpoint.Pending = json.RawMessage(`{"version":1}`)
				wantError = true
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				wantError = true
			}
			latest := append([]common.ChatMessage(nil), e.savedContinuation...)
			sink := &captureSink{}
			_, err = newTestReplier(e).runReviewedWork(ctx, sink, produce)
			if wantError {
				require.Error(t, err)
				require.Empty(t, sink.texts)
			} else {
				require.NoError(t, err)
			}
			wantProductions := 1
			if expectProduce {
				wantProductions++
			}
			if len(latest) == 0 {
				require.Empty(t, e.savedContinuation)
			} else {
				require.Equal(t, latest, e.savedContinuation, "review retry must not overwrite newer receipts or recovery state")
			}
			require.Equal(t, wantProductions, productions)
		})
	}
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
