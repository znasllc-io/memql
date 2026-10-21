package memql

// ask_route_apply.go -- RunAsk routes a person's new message to the goals
// already running in their conversation before intake (design record
// docs/superpowers/specs/2026-10-19-ask-message-routing-design.md, "Flow",
// "Apply" and "Failure handling").
//
// THE ORDER IS THE LOCKING. The conversation's write lock is not reentrant and
// is never held across a model call or a work builtin, so routing runs in
// phases: the decision, then every work builtin that acts on a goal (a
// confirmed stop, an answer to the request a goal waits on), all outside the
// lock; then ONE locked write that records what reached each goal, judging
// each goal still to be given work guidance by its run as read under that
// lock; then, unlocked again, the new titles.
//
// EVERY EFFECT IS DURABLE. What routing did is the transcript turn, the run
// rows and the request rows, so a turn routed on one replica and resent to
// another is answered from that record and never routed or delivered twice.

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// Why routing fell back to intake (AskRouting.Skipped).
const (
	routeSkippedUnavailable = "unavailable"
	routeSkippedInvalid     = "invalid"
)

// askRouteApplyTimeout bounds applying a decision once it is made. The person
// may leave after that, and still gets what they asked for: the apply phase
// does not inherit their request's cancellation, so a decision is applied and
// recorded whole or not at all.
const askRouteApplyTimeout = 30 * time.Second

// askRouteOutcome is what routing did with one message. Intake says the
// message still goes to intake, with Statement as its request; otherwise
// Answer is the turn's whole answer. Routed is what the turn records of its
// routing, nil for a message routing left alone.
type askRouteOutcome struct {
	Intake            bool
	Statement, Answer string
	Routed            *AskRouting
}

// askRouteEligible reports whether a new message is routed at all: guidance
// carries text only, and at most one message's worth of it.
func askRouteEligible(prompt string, attachments []AskAttachment) bool {
	return len(attachments) == 0 && utf8.RuneCountInString(prompt) <= askRouteMessageRunes
}

// routeAskTurn decides where one new message goes and applies the decision.
// RunAsk calls it with no lock held, after the turn was first saved; it takes
// the conversation's write lock once, briefly, to record what it did, and
// leaves the turn's own fields to RunAsk (which keeps the record it wrote).
//
// choice is the turn's route choice, stamped ONLY on the routing call's
// context: outside a run every model call on a stamped context takes the
// choice, and the turn's other calls -- a voice reply's speech, an embedding
// -- are not the routing call. emit records Activity on the turn.
func (e *MemQLEngine) routeAskTurn(ctx context.Context, conversationID, turnID, prompt string, choice common.RouteChoice, emit func(WorkEvent) error) (askRouteOutcome, error) {
	intake := askRouteOutcome{Intake: true, Statement: prompt}
	row, err := e.askRead(ContextWithFreshRead(ctx), conversationID)
	if err != nil {
		return askRouteOutcome{}, err
	}
	transcript, err := approvalTranscript(row)
	if err != nil {
		return askRouteOutcome{}, err
	}
	index := askTurnIndex(transcript.Turns, turnID)
	if index < 0 {
		return askRouteOutcome{}, fmt.Errorf("the accepted conversation turn is unavailable")
	}
	earlier := transcript.Turns[:index]
	goals, err := e.activeAskGoals(ctx, askTranscript{Turns: earlier})
	if err != nil {
		return skipAskRoute(ctx, prompt, routeSkippedUnavailable, routeUnavailableReason(err), emit)
	}
	if len(goals) == 0 {
		return intake, nil
	}
	pending, pendingHandles := askPendingStops(earlier, goals)
	clarification := askPendingClarification(earlier)
	data := askRouteData(prompt, earlier, goals, pendingHandles, clarification, time.Now())
	routing := airoute.WithObserver(ContextWithGoalRouteChoice(ctx, choice), func(call airoute.CallObservation) {
		// The Activity log names a call by its prompt: "Routing message".
		_ = emit(WorkEvent{ID: call.ID, Kind: "model", Phase: call.Phase, At: time.Now().UTC(), Provider: call.Provider, Model: call.Model, ElapsedMS: int64(call.ElapsedMS), Error: call.Error, Call: &call})
	})
	decision, outcome, err := e.decideAskRoute(routing, data, goals, pending, clarification != nil)
	switch {
	case err != nil:
		return skipAskRoute(ctx, prompt, routeSkippedUnavailable, routeUnavailableReason(err), emit)
	case outcome == askRouteIntake:
		return skipAskRoute(ctx, prompt, routeSkippedInvalid, "Routing gave no valid answer after one retry, so the message was taken as a new request.", emit)
	case len(decision.Updates)+len(decision.Stops)+len(decision.ConfirmStops) == 0 && decision.Clarify == "":
		// The model judged the message a new request: intake takes it as
		// written, and nothing was skipped.
		return intake, nil
	}
	apply, cancel := context.WithTimeout(context.WithoutCancel(ctx), askRouteApplyTimeout)
	defer cancel()
	return e.applyAskRoute(apply, conversationID, turnID, prompt, decision, clarification)
}

// skipAskRoute is routing that could not decide: the message goes to intake
// as written, and Activity says routing was skipped and why. A person who
// left decided nothing either; their turn ends interrupted, not skipped.
func skipAskRoute(ctx context.Context, prompt, skipped, reason string, emit func(WorkEvent) error) (askRouteOutcome, error) {
	if ctx.Err() != nil {
		return askRouteOutcome{}, ctx.Err()
	}
	if err := emit(WorkEvent{Kind: "action", Phase: "fallback", Name: "Routing message", Error: reason}); err != nil {
		return askRouteOutcome{}, err
	}
	return askRouteOutcome{Intake: true, Statement: prompt, Routed: &AskRouting{Skipped: skipped}}, nil
}

// routeUnavailableReason is the Activity entry for routing that failed to run.
func routeUnavailableReason(cause error) string {
	return "Routing was unavailable (" + askRouteClip(cause.Error(), askRouteUpdateRunes) + "), so the message was taken as a new request."
}

// applyAskRoute applies a valid decision in the design record's order:
// confirmed stops, then answers to the requests goals wait on, both as the
// person and outside the lock; then the one locked write; then, unlocked,
// the new titles. ctx carries no cancellation from the person's request.
func (e *MemQLEngine) applyAskRoute(ctx context.Context, conversationID, turnID, prompt string, decision askRouteDecision, clarification *AskTurn) (askRouteOutcome, error) {
	text := prompt
	if decision.ResolvesClarification && clarification != nil {
		text = askRouteResolvedText(clarification.Prompt, prompt)
	}
	var stopped, unstopped []askRouteGoal
	for _, goal := range decision.ConfirmStops {
		if err := e.stopAskGoal(ctx, goal.GoalID); err != nil {
			if logger := e.safeLogger(); logger != nil {
				logger.Warn("ask routing: a confirmed stop did not go through", "goal", goal.GoalID, "error", err)
			}
			unstopped = append(unstopped, goal)
			continue
		}
		stopped = append(stopped, goal)
	}
	updated := make([]askRouteGoal, 0, len(decision.Updates))
	for _, update := range decision.Updates {
		updated = append(updated, update.Goal)
	}
	answered, rest := e.deliverApprovalTargets(ctx, updated, text)
	hold, err := e.holdAskConversation(ctx, conversationID, "write")
	if err != nil {
		return askRouteOutcome{}, err
	}
	outcome, retitle, err := e.recordAskRoute(ctx, hold, askRouteRecord{
		conversationID: conversationID, turnID: turnID, prompt: prompt, text: text,
		decision: decision, clarification: clarification,
		answered: answered, rest: rest, stopped: stopped, unstopped: unstopped,
	})
	hold.release()
	if err != nil {
		return askRouteOutcome{}, err
	}
	for _, target := range retitle {
		e.retitleAskGoal(ctx, target.RunID, target.Title)
	}
	return outcome, nil
}

// askRouteRecord is what the locked write records: the decision, the message
// as each goal receives it, and what the phases before the lock did.
type askRouteRecord struct {
	conversationID, turnID, prompt, text string
	decision                             askRouteDecision
	clarification                        *AskTurn
	// answered are the targets the request phase delivered or was refused;
	// rest are the goals still to be judged under the lock.
	answered []AskRouteTarget
	rest     []askRouteGoal
	// stopped and unstopped are the confirmed stops, as they went.
	stopped, unstopped []askRouteGoal
}

// recordAskRoute is routing's one write, under hold: a fresh read of the
// conversation and of every goal still to be given work guidance, judged by
// its run as read there; then, on this turn, what reached each goal, the stop
// questions it asks and, for a message that opens no goal of its own, its
// answer; on earlier turns, the stops it confirmed and the question it
// settled. It returns the delivered targets whose goal takes a new title,
// which a work builtin sets once the lock is released.
func (e *MemQLEngine) recordAskRoute(ctx context.Context, hold *askConversationHold, record askRouteRecord) (askRouteOutcome, []AskRouteTarget, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	row, err := e.askRead(ContextWithFreshRead(ctx), record.conversationID)
	if err != nil {
		return askRouteOutcome{}, nil, err
	}
	transcript, err := approvalTranscript(row)
	if err != nil {
		return askRouteOutcome{}, nil, err
	}
	index := askTurnIndex(transcript.Turns, record.turnID)
	if index < 0 {
		return askRouteOutcome{}, nil, fmt.Errorf("the accepted conversation turn is unavailable")
	}
	runs := make(map[string]map[string]any, len(record.rest))
	for _, goal := range record.rest {
		rows, err := e.workRows(ContextWithFreshRead(ctx), "workRunForOwner", goal.RunID)
		if err != nil {
			return askRouteOutcome{}, nil, err
		}
		if len(rows) == 1 {
			runs[BareShortId(goal.RunID)] = rows[0]
		}
	}
	decision := record.decision
	reached := append(append([]AskRouteTarget(nil), record.answered...), guidanceTargetsLocked(record.conversationID, transcript, record.rest, runs, record.text)...)
	targets, retitle := askRouteTargets(decision.Updates, reached)
	routed := &AskRouting{Targets: targets}
	for _, goal := range decision.Stops {
		routed.Stops = append(routed.Stops, AskStopQuestion{GoalID: goal.GoalID, RunID: goal.RunID, Title: goal.Title})
	}
	reply := honestRouteReply(decision.Reply, targets, decision.Stops, record.stopped, record.unstopped)
	if decision.Clarify != "" {
		routed.Clarify, reply = true, decision.Clarify
	}
	// A stop the person confirmed answers every question still asking about
	// that goal.
	for _, goal := range record.stopped {
		answerAskStopQuestions(transcript.Turns[:index], goal.RunID, stopDecisionStopped)
	}
	if decision.ResolvesClarification && record.clarification != nil {
		if asked := askTurnIndex(transcript.Turns[:index], record.clarification.ID); asked >= 0 && transcript.Turns[asked].Routed != nil {
			transcript.Turns[asked].Routed.Resolved = true
		}
	}
	// Intake still takes the message when it was split, or when every goal it
	// was for had ended and nothing else was decided: a late "actually
	// Phoenix" still gets Phoenix.
	ended := len(targets) > 0
	for _, target := range targets {
		ended = ended && target.Status == routeNotReceived
	}
	outcome := askRouteOutcome{Routed: routed}
	turn := &transcript.Turns[index]
	if decision.NewRequest != "" || (ended && len(decision.Stops) == 0 && len(decision.ConfirmStops) == 0) {
		// Intake takes the split-off request, or else the message as its goals
		// would have: with routing's question settled, both of the person's
		// messages. The turn records what intake took when it is not the
		// message as written.
		statement := record.text
		if decision.NewRequest != "" {
			statement = decision.NewRequest
		}
		routed.Reply = reply
		if statement != record.prompt {
			routed.Statement = statement
		}
		outcome.Intake, outcome.Statement = true, statement
	} else {
		// The answer is saved with what routing did, so a resent turn is
		// answered from the record.
		now := time.Now().UTC()
		turn.Answer, turn.State, turn.EndedAt = reply, "done", &now
		outcome.Answer = reply
	}
	turn.Routed = routed
	if err := hold.verify(ctx); err != nil {
		return askRouteOutcome{}, nil, err
	}
	if err := e.askSave(ctx, record.conversationID, stringArg(row, "title"), transcript); err != nil {
		return askRouteOutcome{}, nil, err
	}
	return outcome, retitle, nil
}

// askRouteTargets are the targets of a decision's updates, in the order the
// decision named the goals. A delivered target of an update that renamed its
// goal carries the new title; retitle lists those whose goal does not have it
// yet.
func askRouteTargets(updates []askRouteUpdate, reached []AskRouteTarget) (targets, retitle []AskRouteTarget) {
	byRun := make(map[string]AskRouteTarget, len(reached))
	for _, target := range reached {
		byRun[BareShortId(target.RunID)] = target
	}
	for _, update := range updates {
		target, ok := byRun[BareShortId(update.Goal.RunID)]
		if !ok {
			continue
		}
		if update.Title != "" && routeDelivered(target.Status) {
			target.Title = update.Title
			if update.Title != runTitle(update.Goal.Run) {
				retitle = append(retitle, target)
			}
		}
		targets = append(targets, target)
	}
	return targets, retitle
}

// retitleAskGoal gives a goal the title the person's message gave it. It is a
// work builtin, called after the conversation's lock is released and as the
// person, never as a job. A failure is logged: the goal still got the message,
// and its label follows whatever title its run has.
func (e *MemQLEngine) retitleAskGoal(ctx context.Context, runID, title string) {
	call, err := parser.RenderCall("work.workRetitleRun", map[string]any{"runId": runID, "title": title})
	if err == nil {
		_, err = e.Execute(ctx, "builtin "+call)
	}
	if err != nil {
		if logger := e.safeLogger(); logger != nil {
			logger.Warn("ask routing: a goal kept its title", "run", runID, "error", err)
		}
	}
}

// honestRouteReply is the reply a routed message gets. The model's line is a
// proposal: it stands when every target was delivered and every stop the
// person confirmed went through, which the model's line cannot know. Otherwise
// Go says plainly what happened: one sentence per goal in the order the
// message went to them, then each confirmed stop as it went, then each stop
// question the message raised (asked), so the reply ends on the question the
// person answers. The words work spoken as well as shown: a live call has no
// cards, and its reply is the only place a stop is asked or reported.
func honestRouteReply(modelReply string, targets []AskRouteTarget, asked, stopped, unstopped []askRouteGoal) string {
	honest := len(unstopped) == 0
	sentences := make([]string, 0, len(targets)+len(asked)+len(stopped)+len(unstopped))
	for _, target := range targets {
		honest = honest && routeDelivered(target.Status)
		sentences = append(sentences, routeTargetSentence(target))
	}
	if honest {
		return modelReply
	}
	for _, goal := range stopped {
		sentences = append(sentences, "Stopped "+routeGoalName(goal.Title, false)+".")
	}
	for _, goal := range unstopped {
		sentences = append(sentences, routeGoalName(goal.Title, true)+" couldn't be stopped. Try again.")
	}
	for _, goal := range asked {
		sentences = append(sentences, "Stop "+routeGoalName(goal.Title, false)+"?")
	}
	return strings.Join(sentences, " ")
}

// routeTargetSentence is what the honest reply says of one goal.
func routeTargetSentence(target AskRouteTarget) string {
	switch {
	case routeDelivered(target.Status):
		return "Sent to " + routeGoalName(target.Title, false) + "."
	case target.Status == routeNotReceived:
		return routeGoalName(target.Title, true) + " already finished, so it didn't get this."
	case target.Reason == routeReasonReview:
		return routeGoalName(target.Title, true) + " is waiting on a review that doesn't take messages. Open it to decide."
	case target.Reason == routeReasonLimit:
		return routeGoalName(target.Title, true) + " can't take more messages."
	}
	return routeGoalName(target.Title, true) + " couldn't take this message."
}

// routeGoalName is a goal as a reply names it: its title, or "this work" for
// one with none, capitalized to open a sentence.
func routeGoalName(title string, opening bool) string {
	if title = strings.TrimSpace(title); title != "" {
		return title
	}
	if opening {
		return "This work"
	}
	return "this work"
}

// askRouteResolvedText is what a message that settles routing's own question
// delivers: the earlier message and this one, both word for word; the earlier
// one alone when together they would pass the guidance limit.
func askRouteResolvedText(earlier, message string) string {
	if both := earlier + "\n\n" + message; utf8.RuneCountInString(both) <= askRouteMessageRunes {
		return both
	}
	return earlier
}

// askPendingStops is which of goals have a stop question still waiting for
// the person: any earlier turn asked about the goal's run and nobody answered
// it. The handles are what the prompt lists, never null.
func askPendingStops(turns []AskTurn, goals []askRouteGoal) (map[string]bool, []string) {
	asked := map[string]bool{}
	for _, turn := range turns {
		if turn.Routed == nil {
			continue
		}
		for _, stop := range turn.Routed.Stops {
			if stop.Decision == "" {
				asked[BareShortId(stop.RunID)] = true
			}
		}
	}
	pending, handles := map[string]bool{}, []string{}
	for _, goal := range goals {
		if asked[BareShortId(goal.RunID)] {
			pending[goal.Handle] = true
			handles = append(handles, goal.Handle)
		}
	}
	return pending, handles
}

// askPendingClarification is routing's own question when it is still waiting:
// the person's previous message was answered with it and nothing settled it.
// A projected answer turn is not something the person said.
func askPendingClarification(turns []AskTurn) *AskTurn {
	for n := len(turns) - 1; n >= 0; n-- {
		if turns[n].AnswerOnly {
			continue
		}
		if routed := turns[n].Routed; routed != nil && routed.Clarify && !routed.Resolved {
			return &turns[n]
		}
		return nil
	}
	return nil
}

// askTurnRequest is what a turn asked of the goal it opened: a split
// message's own request, which is what intake received, else the message.
func askTurnRequest(turn AskTurn) string {
	if turn.Routed != nil && strings.TrimSpace(turn.Routed.Statement) != "" {
		return turn.Routed.Statement
	}
	return turn.Prompt
}

// askTurnIndex is where the turn with turnID is in turns, or -1.
func askTurnIndex(turns []AskTurn, turnID string) int {
	for n := range turns {
		if turns[n].ID == turnID {
			return n
		}
	}
	return -1
}
