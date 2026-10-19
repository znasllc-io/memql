package memql

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
)

// stopAskGoal closes one of the caller's goals and asks every run of it to
// stop, as the person. Cancellation is requested, not done: a run notices at
// its next step boundary. It succeeds only when the goal closed and no run
// refused the request. It is a work builtin, so it is never called while the
// conversation's write lock is held.
func (e *MemQLEngine) stopAskGoal(ctx context.Context, goalID string) error {
	call, err := parser.RenderCall("work.cancelGoal", map[string]any{"goalId": goalID, "reason": "Stopped from Ask."})
	if err != nil {
		return err
	}
	result, err := e.Execute(ctx, "builtin "+call)
	if err != nil {
		return err
	}
	rows := MaterializeRows(result.OutputPayload())
	if len(rows) != 1 || rows[0]["goalClosed"] != true {
		return fmt.Errorf("this work could not be stopped; try again")
	}
	if refused, _ := rows[0]["runsRefused"].([]any); len(refused) > 0 {
		return fmt.Errorf("this work could not be stopped; try again")
	}
	return nil
}

// askStopQuestion finds the stop question turnID asked about runID.
func askStopQuestion(transcript *askTranscript, turnID, runID string) *AskStopQuestion {
	for n := range transcript.Turns {
		turn := &transcript.Turns[n]
		if turn.ID != turnID || turn.Routed == nil {
			continue
		}
		for s := range turn.Routed.Stops {
			if BareShortId(turn.Routed.Stops[s].RunID) == BareShortId(runID) {
				return &turn.Routed.Stops[s]
			}
		}
	}
	return nil
}

// askStopDecisionBuiltin records a person's answer to a stop question Ask
// asked on one of their goals. stop closes the goal first and then records
// stopped; keep records kept and changes nothing else. The same answer again
// returns the saved one; the other answer, once one is saved, is refused.
func (e *MemQLEngine) askStopDecisionBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if rc, ok := common.RunFromContext(ctx); ok && rc.RunId != "" {
		return nil, fmt.Errorf("stopping work needs a person's decision")
	}
	if subject, ok := auth.SubjectFromContext(ctx); !ok || !auth.CapableFor(ctx, subject, "read", "app:ask") {
		return nil, fmt.Errorf("sign in to stop work")
	}
	conversationID, turnID, runID := stringArg(args, "conversationId"), stringArg(args, "turnId"), stringArg(args, "runId")
	decision := map[string]string{"stop": stopDecisionStopped, "keep": stopDecisionKept}[stringArg(args, "decision")]
	if conversationID == "" || turnID == "" || len(turnID) > 160 || BareShortId(runID) == "" || decision == "" {
		return nil, fmt.Errorf("a stop decision needs its conversation, message and work, and stop or keep")
	}
	result := func(question *AskStopQuestion) ([]memorynodes.MemoryNode, error) {
		return askValue(map[string]any{"conversationId": BareShortId(conversationID), "turnId": turnID, "runId": BareShortId(question.RunID), "decision": question.Decision})
	}
	// Resolve ownership before acting on the goal or taking a caller-selected lock.
	row, err := e.askRead(ContextWithFreshRead(ctx), conversationID)
	if err != nil {
		return nil, err
	}
	transcript, err := approvalTranscript(row)
	if err != nil {
		return nil, err
	}
	question := askStopQuestion(&transcript, turnID, runID)
	if question == nil {
		return nil, fmt.Errorf("this stop question is unavailable")
	}
	switch question.Decision {
	case decision:
		return result(question)
	case "":
	default:
		return nil, fmt.Errorf("this stop question is already decided")
	}
	// Cancelling is a work builtin and the conversation lock is not reentrant,
	// so the goal is stopped before the lock is taken.
	if decision == stopDecisionStopped {
		if err := e.stopAskGoal(ctx, question.GoalID); err != nil {
			return nil, err
		}
	}
	hold, err := e.holdAskConversation(ctx, conversationID, "write")
	if err != nil {
		return nil, err
	}
	defer hold.release()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	row, err = e.askRead(ContextWithFreshRead(ctx), conversationID)
	if err != nil {
		return nil, err
	}
	transcript, err = approvalTranscript(row)
	if err != nil {
		return nil, err
	}
	question = askStopQuestion(&transcript, turnID, runID)
	if question == nil {
		return nil, fmt.Errorf("this stop question is unavailable")
	}
	if question.Decision == decision {
		return result(question)
	}
	// Another answer was saved meanwhile. A keep never overwrites it; a stop
	// does, because the goal was just closed and the record must say so.
	if question.Decision != "" && decision != stopDecisionStopped {
		return nil, fmt.Errorf("this stop question is already decided")
	}
	question.Decision = decision
	if err := hold.verify(ctx); err != nil {
		return nil, err
	}
	if err := e.askSave(ctx, conversationID, stringArg(row, "title"), transcript); err != nil {
		return nil, err
	}
	return result(question)
}
