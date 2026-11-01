package memql

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/work"
)

// Delivering a routed message to the goals it names, by each goal's state
// (design record "Delivery by goal state"). A goal that is working, preparing
// or waiting on something other than the person takes WORK GUIDANCE: a target
// on the routed turn that its agent loop reads for the rest of the run. A goal
// waiting on the person takes the message through work.decideApproval, as
// guidance and never as a yes. A goal waiting on a review that takes no
// guidance is not delivered to.
//
// Delivery runs in two phases because the conversation's write lock is not
// reentrant and must never be held across a work builtin:
// deliverApprovalTargets answers requests outside the lock, then
// guidanceTargetsLocked records work guidance under it, judging each goal by
// its run as read under that lock.

// maxActiveAskGoals bounds the goals one message is checked against. What
// routing is shown of each goal is bounded by the askRoute* limits
// (ask_route_decide.go), the one routed message by askRouteMessageRunes.
const maxActiveAskGoals = 12

// routeDelivery is how a message reaches a goal.
type routeDelivery int

const (
	// deliverNone: the goal does not get the message.
	deliverNone routeDelivery = iota
	// deliverGuidance: work guidance on the routed turn, read by the agent loop.
	deliverGuidance
	// deliverApproval: through the request the goal waits on (decideApproval).
	deliverApproval
)

// nonPersonWaits are the waits a run resumes from without the person: the
// failure path's retry, re-plan and repair, a timer, something external and a
// sub-run. Work guidance waits with the run and is read when it resumes.
var nonPersonWaits = map[string]bool{"retry": true, "replan": true, "repair": true, "timer": true, "external": true, "subrun": true}

// activeAskGoals lists the goals still running in a conversation, newest
// first: one per root run that is preparing, working or waiting and not asked
// to stop, at most twelve, read fresh. Handles g1, g2, ... are the only names
// a routing model sees. A goal parked on a pending request carries it. A
// goal's request is what the turn that opened it asked of it: for a split
// message, the part intake received.
func (e *MemQLEngine) activeAskGoals(ctx context.Context, transcript askTranscript) ([]askRouteGoal, error) {
	return e.askGoals(ctx, transcript, false)
}

func (e *MemQLEngine) askGoals(ctx context.Context, transcript askTranscript, includeFinished bool) ([]askRouteGoal, error) {
	ctx = ContextWithFreshRead(ctx)
	turns := transcript.Turns
	// The turn that opened each run is the earliest that names it.
	opening := map[string]int{}
	for n := range turns {
		if key := BareShortId(turns[n].RunID); key != "" {
			if _, seen := opening[key]; !seen {
				opening[key] = n
			}
		}
	}
	var goals []askRouteGoal
	read := map[string]bool{}
	for n := len(turns) - 1; n >= 0 && len(goals) < maxActiveAskGoals; n-- {
		key := BareShortId(turns[n].RunID)
		if key == "" || read[key] {
			continue
		}
		read[key] = true
		rows, err := e.workRows(ctx, "workRunForOwner", turns[n].RunID)
		if err != nil {
			return nil, err
		}
		if len(rows) != 1 {
			continue
		}
		run, opened := rows[0], turns[opening[key]]
		continuation := ""
		if !activeWorkRun(run) {
			if !includeFinished {
				continue
			}
			continuation, err = e.askContinuationVersion(ctx, run)
			if err != nil {
				return nil, err
			}
			if continuation == "" {
				continue
			}
		}
		request := askTurnRequest(opened)
		title := runTitle(run)
		if strings.TrimSpace(title) == "" {
			title = opened.WorkTitle
		}
		if strings.TrimSpace(title) == "" {
			title = request
		}
		goals = append(goals, askRouteGoal{
			ContinueVersion: continuation,
			Handle:          "g" + strconv.Itoa(len(goals)+1),
			TurnID:          opened.ID,
			GoalID:          BareShortId(stringArg(run, "goalId")),
			RunID:           key,
			Title:           askRouteClip(title, askRouteTitleRunes),
			Request:         askRouteClip(request, askRouteSideRunes),
			Run:             run,
			Approval:        e.pendingRunApproval(ctx, run),
			Updates:         activeGoalUpdates(turns, key),
		})
	}
	return goals, nil
}

// pendingRunApproval is the request run is parked on while it can still take
// the person's message (approvalDiscussionGuidancePending): undecided and
// unexpired, or a native computer-access request past its expiry, which still
// takes a correction. Any other wait, or a request that cannot be read, was
// decided or has otherwise expired, carries none.
func (e *MemQLEngine) pendingRunApproval(ctx context.Context, run map[string]any) map[string]any {
	waiting, _ := run["waitingOn"].(map[string]any)
	subject := BareShortId(stringArg(waiting, "subject"))
	if stringArg(run, "status") != "waiting" || stringArg(waiting, "kind") != "approval" || subject == "" {
		return nil
	}
	approval, _, err := e.approvalRows(ctx, subject)
	if err != nil || !approvalDiscussionGuidancePending(approval, run) {
		return nil
	}
	return approval
}

// activeGoalUpdates is what earlier messages already delivered to run: their
// text, the most recent three, oldest first.
func activeGoalUpdates(turns []AskTurn, runID string) []string {
	var updates []string
	for n := range turns {
		if turns[n].Routed == nil {
			continue
		}
		for _, target := range turns[n].Routed.Targets {
			if BareShortId(target.RunID) == runID && routeDelivered(target.Status) {
				updates = append(updates, askRouteClip(target.Text, askRouteUpdateRunes))
			}
		}
	}
	if len(updates) > askRouteUpdates {
		updates = updates[len(updates)-askRouteUpdates:]
	}
	return updates
}

// waitsOnApproval reports whether run is parked on approval.
func waitsOnApproval(run, approval map[string]any) bool {
	waiting, _ := run["waitingOn"].(map[string]any)
	subject := BareShortId(stringArg(waiting, "subject"))
	return stringArg(run, "status") == "waiting" && stringArg(waiting, "kind") == "approval" &&
		subject != "" && subject == BareShortId(stringArg(approval, "id"))
}

// routeDeliveryFor is how a message reaches a goal in this state, and the
// status and reason its target starts with. approval is the request the run
// is parked on while it can still take a message (pendingRunApproval), or nil
// when there is none.
func routeDeliveryFor(run, approval map[string]any) (routeDelivery, string, string) {
	if !activeWorkRun(run) {
		return deliverNone, routeNotReceived, routeReasonEnded
	}
	if stringArg(run, "status") != "waiting" {
		return deliverGuidance, routeQueued, ""
	}
	waiting, _ := run["waitingOn"].(map[string]any)
	wait := stringArg(waiting, "kind")
	// A model that cannot be reached parks the run on a condition, not on
	// the person: it resumes by itself.
	if nonPersonWaits[wait] || (wait == "approval" && stringArg(waiting, "approvalKind") == work.ApprovalKindInferenceUnavailable) {
		return deliverGuidance, routeQueued, ""
	}
	if !waitsOnApproval(run, approval) {
		return deliverNone, routeNotDelivered, routeReasonReview
	}
	kind, options := stringArg(approval, "kind"), approval["options"]
	subject, _ := approval["subject"].(map[string]any)
	switch {
	case !work.ApprovalAcceptsGuidance(kind, subject, options):
		return deliverNone, routeNotDelivered, routeReasonReview
	case kind == work.ApprovalKindFeedback || work.IsFailureQuestion(options):
		return deliverApproval, routeAnswered, ""
	default:
		// sideEffect and scopeElevation: guidance instead of approval.
		return deliverApproval, routeNotApproved, ""
	}
}

// routeGoalDelivery is how a message reaches a goal as delivery judges it:
// routeDeliveryFor, except that a run waiting on something the request phase
// cannot judge -- a request that could not be read, one it is no longer
// parked on, a wait of another kind -- takes work guidance, read when it
// resumes (guidanceTargetsLocked). What a routing call is told a goal is
// doing comes from it too (askRouteGoalState), so the model is never told a
// goal will read a message that delivery then records Not delivered.
func routeGoalDelivery(run, approval map[string]any) (routeDelivery, string, string) {
	kind, status, reason := routeDeliveryFor(run, approval)
	if kind == deliverNone && activeWorkRun(run) && !waitsOnApproval(run, approval) {
		return deliverGuidance, routeQueued, ""
	}
	return kind, status, reason
}

// deliverApprovalTargets answers, as the person, each request a goal is
// parked on with the message as guidance: work.decideApproval {answered,
// {feedback: text}}, never an approval. Each request is read fresh first; a
// goal no longer parked on an answerable request, one whose request can no
// longer be read, and one that never was parked on one are returned unchanged
// in rest for the locked phase. It calls a work builtin, so it must never run
// under the conversation's write lock, and takes none.
func (e *MemQLEngine) deliverApprovalTargets(ctx context.Context, goals []askRouteGoal, text string) (delivered []AskRouteTarget, rest []askRouteGoal) {
	for _, goal := range goals {
		if kind, _, _ := routeDeliveryFor(goal.Run, goal.Approval); kind != deliverApproval {
			rest = append(rest, goal)
			continue
		}
		approvalID := BareShortId(stringArg(goal.Approval, "id"))
		approval, run, err := e.approvalRows(ctx, approvalID)
		if err != nil {
			rest = append(rest, goal)
			continue
		}
		if !approvalDiscussionGuidancePending(approval, run) {
			approval = nil
		}
		kind, status, _ := routeDeliveryFor(run, approval)
		if kind != deliverApproval {
			rest = append(rest, goal)
			continue
		}
		target := AskRouteTarget{GoalID: goal.GoalID, RunID: goal.RunID, Title: goal.Title, Status: status, ApprovalID: approvalID, Text: text}
		saved, err := e.answerApprovalWithGuidance(ctx, approvalID, text)
		switch {
		case err != nil:
			target.Status, target.Reason = routeNotDelivered, routeReasonRefused
		case saved:
			target.Status = routeSaved
		}
		delivered = append(delivered, target)
	}
	return delivered, rest
}

// answerApprovalWithGuidance sends text to one request as guidance and
// reports whether the request saved it rather than acting on it now.
func (e *MemQLEngine) answerApprovalWithGuidance(ctx context.Context, approvalID, text string) (bool, error) {
	call, err := parser.RenderCall("work.decideApproval", map[string]any{"approvalId": approvalID, "decision": "answered", "answer": map[string]any{"feedback": text}})
	if err != nil {
		return false, err
	}
	result, err := e.Execute(ctx, "builtin "+call)
	if err != nil {
		return false, err
	}
	rows := MaterializeRows(result.OutputPayload())
	if len(rows) != 1 {
		return false, fmt.Errorf("the request did not take this message")
	}
	return rows[0]["guidanceSaved"] == true, nil
}

// guidanceTargetsLocked records the message as work guidance for each goal
// the request phase left, judged by its run as read under the lock on
// conversationID (freshRuns, keyed by bare run id). It reads and writes
// nothing. A goal whose run ended did not receive it. A goal still parked on
// the request it was listed with is not given guidance: a review that takes
// none is Not delivered, and an answerable request the request phase did not
// answer was refused. Guidance reaches only a run whose own conversation is
// this one, so any other run refuses it. Every other active goal takes it as
// queued work guidance, unless that would hand its agent more than the read
// accepts.
func guidanceTargetsLocked(conversationID string, transcript askTranscript, goals []askRouteGoal, freshRuns map[string]map[string]any, text string) []AskRouteTarget {
	targets := make([]AskRouteTarget, 0, len(goals))
	for _, goal := range goals {
		target := AskRouteTarget{GoalID: goal.GoalID, RunID: goal.RunID, Title: goal.Title, Text: text}
		run := freshRuns[BareShortId(goal.RunID)]
		kind, status, reason := routeGoalDelivery(run, goal.Approval)
		switch {
		case !activeWorkRun(run):
			target.Status, target.Reason = routeNotReceived, routeReasonEnded
		case kind == deliverNone:
			// Still parked on the review it was listed with.
			target.ApprovalID, target.Status, target.Reason = BareShortId(stringArg(goal.Approval, "id")), status, reason
		case kind == deliverApproval:
			// Still parked on an answerable request the request phase did not
			// answer.
			target.ApprovalID, target.Status, target.Reason = BareShortId(stringArg(goal.Approval, "id")), routeNotDelivered, routeReasonRefused
		case !guidanceReaches(run, conversationID):
			target.Status, target.Reason = routeNotDelivered, routeReasonRefused
		case guidanceFits(text):
			target.Status = routeQueued
		default:
			target.Status, target.Reason = routeNotDelivered, routeReasonLimit
		}
		targets = append(targets, target)
	}
	return targets
}

// guidanceReaches reports whether work guidance recorded in conversationID can
// reach run. The agent reads a run's guidance only from the conversation on
// the run's own input; a run started outside Ask has none, even when it
// appears in a private conversation opened for it.
func guidanceReaches(run map[string]any, conversationID string) bool {
	own := BareShortId(guidanceConversation(run))
	return own != "" && own == BareShortId(conversationID)
}

// Each incoming message remains bounded. Existing durable history cannot make
// later human direction undeliverable; the reader pages that history instead.
func guidanceFits(text string) bool {
	return utf8.RuneCountInString(text) <= askRouteMessageRunes
}
