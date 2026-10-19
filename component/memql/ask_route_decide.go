package memql

// ask_route_decide.go -- where a person's message goes when goals are already
// running in their Ask conversation (design record
// docs/superpowers/specs/2026-10-19-ask-message-routing-design.md, "The
// recipe", "The routing prompt" and "Validation (Go)").
//
// One fast structured model call decides which goals the message updates
// (with an optional new title), which goals to offer stopping, which pending
// stop questions it confirms, any new request split off it, a one-line reply,
// or a clarifying question.
//
// THE RECIPE CHOOSES, GO VALIDATES. The DSL template askMessageRouting
// (dsl/os/automations.memql) picks the prompt and the one retry; this file
// binds the two operations it calls to one message and checks every answer
// against the goals the call was shown before the answer counts. A handle the
// call did not show, a goal both updated and stopped, a confirmation nobody
// was asked for: each is refused in one sentence, and that sentence is what
// the retry is told.
//
// THE MODEL SEES HANDLES, NEVER IDS. g1..g12 name the goals in the prompt's
// data; goal, run, turn and approval ids stay in askRouteGoal, in Go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"time"
	"unicode/utf8"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/num"
)

// The routing recipe, its structured answer, and what the recipe returns.
const (
	askRouteWorkflow   = "askMessageRouting"
	askRouteSchemaName = "ask_message_route"
	askRouteRouted     = "routed"
	askRouteIntake     = "intake"
)

// What a routing call is shown, and what it may answer.
const (
	askRouteTurns        = 8    // the conversation's most recent turns
	askRouteSideRunes    = 600  // one side of a turn; a goal's request
	askRouteUpdates      = 3    // messages a goal was already sent
	askRouteUpdateRunes  = 300  // one of those messages; what a goal waits on
	askRouteTitleRunes   = 80   // a goal's title, shown or proposed
	askRouteReplyRunes   = 280  // the reply line
	askRouteMessageRunes = 4000 // a message routing takes, a split-off request, and one message a goal receives (the guidance limit)
	askRouteDecisions    = 2    // one decision and one retry
)

// How long one routing decision may take, in seconds. The recipe chooses
// (askRouteDecide's timeoutSeconds); Go holds the choice to 1 to the longest,
// and takes the default when the recipe chooses nothing.
const (
	askRouteDecideSeconds = 30
	askRouteDecideLongest = 60
)

// askRouteDecision is a validated routing answer, its handles resolved to the
// goals they name. A decision with nothing set is intake: the message is a new
// request, taken in the person's own words.
type askRouteDecision struct {
	Updates                    []askRouteUpdate
	Stops, ConfirmStops        []askRouteGoal
	NewRequest, Reply, Clarify string
	ResolvesClarification      bool
}

// askRouteUpdate is one goal the message is delivered to, and the title the
// goal takes because the message changed what it is about ("" keeps it).
type askRouteUpdate struct {
	Goal  askRouteGoal
	Title string
}

// askRouteSchema is the answer's strict shape: at every level every key is
// required and no other is admitted, as a strict structured call needs.
var askRouteSchema = json.RawMessage(`{"type":"object","additionalProperties":false,` +
	`"required":["updates","stops","confirmStops","newRequest","reply","clarify","resolvesClarification"],"properties":{` +
	`"updates":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["goal","title"],` +
	`"properties":{"goal":{"type":"string"},"title":{"type":"string"}}}},` +
	`"stops":{"type":"array","items":{"type":"string"}},` +
	`"confirmStops":{"type":"array","items":{"type":"string"}},` +
	`"newRequest":{"type":"string"},"reply":{"type":"string"},"clarify":{"type":"string"},` +
	`"resolvesClarification":{"type":"boolean"}}}`)

// askRouteAnswer is a routing answer as the model writes it.
type askRouteAnswer struct {
	Updates []struct {
		Goal  string `json:"goal"`
		Title string `json:"title"`
	} `json:"updates"`
	Stops                 []string `json:"stops"`
	ConfirmStops          []string `json:"confirmStops"`
	NewRequest            string   `json:"newRequest"`
	Reply                 string   `json:"reply"`
	Clarify               string   `json:"clarify"`
	ResolvesClarification bool     `json:"resolvesClarification"`
}

// validateAskRoute reads one routing answer against the goals its call was
// shown. pendingStops holds the handles whose stop question is unanswered;
// clarification reports that the previous turn is routing's own unanswered
// question.
//
// A refusal is one short sentence naming what to fix, because it is what the
// retry is told. An answer that acts on nothing -- no update, stop,
// confirmation or question, whatever it says in newRequest or reply -- is the
// zero decision: intake with the person's own words.
func validateAskRoute(raw string, goals []askRouteGoal, pendingStops map[string]bool, clarification bool) (askRouteDecision, error) {
	answer, err := decodeAskRouteAnswer(raw)
	if err != nil {
		return askRouteDecision{}, err
	}
	byHandle := make(map[string]askRouteGoal, len(goals))
	for _, goal := range goals {
		byHandle[goal.Handle] = goal
	}

	var decision askRouteDecision
	updated, stopped, confirmed := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, update := range answer.Updates {
		handle := strings.TrimSpace(update.Goal)
		goal, err := askRouteHandle(byHandle, handle, "updates", updated)
		if err != nil {
			return askRouteDecision{}, err
		}
		title := strings.TrimSpace(update.Title)
		if n := utf8.RuneCountInString(title); n > askRouteTitleRunes {
			return askRouteDecision{}, fmt.Errorf("the title for %s is %d characters; a title is at most %d", handle, n, askRouteTitleRunes)
		}
		decision.Updates = append(decision.Updates, askRouteUpdate{Goal: goal, Title: title})
	}
	for _, handle := range answer.Stops {
		handle = strings.TrimSpace(handle)
		goal, err := askRouteHandle(byHandle, handle, "stops", stopped)
		if err != nil {
			return askRouteDecision{}, err
		}
		if updated[handle] {
			return askRouteDecision{}, fmt.Errorf("goal %s cannot be both updated and stopped", handle)
		}
		decision.Stops = append(decision.Stops, goal)
	}
	for _, handle := range answer.ConfirmStops {
		handle = strings.TrimSpace(handle)
		goal, err := askRouteHandle(byHandle, handle, "confirmStops", confirmed)
		if err != nil {
			return askRouteDecision{}, err
		}
		switch {
		case !pendingStops[handle]:
			return askRouteDecision{}, fmt.Errorf("goal %s has no stop question waiting for an answer; confirmStops names only goals listed under STOP QUESTIONS", handle)
		case updated[handle]:
			return askRouteDecision{}, fmt.Errorf("goal %s cannot be both updated and stopped", handle)
		case stopped[handle]:
			return askRouteDecision{}, fmt.Errorf("goal %s is in both stops and confirmStops; ask about stopping it or confirm its stop, not both", handle)
		}
		decision.ConfirmStops = append(decision.ConfirmStops, goal)
	}

	reply := strings.TrimSpace(answer.Reply)
	clarify := strings.TrimSpace(answer.Clarify)
	newRequest := strings.TrimSpace(answer.NewRequest)
	acted := len(decision.Updates)+len(decision.Stops)+len(decision.ConfirmStops) > 0
	if clarify != "" && (acted || newRequest != "" || answer.ResolvesClarification) {
		return askRouteDecision{}, errors.New("clarify excludes every other action: with a question, leave updates, stops, confirmStops and newRequest empty and resolvesClarification false")
	}
	if answer.ResolvesClarification {
		if !clarification {
			return askRouteDecision{}, errors.New("resolvesClarification is true, but no question of yours is waiting for an answer")
		}
		if len(decision.Updates) == 0 {
			return askRouteDecision{}, errors.New("resolvesClarification is true without updates: name the goals your question was about")
		}
	}
	// Nothing acted on or asked is intake. A new request counts only beside
	// another action, so a message that is only a new request keeps the
	// person's own words; past here, a new request is a split.
	if !acted && clarify == "" {
		return askRouteDecision{}, nil
	}
	if reply == "" {
		return askRouteDecision{}, errors.New("reply is empty: when anything is acted on or asked, reply says what in one sentence")
	}
	if n := utf8.RuneCountInString(reply); n > askRouteReplyRunes {
		return askRouteDecision{}, fmt.Errorf("the reply is %d characters; a reply is at most %d", n, askRouteReplyRunes)
	}
	if n := utf8.RuneCountInString(newRequest); n > askRouteMessageRunes {
		return askRouteDecision{}, fmt.Errorf("newRequest is %d characters; it is at most %d", n, askRouteMessageRunes)
	}
	decision.NewRequest = newRequest
	decision.Reply, decision.Clarify, decision.ResolvesClarification = reply, clarify, answer.ResolvesClarification
	return decision, nil
}

// decodeAskRouteAnswer reads the one JSON object an answer must be. A key
// left out reads as empty; a key the format does not have is refused, because
// a misspelled key would otherwise drop an action without a word.
func decodeAskRouteAnswer(raw string) (askRouteAnswer, error) {
	notTheFormat := errors.New("the answer is not the JSON object the format asks for: answer with that object alone")
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "{") {
		return askRouteAnswer{}, notTheFormat
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var answer askRouteAnswer
	if err := decoder.Decode(&answer); err != nil {
		var mistyped *json.UnmarshalTypeError
		if key, unknown := strings.CutPrefix(err.Error(), "json: unknown field "); unknown {
			return askRouteAnswer{}, fmt.Errorf("the answer has a key the format does not have: %s", key)
		}
		if errors.As(err, &mistyped) && mistyped.Field != "" {
			return askRouteAnswer{}, fmt.Errorf("%s does not have the type the format asks for", mistyped.Field)
		}
		return askRouteAnswer{}, notTheFormat
	}
	if _, err := decoder.Token(); err != io.EOF {
		return askRouteAnswer{}, notTheFormat
	}
	return answer, nil
}

// askRouteHandle resolves one handle an answer's list names, refusing a
// handle its call did not show and one the list already named.
func askRouteHandle(goals map[string]askRouteGoal, handle, list string, seen map[string]bool) (askRouteGoal, error) {
	if handle == "" {
		return askRouteGoal{}, fmt.Errorf("an entry in %s names no goal", list)
	}
	goal, ok := goals[handle]
	if !ok {
		return askRouteGoal{}, fmt.Errorf("goal %s is not one of the active goals", handle)
	}
	if seen[handle] {
		return askRouteGoal{}, fmt.Errorf("goal %s is listed twice in %s", handle, list)
	}
	seen[handle] = true
	return goal, nil
}

// askRouteData is what the routing prompt is shown for one message: the
// conversation's recent turns, its active goals by handle, the goals whose
// stop question is unanswered (pendingStops, as handles), routing's own
// question when the previous turn asked one (clarification), and the clock.
// turns are the conversation's turns before this message, oldest first.
//
// Everything is bounded, nothing carries an id, and the keys are exactly the
// prompt's: its input schema is checked before it renders and admits no
// other key, nor null where it takes a list. previousError is added by the
// retry.
func askRouteData(message string, turns []AskTurn, goals []askRouteGoal, pendingStops []string, clarification *AskTurn, now time.Time) map[string]any {
	shown := make([]any, 0, len(goals))
	for _, goal := range goals {
		shown = append(shown, askRouteGoalData(goal))
	}
	stops := make([]any, 0, len(pendingStops))
	for _, handle := range pendingStops {
		stops = append(stops, handle)
	}
	data := map[string]any{
		"message":      message,
		"conversation": askRouteConversation(turns),
		"goals":        shown,
		"pendingStops": stops,
		"now":          now.UTC().Format(time.RFC3339),
	}
	if clarification != nil {
		data["pendingClarification"] = map[string]any{
			"message":  askRouteClip(clarification.Prompt, askRouteMessageRunes),
			"question": askRouteClip(askRouteSaid(*clarification), askRouteSideRunes),
		}
	}
	return data
}

// askRouteGoalData is one active goal as the prompt shows it: its handle,
// title, original request, state, what it waits on, and the last messages it
// was already sent.
func askRouteGoalData(goal askRouteGoal) map[string]any {
	state, waitingOn := askRouteGoalState(goal.Run, goal.Approval)
	shown := map[string]any{
		"handle":  goal.Handle,
		"title":   askRouteClip(goal.Title, askRouteTitleRunes),
		"request": askRouteClip(goal.Request, askRouteSideRunes),
		"state":   state,
	}
	if waitingOn != "" {
		shown["waitingOn"] = waitingOn
	}
	updates := goal.Updates
	if len(updates) > askRouteUpdates {
		updates = updates[len(updates)-askRouteUpdates:]
	}
	if len(updates) > 0 {
		sent := make([]any, 0, len(updates))
		for _, text := range updates {
			sent = append(sent, askRouteClip(text, askRouteUpdateRunes))
		}
		shown["updates"] = sent
	}
	return shown
}

// askRouteGoalState is what a goal is doing, in the routing prompt's words,
// and what it waits on when it waits on the person. The words are the
// delivery the goal gets (routeGoalDelivery), so the model is never told a
// goal will read a message that delivery records Not delivered: a goal shown
// waiting for an answer, a permission or its budget takes the message through
// its request; one shown waiting on a review takes none; every other goal
// reads it as work guidance, now or when it resumes.
func askRouteGoalState(run, approval map[string]any) (state, waitingOn string) {
	kind, status, _ := routeGoalDelivery(run, approval)
	asked := stringArg(approval, "question")
	switch {
	case kind == deliverApproval && status == routeSaved:
		return "paused on its budget", ""
	case kind == deliverApproval && status == routeAnswered:
		return "waiting for your answer", askRouteClip(asked, askRouteUpdateRunes)
	case kind == deliverApproval:
		if strings.TrimSpace(asked) == "" {
			evidence, _ := approval["evidence"].(map[string]any)
			asked = stringArg(evidence, "reason")
		}
		return "waiting for your permission", askRouteClip(asked, askRouteUpdateRunes)
	case kind == deliverNone && status == routeNotDelivered:
		return "waiting on a review", askRouteClip(asked, askRouteUpdateRunes)
	case kind == deliverNone:
		// Listing shows only active goals: a run that is not one has ended.
		return "finished", ""
	}
	switch stringArg(run, "status") {
	case "compiling":
		return "preparing", ""
	case "waiting":
		return "waiting to resume", ""
	}
	return "working", ""
}

// askRouteConversation is the conversation's last turns as the prompt shows
// them, oldest first, each side cut to its bound.
func askRouteConversation(turns []AskTurn) string {
	var kept []AskTurn
	for _, turn := range turns {
		// As askConversationMessages: a failed transcription is not something
		// the person said.
		if strings.TrimSpace(turn.Prompt) == "" || (turn.Prompt == "Voice message" && turn.State == "error") {
			continue
		}
		kept = append(kept, turn)
	}
	if len(kept) > askRouteTurns {
		kept = kept[len(kept)-askRouteTurns:]
	}
	var b strings.Builder
	for _, turn := range kept {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("Person: " + askRouteClip(turn.Prompt, askRouteSideRunes))
		if said := askRouteSaid(turn); said != "" {
			b.WriteString("\nMemQL: " + askRouteClip(said, askRouteSideRunes))
		}
	}
	return b.String()
}

// askRouteSaid is what MemQL said on a turn: a split message's routing reply
// first, then the answer or, while the work runs, its acknowledgement.
func askRouteSaid(turn AskTurn) string {
	said := strings.TrimSpace(turn.Answer)
	if said == "" {
		said = strings.TrimSpace(turn.Acknowledgement)
	}
	if turn.Routed != nil {
		if reply := strings.TrimSpace(turn.Routed.Reply); reply != "" && reply != said {
			said = strings.TrimSpace(reply + "\n" + said)
		}
	}
	return said
}

// askRouteClip is text without surrounding space, kept within limit
// characters; a cut ends in an ellipsis inside the limit. It is the one clip
// for everything routing shows and lists.
func askRouteClip(text string, limit int) string {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit-1]) + "…"
}

// decideAskRoute runs the routing recipe for one message. data is what the
// prompt is shown (askRouteData); goals, pendingStops and clarification are
// what each answer is validated against.
//
// outcome is the recipe's return. "routed" comes with the valid decision --
// which, when it acts on nothing, is the model's own judgement that the
// message is a new request. "intake" means no valid decision was made after
// the one retry, and the message goes to intake as written. An error is a
// routing failure -- no model, a decision that ran past the deadline the
// recipe gave it, a recipe that would not run -- and decides nothing.
//
// The model is called for the person on ctx, under whatever route ctx
// carries (ApplyRunRouting). Nothing here writes: the caller applies the
// decision.
func (e *MemQLEngine) decideAskRoute(ctx context.Context, data map[string]any, goals []askRouteGoal, pendingStops map[string]bool, clarification bool) (decision askRouteDecision, outcome string, err error) {
	var decided *askRouteDecision
	asked := 0
	result, err := e.runScopedWorkflow(ctx, askRouteWorkflow, map[string]any{}, map[string]WorkflowOperation{
		"askRouteGoals": func(context.Context, map[string]any) (any, error) {
			return map[string]any{"count": len(goals)}, nil
		},
		"askRouteDecide": func(ctx context.Context, args map[string]any) (any, error) {
			asked++
			if asked > askRouteDecisions {
				return nil, fmt.Errorf("routing makes one decision and one retry, and this would be call %d", asked)
			}
			// A copy: the retry's refusal is this call's, never the caller's.
			input := make(map[string]any, len(data)+1)
			maps.Copy(input, data)
			if refused := stringArg(args, "previousError"); refused != "" {
				input["previousError"] = refused
			}
			// The recipe chooses how long this decision may take and Go holds
			// it to its bounds: the person's message waits on routing, so a
			// routing door that does not answer in time is a routing failure,
			// never a wait. Running out of time is said as such; the person
			// leaving is left as the cancellation it is.
			timeout := askRouteDecideTimeout(args)
			attempt, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			raw, err := e.InvokeAIStructured(attempt, stringArg(args, "prompt"), input, askRouteSchemaName, askRouteSchema, true)
			if err != nil {
				if ctx.Err() == nil && errors.Is(attempt.Err(), context.DeadlineExceeded) {
					return nil, fmt.Errorf("no routing decision within %s: %w", timeout, err)
				}
				return nil, err
			}
			valid, err := validateAskRoute(raw, goals, pendingStops, clarification)
			if err != nil {
				return map[string]any{"valid": false, "error": err.Error()}, nil
			}
			decided = &valid
			return map[string]any{"valid": true, "error": ""}, nil
		},
	})
	if err != nil {
		return askRouteDecision{}, "", err
	}
	switch outcome, _ = result.(string); {
	case outcome == askRouteIntake:
		return askRouteDecision{}, outcome, nil
	case outcome == askRouteRouted && decided != nil:
		return *decided, outcome, nil
	}
	return askRouteDecision{}, "", fmt.Errorf("the routing recipe returned %v without a valid decision", result)
}

// askRouteDecideTimeout is how long one routing decision may take: what the
// recipe gives it (timeoutSeconds), held to 1 to askRouteDecideLongest
// seconds; askRouteDecideSeconds when the recipe says nothing a number can.
func askRouteDecideTimeout(args map[string]any) time.Duration {
	seconds := askRouteDecideSeconds
	switch given := args["timeoutSeconds"].(type) {
	case int, int32, int64, float64:
		seconds = intFromAny(given)
	case json.Number:
		if value, err := given.Float64(); err == nil {
			seconds = num.ClampFloat64(value)
		}
	}
	return time.Duration(min(max(seconds, 1), askRouteDecideLongest)) * time.Second
}

// scopedAskRouteOperation answers a direct call of askRouteGoals or
// askRouteDecide. Both exist only inside decideAskRoute's workflow scope,
// which binds them to one person's message and its goals; called from
// anywhere else there is nothing to route, and no caller may spend a model
// call with a prompt and data of its own choosing.
func scopedAskRouteOperation(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
	return nil, errors.New("ask routing operation requires its authorized workflow scope")
}
