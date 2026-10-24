package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// Activity and tool receipts remain live. Proposed answer text and its terminal
// envelope are held until the same owned run's evidence check completes.
type replyReviewSink struct {
	DeltaSink
	terminalID, terminalArguments string
}

func (*replyReviewSink) TextDelta(string) {}
func (s *replyReviewSink) ToolCall(id, name, arguments string) {
	if name == RespondToUserToolName {
		s.terminalID, s.terminalArguments = id, arguments
		return
	}
	s.DeltaSink.ToolCall(id, name, arguments)
}

func (r *Replier) handleReviewedWork(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg, sink DeltaSink) (*TurnResult, error) {
	ctx, err := r.prepareReviewedWorkContext(ctx, msg)
	if err != nil {
		return nil, err
	}
	return r.runReviewedWork(ctx, sink, func(callCtx context.Context, held *replyReviewSink) (*TurnResult, error) {
		return r.handleTurn(callCtx, msg, held)
	})
}

func (r *Replier) prepareReviewedWorkContext(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg) (context.Context, error) {
	task := msg.Hints["work_section_prompt"]
	ctx = memql.ContextWithWorkReplyTask(ctx, task)
	if reader, ok := r.engine.(interface {
		ReadWorkReplanSources(context.Context) (string, error)
	}); ok {
		sources, err := reader.ReadWorkReplanSources(ctx)
		if err != nil {
			return ctx, err
		}
		ctx = memql.ContextWithWorkReplanSources(ctx, sources)
	}
	return ctx, nil
}

func (r *Replier) runReviewedWork(ctx context.Context, sink DeltaSink, produce func(context.Context, *replyReviewSink) (*TurnResult, error)) (delivered *TurnResult, failure error) {
	formatRecovery := false
	defer func() {
		var wait *work.HumanWait
		if formatRecovery && failure != nil && !errors.As(failure, &wait) && !work.IsOutcomeRecoveryFailure(failure.Error()) {
			// Every failure after retaining a completed App stays bound to that
			// stage, including a later review/storage failure. Retry cannot make
			// a new generation that forgets its consumed conversion allowance.
			failure = work.OutcomeRecoveryError(failure)
		}
	}()
	if !isOwnedWorkExecution(ctx) {
		return nil, fmt.Errorf("reply review requires owned work")
	}
	store, ok := r.engine.(workReplyReviewStore)
	if !ok {
		return nil, fmt.Errorf("%w: reply review checkpoint store unavailable", memql.ErrWorkCheckpointRequired)
	}
	// The native ceiling is one continuation. The installed recipe decides
	// whether to use it, and the original run retains every spend/deadline gate.
	for attempt := 0; attempt < 2; attempt++ {
		checkpoint, err := store.ReadWorkReplyReview(ctx)
		if err != nil {
			return nil, err
		}
		held := &replyReviewSink{DeltaSink: sink}
		var result *TurnResult
		var messages []common.ChatMessage
		var formatPending *pendingWorkReply
		sourceCompatible := true
		priorRepairs := 0
		if len(checkpoint.Pending) > 0 {
			pending, err := decodePendingWorkReply(checkpoint.Pending)
			if err != nil {
				return nil, err
			}
			formatRecovery = pending.OutcomeFormat != nil
			latest, err := r.restoreAfterQuestion(ctx, nil)
			if err != nil {
				return nil, fmt.Errorf("pending reply source checkpoint unavailable: %w", err)
			}
			if len(latest) == 0 {
				return nil, fmt.Errorf("%w: pending reply source checkpoint missing", memql.ErrWorkCheckpointRequired)
			}
			ctx = clearConsumedWorkContinuation(ctx)
			previousState, _, err := recoveryState(pending.Messages)
			if err != nil {
				return nil, err
			}
			latestState, _, err := recoveryState(latest)
			if err != nil {
				return nil, err
			}
			messages = latest
			priorRepairs = previousState.EvidenceRepairs
			// A crash between the evidence-continuation barrier and retiring the
			// draft must not resurrect the earlier repair allowance. New guidance
			// instead resumes production with completed exchanges, never the draft
			// as a substitute for the newly requested work.
			var added bool
			sourceCompatible, added = replySourceExtends(pending.Messages, latest)
			continued := latestState.EvidenceRepairs > previousState.EvidenceRepairs || added
			if pending.OutcomeFormat != nil && !pending.OutcomeFormat.Question {
				// A format failure never grants production, even after fresh
				// guidance. It retains its exact source and durable attempt bound.
				if checkpoint.Saved != checkpoint.Binding {
					return nil, fmt.Errorf("outcome recovery guidance changed; saved production retained")
				}
				if recoveryDigest(previousState) != recoveryDigest(latestState) {
					return nil, fmt.Errorf("outcome recovery progress state changed; newer checkpoint retained")
				}
				if !sourceCompatible || added {
					// Only the final terminal checkpoint may be one write behind
					// its already-saved stage. Any newer/different source stops.
					extends, extra := replySourceExtends(latest, pending.Messages)
					if !pending.OutcomeFormat.Done || !extends || !extra || len(pending.Messages) != len(latest)+2 {
						return nil, fmt.Errorf("outcome recovery source changed; newer progress retained")
					}
					if err := r.saveWorkProgress(ctx, pending.Messages); err != nil {
						return nil, err
					}
				}
				formatPending = pending
				result, messages = pending.Result, pending.Messages
				held.terminalID, held.terminalArguments = pending.TerminalID, pending.TerminalArguments
			} else if checkpoint.Saved.Guidance != checkpoint.Binding.Guidance || continued {
				if !sourceCompatible {
					return nil, fmt.Errorf("%w: pending reply source changed before production continuation", memql.ErrWorkCheckpointRequired)
				}
				if err := savePendingWorkReply(ctx, store, checkpoint.Binding, nil); err != nil {
					return nil, err
				}
				formatRecovery = false
			} else {
				if pending.OutcomeFormat != nil && pending.OutcomeFormat.Question {
					return nil, fmt.Errorf("outcome recovery awaits its recorded question")
				}
				result = pending.Result
				held.terminalID, held.terminalArguments = pending.TerminalID, pending.TerminalArguments
			}
		}
		if result == nil {
			if runtime, ok := r.engine.(outcomeRecoveryRuntime); ok && len(checkpoint.Pending) == 0 {
				legacy, readErr := runtime.ReadLegacyWorkOutcome(ctx)
				if readErr != nil {
					return nil, work.OutcomeRecoveryError(readErr)
				}
				if legacy != nil {
					formatRecovery = true
					// Validate any native approval proof before upgrading old state.
					if _, err := r.restoreAfterQuestion(ctx, nil); err != nil {
						return nil, err
					}
					ctx = clearConsumedWorkContinuation(ctx)
					formatPending, err = r.saveOutcomeDraft(ctx, store, checkpoint.Binding, *legacy, false)
					if err != nil {
						return nil, work.OutcomeRecoveryError(err)
					}
				}
			}
			if formatPending == nil {
				result, err = produce(ctx, held)
				if err != nil {
					var failure *delegatedOutcomeError
					if !errors.As(err, &failure) {
						return nil, err
					}
					formatRecovery = true
					ctx = clearConsumedWorkContinuation(ctx)
					formatPending, err = r.saveOutcomeDraft(ctx, store, checkpoint.Binding, failure.input, true)
					if err != nil {
						return nil, work.OutcomeRecoveryError(err)
					}
				}
			}
			if formatPending != nil {
				result, messages = formatPending.Result, formatPending.Messages
			}
			if result != nil && result.Paused {
				return result, nil
			}
			if formatPending == nil && (result == nil || strings.TrimSpace(result.FinalText) == "") {
				if memql.HasWorkReplyTask(ctx) {
					return nil, work.UnfinishedStepError("The current step returned no completed result. Reconcile its retained tool receipts, then finish or verify the required output.")
				}
				return result, nil
			}
			ctx = clearConsumedWorkContinuation(ctx)
			if formatPending == nil {
				messages, err = r.restoreAfterQuestion(ctx, nil)
				if err != nil {
					return nil, err
				}
				pending := &pendingWorkReply{Version: 1, Result: result, TerminalID: held.terminalID, TerminalArguments: held.terminalArguments, Messages: messages}
				if err := savePendingWorkReply(ctx, store, checkpoint.Binding, pending); err != nil {
					return nil, err
				}
			}
		}
		if formatPending != nil {
			if err := r.repairOutcomeDraft(ctx, store, checkpoint.Binding, formatPending, held); err != nil {
				var wait *work.HumanWait
				if !errors.As(err, &wait) {
					err = work.OutcomeRecoveryError(err)
				}
				return nil, err
			}
			result, messages = formatPending.Result, formatPending.Messages
		}
		current, err := store.ReadWorkReplyReview(ctx)
		if err != nil || current.Binding != checkpoint.Binding {
			return nil, fmt.Errorf("reply review execution or guidance changed before review: %v", err)
		}
		state, _, err := recoveryState(messages)
		if err != nil {
			return nil, err
		}
		canContinue := formatPending == nil && attempt == 0 && sourceCompatible && len(messages) > 1 && state.EvidenceRepairs == 0 && priorRepairs == 0
		review, err := r.checkWorkResult(memql.ContextWithWorkReplyReviewSource(ctx, messages), result.FinalText, canContinue)
		if err != nil {
			// Review is a reasoning boundary after all tool receipts are saved.
			// A native ceiling must suspend this exact checkpoint; a raw ceiling
			// error would leave a failed step that feedback cannot resume.
			failure, _ := r.workRecoveryReadFailure(ctx, err, messages)
			return nil, failure
		}
		gap := review.MissingEvidence
		if review.Completion != nil && review.Completion.Disposition == "unfinished" {
			if formatPending != nil {
				return nil, work.OutcomeRecoveryError(fmt.Errorf("%w: work_outcome_format_unverified; retained result requires review, production cannot be replayed", memql.ErrWorkCheckpointRequired))
			}
			gap = review.Completion.Gap
		}
		continued, err := r.continueWorkEvidence(ctx, messages, gap, canContinue)
		if err != nil {
			return nil, err
		}
		if continued {
			if err := savePendingWorkReply(ctx, store, checkpoint.Binding, nil); err != nil {
				return nil, err
			}
			continue
		}
		// Review may be slow. Recheck current authority and direction before
		// exposing its answer; a saved draft never grants a completion claim.
		current, err = store.ReadWorkReplyReview(ctx)
		if err != nil || current.Binding != checkpoint.Binding {
			return nil, fmt.Errorf("reply review execution or guidance changed before delivery: %v", err)
		}
		if review.Completion != nil && review.Completion.Disposition == "unfinished" {
			// Preserve the pending result and exact operational source. A native plan
			// continuation handles the remainder; never turn unfinished work into a
			// successful terminal reply or manufacture a question for the owner.
			return nil, work.UnfinishedStepError(review.Completion.Gap)
		}
		return deliverReviewedWorkResult(result, held, review.Reply)
	}
	return nil, fmt.Errorf("reply evidence continuation exceeded its bound")
}

type workReplyDecision struct {
	Reply, MissingEvidence string
	Completion             *memql.WorkReplyCompletion
}

func (r *Replier) continueWorkEvidence(ctx context.Context, messages []common.ChatMessage, gap string, available bool) (bool, error) {
	state, marker, err := recoveryState(messages)
	if err != nil {
		return false, err
	}
	value, err := workflowhost.Run(ctx, "agentWorkReplyCompletion", map[string]any{"facts": map[string]any{
		"continuationAvailable": available, "missingEvidence": gap, "repairs": state.EvidenceRepairs,
	}}, workflowhost.Options{})
	if err != nil {
		return false, err
	}
	proceed, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("reply completion policy must return a boolean")
	}
	if !proceed {
		return false, nil
	}
	// Authored policy cannot reset the durable attempt count or invent a
	// continuation without a bounded gap and an existing source checkpoint.
	if !available || len(messages) < 2 || state.EvidenceRepairs != 0 || strings.TrimSpace(gap) == "" || len(gap) > 1200 {
		return false, fmt.Errorf("reply evidence continuation is unavailable")
	}
	instruction, err := r.engine.RenderPrompt("workEvidenceCompletion", map[string]any{"gap": gap})
	if err != nil {
		return false, err
	}
	state.EvidenceRepairs, state.EvidenceGap = 1, gap
	raw, err := json.Marshal(state)
	if err != nil {
		return false, err
	}
	out := append([]common.ChatMessage(nil), messages...)
	if marker >= 0 {
		out = append(out[:marker], out[marker+1:]...)
	}
	out = append(out, common.ChatMessage{Role: "system", Name: workRecoveryMarker, Content: string(raw)})
	// A new reasoning boundary needs an explicit correction, as with invalid
	// outcome repair. Metadata alone was carried faithfully yet ignored by the
	// live model. This is marked runtime input, never a new human authorization.
	out = append(out, common.ChatMessage{Role: "user", Name: "work_evidence_completion", Content: instruction})
	// Save before any next call. A different replica sees the same bound and
	// the same completed source receipts; it cannot restart the repair allowance.
	if err := r.saveWorkProgress(ctx, out); err != nil {
		return false, err
	}
	return true, nil
}

func (r *Replier) checkWorkResult(ctx context.Context, draft string, canContinue bool) (workReplyDecision, error) {
	completionRequired := memql.HasWorkReplyTask(ctx)
	call, err := parser.RenderCall("work.workReviewReply", map[string]any{"draft": map[string]any{"reply": draft, "continueLookup": canContinue, "reviewCompletion": completionRequired}})
	if err != nil {
		return workReplyDecision{}, err
	}
	raw, err := r.engine.Execute(ctx, "builtin "+call)
	if err != nil {
		return workReplyDecision{}, err
	}
	rows := memql.MaterializeRows(raw)
	if len(rows) != 1 {
		return workReplyDecision{}, fmt.Errorf("reply evidence review returned no answer")
	}
	reply, ok := rows[0]["reply"].(string)
	if !ok || strings.TrimSpace(reply) == "" {
		return workReplyDecision{}, fmt.Errorf("reply evidence review returned an empty answer")
	}
	gap, _ := rows[0]["missingEvidence"].(string)
	if len(gap) > 1200 {
		return workReplyDecision{}, fmt.Errorf("reply evidence gap exceeds its bound")
	}
	if !canContinue {
		gap = ""
	}
	var completion *memql.WorkReplyCompletion
	if completionRequired {
		raw, err := json.Marshal(rows[0]["completion"])
		if err != nil || json.Unmarshal(raw, &completion) != nil {
			return workReplyDecision{}, fmt.Errorf("reply completion returned invalid data")
		}
		if err := completion.Validate(nil); err != nil {
			return workReplyDecision{}, err
		}
	}
	return workReplyDecision{Reply: reply, MissingEvidence: gap, Completion: completion}, nil
}

func deliverReviewedWorkResult(result *TurnResult, held *replyReviewSink, reply string) (*TurnResult, error) {
	if held.terminalID != "" {
		var envelope map[string]any
		if err := json.Unmarshal([]byte(held.terminalArguments), &envelope); err != nil {
			return nil, err
		}
		envelope["response"] = reply
		delete(envelope, "responseFromText")
		encoded, err := json.Marshal(envelope)
		if err != nil {
			return nil, err
		}
		held.DeltaSink.ToolCall(held.terminalID, RespondToUserToolName, string(encoded))
	}
	result.FinalText, result.TextChunks = reply, 1
	held.DeltaSink.TextDelta(reply)
	return result, nil
}
