package memql

// The routing decision (design record
// docs/superpowers/specs/2026-10-19-ask-message-routing-design.md, "The routing
// prompt" and "Validation (Go)"): what the model is shown, what Go accepts from
// it, and the recipe's one retry.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// routeGoals are three active goals as routing sees them. Their ids share no
// text with anything a model is shown, so a leaked id is visible.
func routeGoals() []askRouteGoal {
	return []askRouteGoal{
		{Handle: "g1", TurnID: "turn-0c42", GoalID: "v1:work:goal:5d1e", RunID: "v1:work:run:81aa", Title: "Weather in Tucson", Request: "What's the weather in Tucson?", Run: map[string]any{"id": "v1:work:run:81aa", "goalId": "v1:work:goal:5d1e", "status": "running"}},
		{Handle: "g2", TurnID: "turn-7f3b", GoalID: "v1:work:goal:2c9d", RunID: "v1:work:run:4e60", Title: "Hike near Tucson", Request: "Plan a hike near Tucson", Run: map[string]any{"id": "v1:work:run:4e60", "goalId": "v1:work:goal:2c9d", "status": "compiling"}},
		{Handle: "g3", TurnID: "turn-a51c", GoalID: "v1:work:goal:93b7", RunID: "v1:work:run:d204", Title: "Quarterly report", Request: "Write the quarterly report",
			Run:      map[string]any{"id": "v1:work:run:d204", "goalId": "v1:work:goal:93b7", "status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": "v1:work:approval:6e1f", "approvalKind": "feedback"}},
			Approval: map[string]any{"id": "v1:work:approval:6e1f", "kind": "feedback", "question": "Which quarter?", "subject": map[string]any{"question": "Which quarter?"}}},
	}
}

// routeAnswer is a complete answer in the routing format: every key the
// schema requires, empty unless set.
func routeAnswer(set map[string]any) string {
	full := map[string]any{"updates": []any{}, "stops": []any{}, "confirmStops": []any{}, "newRequest": "", "reply": "", "clarify": "", "resolvesClarification": false}
	for k, v := range set {
		full[k] = v
	}
	raw, _ := json.Marshal(full)
	return string(raw)
}

func routeUpdate(goal, title string) map[string]any {
	return map[string]any{"goal": goal, "title": title}
}

func TestValidateAskRoute(t *testing.T) {
	goals := routeGoals()
	pending := map[string]bool{"g3": true}

	refused := []struct {
		name          string
		raw           string
		clarification bool
		names         string // what the refusal must name, so the retry can fix exactly that
	}{
		{"an unknown handle", routeAnswer(map[string]any{"updates": []any{routeUpdate("g7", "")}, "reply": "Sent."}), false, "g7"},
		{"an unknown stop", routeAnswer(map[string]any{"stops": []any{"g9"}, "reply": "Stop it?"}), false, "g9"},
		{"a duplicate update", routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", ""), routeUpdate("g1", "")}, "reply": "Sent."}), false, "g1"},
		{"a duplicate stop", routeAnswer(map[string]any{"stops": []any{"g2", "g2"}, "reply": "Stop it?"}), false, "g2"},
		{"updated and stopped", routeAnswer(map[string]any{"updates": []any{routeUpdate("g2", "")}, "stops": []any{"g2"}, "reply": "Sent."}), false, "g2"},
		{"updated and confirmed stopped", routeAnswer(map[string]any{"updates": []any{routeUpdate("g3", "")}, "confirmStops": []any{"g3"}, "reply": "Sent."}), false, "g3"},
		{"asked about and confirmed", routeAnswer(map[string]any{"stops": []any{"g3"}, "confirmStops": []any{"g3"}, "reply": "Stopping."}), false, "g3"},
		{"a confirmation nobody was asked", routeAnswer(map[string]any{"confirmStops": []any{"g1"}, "reply": "Stopping it."}), false, "g1"},
		{"a question with an update", routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "")}, "clarify": "The weather or the hike?", "reply": "The weather or the hike?"}), false, "clarify"},
		{"a question with a new request", routeAnswer(map[string]any{"clarify": "The weather or the hike?", "newRequest": "What time is it?", "reply": "The weather or the hike?"}), false, "clarify"},
		{"a resolution with no question waiting", routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "")}, "resolvesClarification": true, "reply": "Sent."}), false, "resolvesClarification"},
		{"a resolution with no update", routeAnswer(map[string]any{"resolvesClarification": true, "reply": "Sent."}), true, "resolvesClarification"},
		{"an action with no reply", routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "")}}), false, "reply"},
		{"a question with no reply", routeAnswer(map[string]any{"clarify": "The weather or the hike?"}), false, "reply"},
		{"a reply over 280 characters", routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "")}, "reply": strings.Repeat("é", 281)}), false, "280"},
		{"a title over 80 characters", routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", strings.Repeat("é", 81))}, "reply": "Sent."}), false, "80"},
		{"a split request over 4000 characters", routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "")}, "newRequest": strings.Repeat("é", 4001), "reply": "Sent."}), false, "4000"},
		{"prose", "Sure! The message is about the weather.", false, "JSON"},
		{"a key the format does not have", `{"update":[{"goal":"g1","title":""}],"reply":"Sent."}`, false, "update"},
	}
	for _, tc := range refused {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			decision, err := validateAskRoute(tc.raw, goals, pending, tc.clarification)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.names)
			require.Equal(t, askRouteDecision{}, decision)
		})
	}

	accepted := []struct {
		name          string
		raw           string
		clarification bool
		want          askRouteDecision
	}{
		{"the Tucson-to-Phoenix answer", routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", " Weather in Phoenix ")}, "reply": "Switching the weather check to Phoenix."}), false,
			askRouteDecision{Updates: []askRouteUpdate{{Goal: goals[0], Title: "Weather in Phoenix"}}, Reply: "Switching the weather check to Phoenix."}},
		{"one message to two related goals", routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "Weather in Phoenix"), routeUpdate("g2", "")}, "reply": "Switching both to Phoenix."}), false,
			askRouteDecision{Updates: []askRouteUpdate{{Goal: goals[0], Title: "Weather in Phoenix"}, {Goal: goals[1]}}, Reply: "Switching both to Phoenix."}},
		{"a split keeps its new request", routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "")}, "newRequest": "What time is it in Phoenix right now?", "reply": "Switching the weather check to Phoenix."}), false,
			askRouteDecision{Updates: []askRouteUpdate{{Goal: goals[0]}}, NewRequest: "What time is it in Phoenix right now?", Reply: "Switching the weather check to Phoenix."}},
		{"a stop question", routeAnswer(map[string]any{"stops": []any{"g2"}, "reply": "Stop planning the hike?"}), false,
			askRouteDecision{Stops: []askRouteGoal{goals[1]}, Reply: "Stop planning the hike?"}},
		{"a confirmed stop", routeAnswer(map[string]any{"confirmStops": []any{"g3"}, "reply": "Stopping the report."}), false,
			askRouteDecision{ConfirmStops: []askRouteGoal{goals[2]}, Reply: "Stopping the report."}},
		{"a clarifying question", routeAnswer(map[string]any{"clarify": "The weather or the hike?", "reply": "The weather or the hike?"}), false,
			askRouteDecision{Clarify: "The weather or the hike?", Reply: "The weather or the hike?"}},
		{"an answer to the question", routeAnswer(map[string]any{"updates": []any{routeUpdate("g2", "")}, "resolvesClarification": true, "reply": "Sent to the hike plan."}), true,
			askRouteDecision{Updates: []askRouteUpdate{{Goal: goals[1]}}, ResolvesClarification: true, Reply: "Sent to the hike plan."}},
		{"empty keys left out", `{"updates":[{"goal":"g1","title":""}],"reply":"Sent."}`, false,
			askRouteDecision{Updates: []askRouteUpdate{{Goal: goals[0]}}, Reply: "Sent."}},
		{"a title, reply and split request at their limits", routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", strings.Repeat("é", 80))}, "newRequest": strings.Repeat("é", 4000), "reply": strings.Repeat("é", 280)}), false,
			askRouteDecision{Updates: []askRouteUpdate{{Goal: goals[0], Title: strings.Repeat("é", 80)}}, NewRequest: strings.Repeat("é", 4000), Reply: strings.Repeat("é", 280)}},
		// No action at all is intake with the person's own words: a new request
		// alone, a reply alone, nothing at all.
		{"a new request alone is intake", routeAnswer(map[string]any{"newRequest": "What time is it in Phoenix?", "reply": "Looking that up."}), false, askRouteDecision{}},
		{"a reply alone is intake", routeAnswer(map[string]any{"reply": "You're welcome!"}), false, askRouteDecision{}},
		{"nothing at all is intake", routeAnswer(nil), false, askRouteDecision{}},
	}
	for _, tc := range accepted {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			decision, err := validateAskRoute(tc.raw, goals, pending, tc.clarification)
			require.NoError(t, err)
			require.Equal(t, tc.want, decision)
		})
	}
}

// A strict structured call is refused by the provider unless every object
// lists every one of its properties as required and admits no other.
func TestAskRouteSchemaIsStrict(t *testing.T) {
	var schema map[string]any
	require.NoError(t, json.Unmarshal(askRouteSchema, &schema))
	var walk func(path string, node map[string]any)
	walk = func(path string, node map[string]any) {
		if items, ok := node["items"].(map[string]any); ok {
			walk(path+"[]", items)
		}
		if node["type"] != "object" {
			return
		}
		require.Equal(t, false, node["additionalProperties"], "%s admits keys it does not name", path)
		properties, _ := node["properties"].(map[string]any)
		var names, required []string
		for name, property := range properties {
			names = append(names, name)
			child, _ := property.(map[string]any)
			walk(path+"."+name, child)
		}
		listed, _ := node["required"].([]any)
		for _, name := range listed {
			required = append(required, fmt.Sprint(name))
		}
		sort.Strings(names)
		sort.Strings(required)
		require.Equal(t, names, required, "%s must require every property it has", path)
	}
	walk("answer", schema)
	properties, _ := schema["properties"].(map[string]any)
	require.Len(t, properties, 7)
	for _, key := range []string{"updates", "stops", "confirmStops", "newRequest", "reply", "clarify", "resolvesClarification"} {
		require.Contains(t, properties, key)
	}

	compiled, err := jsonschema.CompileString("ask_message_route.json", string(askRouteSchema))
	require.NoError(t, err)
	var phoenix any
	require.NoError(t, json.Unmarshal([]byte(routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "Weather in Phoenix")}, "reply": "Switching the weather check to Phoenix."})), &phoenix))
	require.NoError(t, compiled.Validate(phoenix), "the schema refuses the answer validation accepts")
}

func TestAskRouteDataIsBounded(t *testing.T) {
	var turns []AskTurn
	for n := 1; n <= 20; n++ {
		turns = append(turns, AskTurn{ID: fmt.Sprintf("turn-%02d", n), Prompt: fmt.Sprintf("question %02d", n), Answer: fmt.Sprintf("answer %02d", n), State: "done"})
	}
	long := strings.Repeat("é", 5000)
	turns[18].Prompt = "question 19 " + long
	turns[19].Answer = long
	// A failed transcription is not something the person said, and takes no
	// place among the last 8.
	turns = append(turns, AskTurn{ID: "voice", Prompt: "Voice message", State: "error"})
	goals := routeGoals()
	goals[0].Updates = []string{"first", "second", "third", "fourth " + long, "fifth"}
	goals[1].Request = "Plan a hike " + long

	data := askRouteData("Actually, I'm in Phoenix", turns, goals, nil, nil, time.Date(2026, 10, 19, 17, 4, 5, 0, time.UTC))

	conversation, _ := data["conversation"].(string)
	require.Equal(t, 8, strings.Count(conversation, "Person: "), "the last 8 turns, and no more")
	for n := 1; n <= 12; n++ {
		require.NotContains(t, conversation, fmt.Sprintf("question %02d", n))
	}
	for n := 13; n <= 20; n++ {
		require.Contains(t, conversation, fmt.Sprintf("question %02d", n))
	}
	require.NotContains(t, conversation, "Voice message")
	for _, line := range strings.Split(conversation, "\n") {
		side, _ := strings.CutPrefix(line, "Person: ")
		side, _ = strings.CutPrefix(side, "MemQL: ")
		require.LessOrEqual(t, utf8.RuneCountInString(side), 600, "a side of the conversation is longer than 600 characters")
		if strings.Contains(line, "éééé") {
			require.Equal(t, 600, utf8.RuneCountInString(side), "a long side is cut to 600 characters")
		}
	}

	shown := data["goals"].([]any)
	require.Len(t, shown, 3)
	first := shown[0].(map[string]any)
	require.Equal(t, "g1", first["handle"])
	updates := first["updates"].([]any)
	require.Len(t, updates, 3, "the last three messages a goal was sent, no more")
	require.Equal(t, "third", updates[0])
	require.True(t, strings.HasPrefix(updates[1].(string), "fourth éé"))
	require.Equal(t, 300, utf8.RuneCountInString(updates[1].(string)), "a message already sent is cut to 300 characters")
	require.Equal(t, "fifth", updates[2])
	require.Equal(t, 600, utf8.RuneCountInString(shown[1].(map[string]any)["request"].(string)), "a request is cut to 600 characters")

	raw, err := json.Marshal(data)
	require.NoError(t, err)
	for _, id := range []string{"v1:work", "5d1e", "81aa", "turn-0c42", "2c9d", "4e60", "93b7", "d204", "6e1f"} {
		require.NotContains(t, string(raw), id, "the model is shown handles, never ids")
	}
	require.Equal(t, "2026-10-19T17:04:05Z", data["now"])
}

// askRouteStateCase is one goal as routing lists it -- its run and the request
// it is parked on, if any -- and the words the prompt says it in.
type askRouteStateCase struct {
	name             string
	run, approval    map[string]any
	state, waitingOn string
}

// askRouteStateCases are the goal states routing tells apart. Each run is an
// active root run of this conversation, as listing returns it; each request
// is the one listing read for it.
func askRouteStateCases() []askRouteStateCase {
	run := func(status string, wait map[string]any) map[string]any {
		row := map[string]any{"id": "v1:work:run:r1", "goalId": "v1:work:goal:g1", "status": status, "input": map[string]any{"conversation": map[string]any{"id": "conversation"}}}
		if wait != nil {
			row["waitingOn"] = wait
		}
		return row
	}
	waiting := func(kind string) map[string]any {
		return run("waiting", map[string]any{"kind": kind, "subject": "v1:work:approval:a1"})
	}
	// parked is the run waiting on request a1, the way the work spine parks it.
	parked := func(kind string) map[string]any {
		return run("waiting", map[string]any{"kind": "approval", "subject": "v1:work:approval:a1", "approvalKind": kind})
	}
	request := func(fields map[string]any) map[string]any {
		fields["id"] = "v1:work:approval:a1"
		return fields
	}
	retryAbandon := []any{map[string]any{"label": "Retry", "value": "retry"}, map[string]any{"label": "Abandon", "value": "abandon"}}
	return []askRouteStateCase{
		{"preparing", run("compiling", nil), nil, "preparing", ""},
		{"working", run("running", nil), nil, "working", ""},
		{"a retry", waiting("retry"), nil, "waiting to resume", ""},
		{"a timer", waiting("timer"), nil, "waiting to resume", ""},
		{"no model available", parked("inferenceUnavailable"), request(map[string]any{"kind": "inferenceUnavailable"}), "waiting to resume", ""},
		{"a question", parked("feedback"), request(map[string]any{"kind": "feedback", "question": "Which city?", "subject": map[string]any{"question": "Which city?"}}), "waiting for your answer", "Which city?"},
		{"a failure question", parked("planReview"), request(map[string]any{"kind": "planReview", "question": "The search failed. Retry?", "options": retryAbandon, "subject": map[string]any{"symptom": "plan"}}), "waiting for your answer", "The search failed. Retry?"},
		{"a permission", parked("sideEffect"), request(map[string]any{"kind": "sideEffect", "evidence": map[string]any{"reason": "Sends an email outside your organization"}}), "waiting for your permission", "Sends an email outside your organization"},
		{"a scope elevation", parked("scopeElevation"), request(map[string]any{"kind": "scopeElevation", "question": "Use your browser?"}), "waiting for your permission", "Use your browser?"},
		{"a budget checkpoint", parked("budget"), request(map[string]any{"kind": "budget", "subject": map[string]any{"resumeKind": "checkpoint", "continuationId": "c1"}}), "paused on its budget", ""},
		{"a budget review", parked("budget"), request(map[string]any{"kind": "budget", "subject": map[string]any{"ceiling": "maxModelCalls"}}), "waiting on a review", ""},
		{"a plan review", parked("planReview"), request(map[string]any{"kind": "planReview", "subject": map[string]any{"patches": []any{}}}), "waiting on a review", ""},
		{"a document review", parked("feedback"), request(map[string]any{"kind": "feedback", "question": "Accept the edits?", "subject": map[string]any{"reviewType": "document"}}), "waiting on a review", "Accept the edits?"},
		// Where the prompt and delivery used to disagree: a wait the request
		// phase cannot judge takes work guidance, read when the run resumes.
		{"a request that could not be read", parked("feedback"), nil, "waiting to resume", ""},
		{"a request it no longer waits on", parked("feedback"), map[string]any{"id": "v1:work:approval:b2", "kind": "feedback", "question": "Which city?", "subject": map[string]any{"question": "Which city?"}}, "waiting to resume", ""},
		{"a wait of another kind", waiting("feedback"), nil, "waiting to resume", ""},
		{"no model available, said only on the request", parked(""), request(map[string]any{"kind": "inferenceUnavailable"}), "waiting on a review", ""},
	}
}

// The words the prompt uses for what each goal is doing, and what it waits on
// when it waits on the person.
func TestAskRouteDataNamesEachGoalState(t *testing.T) {
	for _, tc := range askRouteStateCases() {
		t.Run(tc.name, func(t *testing.T) {
			data := askRouteData("Actually, Phoenix", nil, []askRouteGoal{{Handle: "g1", Title: "Weather", Request: "Weather?", Run: tc.run, Approval: tc.approval}}, nil, nil, time.Now())
			goal := data["goals"].([]any)[0].(map[string]any)
			require.Equal(t, tc.state, goal["state"])
			waitingOn, _ := goal["waitingOn"].(string)
			require.Equal(t, tc.waitingOn, waitingOn)
		})
	}
}

// What a routing call is told a goal is doing is the delivery it then gets: a
// goal shown waiting for an answer, a permission or its budget is answered
// through its request, one shown waiting on a review is Not delivered, and
// every other one takes the message as work guidance. The model is never told
// a goal will read a message that delivery records Not delivered.
func TestAskRouteGoalStateIsTheDeliveryItGets(t *testing.T) {
	promised := map[string]string{
		"preparing": routeQueued, "working": routeQueued, "waiting to resume": routeQueued,
		"waiting for your answer": "request", "waiting for your permission": "request", "paused on its budget": "request",
		"waiting on a review": routeNotDelivered + "/" + routeReasonReview,
	}
	for _, tc := range askRouteStateCases() {
		t.Run(tc.name, func(t *testing.T) {
			state, _ := askRouteGoalState(tc.run, tc.approval)
			want, known := promised[state]
			require.True(t, known, "state %q promises nothing", state)
			goal := askRouteGoal{Handle: "g1", GoalID: "g1", RunID: "r1", Title: "Weather", Run: tc.run, Approval: tc.approval}
			got := "request"
			// The request phase answers what it can; the locked phase records
			// the rest by the run as it reads it, here unchanged since listing.
			if kind, _, _ := routeDeliveryFor(tc.run, tc.approval); kind != deliverApproval {
				target := guidanceTargetsLocked("conversation", askTranscript{}, []askRouteGoal{goal}, map[string]map[string]any{"r1": tc.run}, "Actually, Phoenix.")[0]
				got = target.Status
				if target.Reason != "" {
					got += "/" + target.Reason
				}
			}
			require.Equal(t, want, got, "shown as %q", state)
		})
	}
}

// What askRouteData builds is exactly what the installed prompt accepts: its
// input schema admits no key it does not declare and no null where it expects
// a list, and is checked before the template renders.
func TestAskRouteDataRendersTheRoutingPrompt(t *testing.T) {
	prompts, _ := loadCorpusPrompts(t)
	prompt, ok := prompts.Get("askMessageRouting")
	require.True(t, ok, "the routing prompt is not installed")
	require.Equal(t, "fast", prompt.Level)
	render := func(data map[string]any) string {
		t.Helper()
		normalized, err := normalizeAIData(data)
		require.NoError(t, err)
		require.NoError(t, prompt.ValidateData(normalized))
		text, err := prompt.Render(normalized)
		require.NoError(t, err)
		return text
	}
	turns := []AskTurn{{ID: "t1", Prompt: "What's the weather in Tucson?", Answer: "Checking the weather in Tucson.", State: "done"}}
	now := time.Date(2026, 10, 19, 17, 0, 0, 0, time.UTC)

	plain := render(askRouteData("Actually, I'm in Phoenix", turns, routeGoals(), nil, nil, now))
	require.Contains(t, plain, `- g1: "Weather in Tucson"`)
	require.Contains(t, plain, "State: waiting for your answer: Which quarter?")
	require.Contains(t, plain, "Person: What's the weather in Tucson?\nMemQL: Checking the weather in Tucson.")
	require.Contains(t, plain, `"Actually, I'm in Phoenix"`)
	require.NotContains(t, plain, "STOP QUESTIONS WAITING")
	require.NotContains(t, plain, "YOUR QUESTION WAITING")
	require.NotContains(t, plain, "YOUR LAST ANSWER WAS REFUSED")

	clarification := &AskTurn{ID: "t2", Prompt: "Make it shorter", Answer: "The report or the hike plan?", State: "done", Routed: &AskRouting{Clarify: true}}
	goals := routeGoals()
	goals[0].Updates = []string{"Use Celsius"}
	data := askRouteData("The report", append(turns, *clarification), goals, []string{"g3"}, clarification, now)
	data["previousError"] = "goal g7 is not one of the active goals"
	full := render(data)
	require.Contains(t, full, "You asked whether to stop: g3")
	require.Contains(t, full, `Their previous message: "Make it shorter"`)
	require.Contains(t, full, `You asked: "The report or the hike plan?"`)
	require.Contains(t, full, "goal g7 is not one of the active goals")
	require.Contains(t, full, `- "Use Celsius"`)
}

// askRouteModel answers each structured call with its next scripted answer and
// keeps what each call was shown.
type askRouteModel struct {
	answers []string
	err     error
	// stall, when set, makes every call answer nothing, the way a routing
	// door that hangs does: it waits until its context ends, or until the
	// test closes stall to let it go.
	stall   <-chan struct{}
	prompts []string
	schemas []common.StructuredSchema
}

func (m *askRouteModel) CallChatStructured(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, error) {
	if m.stall != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-m.stall:
			return "", errors.New("the test let a stalled routing call go")
		}
	}
	if m.err != nil {
		return "", m.err
	}
	if len(messages) > 0 {
		m.prompts = append(m.prompts, messages[0].Content)
	}
	m.schemas = append(m.schemas, schema)
	if len(m.answers) == 0 {
		return "", errors.New("the model was asked more often than the test scripted")
	}
	answer := m.answers[0]
	m.answers = m.answers[1:]
	return answer, nil
}

// askRoutingRecipe runs dsl/os/automations.memql's askMessageRouting statement
// by statement over the operations the engine binds, as the automation
// interpreter does. component/memql cannot import the interpreter
// (workflowhost imports this package), so the installed recipe itself runs in
// app/.
//
// It gives each decision the 30 seconds the installed recipe does.
var askRoutingRecipe = askRoutingRecipeWithin(30)

// askRoutingRecipeWithin is the routing recipe giving each decision
// timeoutSeconds, as the installed one passes it.
func askRoutingRecipeWithin(timeoutSeconds int) ScopedWorkflowRunner {
	return func(ctx context.Context, name string, _ map[string]any, ops map[string]WorkflowOperation) (any, error) {
		if name != "askMessageRouting" {
			return nil, fmt.Errorf("workflow %q is not the routing recipe", name)
		}
		goals, err := ops["askRouteGoals"](ctx, map[string]any{})
		if err != nil {
			return nil, err
		}
		if goals.(map[string]any)["count"] == 0 {
			return "intake", nil
		}
		first, err := ops["askRouteDecide"](ctx, map[string]any{"prompt": "askMessageRouting", "previousError": "", "timeoutSeconds": timeoutSeconds})
		if err != nil {
			return nil, err
		}
		if first.(map[string]any)["valid"] == true {
			return "routed", nil
		}
		second, err := ops["askRouteDecide"](ctx, map[string]any{"prompt": "askMessageRouting", "previousError": first.(map[string]any)["error"], "timeoutSeconds": timeoutSeconds})
		if err != nil {
			return nil, err
		}
		if second.(map[string]any)["valid"] == true {
			return "routed", nil
		}
		return "intake", nil
	}
}

// The recipe chooses how long one routing decision may take, and Go holds it
// to 1 to 60 seconds: 30 when the recipe says nothing a number can.
func TestAskRouteDecideTimeout(t *testing.T) {
	for _, c := range []struct {
		name string
		args map[string]any
		want time.Duration
	}{
		{"what the recipe asks for", map[string]any{"timeoutSeconds": 30}, 30 * time.Second},
		{"a decoded JSON number", map[string]any{"timeoutSeconds": float64(12)}, 12 * time.Second},
		{"a 64-bit integer", map[string]any{"timeoutSeconds": int64(45)}, 45 * time.Second},
		{"a number literal kept as written", map[string]any{"timeoutSeconds": json.Number("20")}, 20 * time.Second},
		{"nothing asked", map[string]any{}, 30 * time.Second},
		{"null", map[string]any{"timeoutSeconds": nil}, 30 * time.Second},
		{"not a number", map[string]any{"timeoutSeconds": "soon"}, 30 * time.Second},
		{"the least", map[string]any{"timeoutSeconds": 1}, time.Second},
		{"zero", map[string]any{"timeoutSeconds": 0}, time.Second},
		{"less than nothing", map[string]any{"timeoutSeconds": -5}, time.Second},
		{"the most", map[string]any{"timeoutSeconds": 60}, time.Minute},
		{"more than the most", map[string]any{"timeoutSeconds": 600}, time.Minute},
		{"more than any int holds", map[string]any{"timeoutSeconds": 1e300}, time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, askRouteDecideTimeout(c.args))
		})
	}
}

// askRouteDecideEngine is an engine with the installed prompts, a resolver
// that hands every call to model, and the recipe above as its scoped runner.
func askRouteDecideEngine(t *testing.T, model *askRouteModel) (*MemQLEngine, *[]airoute.ResolveRequest) {
	t.Helper()
	prompts, _ := loadCorpusPrompts(t)
	providers := newProviderRegistry()
	e := &MemQLEngine{providers: providers, prompts: prompts, modelSeam: &modelSeam{}}
	e.aiRuntime = newTestAIRuntime(prompts, providers, aiCacheConfig{})
	e.aiRuntime.seam = e.modelSeam
	requests := &[]airoute.ResolveRequest{}
	e.SetAIResolver(testAIResolver{fn: func(_ context.Context, req airoute.ResolveRequest) (ResolvedProvider, error) {
		*requests = append(*requests, req)
		return ResolvedProvider{Client: model, Resolution: airoute.Resolution{ProviderName: "fleet:router", Model: "router-test"}}, nil
	}})
	e.SetScopedWorkflowRunner(askRoutingRecipe)
	return e, requests
}

func TestDecideAskRouteRetriesOnceThenIntake(t *testing.T) {
	goals := routeGoals()
	pending := map[string]bool{"g3": true}
	data := askRouteData("Actually, I'm in Phoenix", nil, goals, []string{"g3"}, nil, time.Date(2026, 10, 19, 17, 0, 0, 0, time.UTC))
	invalid := routeAnswer(map[string]any{"updates": []any{routeUpdate("g7", "")}, "reply": "Sent."})
	phoenix := routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "Weather in Phoenix")}, "reply": "Switching the weather check to Phoenix."})
	routed := askRouteDecision{Updates: []askRouteUpdate{{Goal: goals[0], Title: "Weather in Phoenix"}}, Reply: "Switching the weather check to Phoenix."}

	t.Run("invalid twice is intake", func(t *testing.T) {
		model := &askRouteModel{answers: []string{invalid, invalid}}
		e, requests := askRouteDecideEngine(t, model)
		decision, outcome, err := e.decideAskRoute(userCtx("alice"), data, goals, pending, false)
		require.NoError(t, err)
		require.Equal(t, "intake", outcome)
		require.Equal(t, askRouteDecision{}, decision)
		require.Len(t, model.prompts, 2, "one decision and one retry, no more")
		require.NotContains(t, model.prompts[0], "YOUR LAST ANSWER WAS REFUSED")
		require.Contains(t, model.prompts[1], "YOUR LAST ANSWER WAS REFUSED")
		require.Contains(t, model.prompts[1], "g7", "the retry is told what was refused")
		for _, req := range *requests {
			require.Equal(t, airoute.LevelFast, req.Level)
			require.Equal(t, "askMessageRouting", req.PromptName)
		}
		for _, schema := range model.schemas {
			require.Equal(t, "ask_message_route", schema.Name)
			require.True(t, schema.Strict)
		}
	})

	t.Run("a valid retry routes", func(t *testing.T) {
		model := &askRouteModel{answers: []string{invalid, phoenix}}
		e, _ := askRouteDecideEngine(t, model)
		decision, outcome, err := e.decideAskRoute(userCtx("alice"), data, goals, pending, false)
		require.NoError(t, err)
		require.Equal(t, "routed", outcome)
		require.Equal(t, routed, decision)
		require.Len(t, model.prompts, 2)
	})

	t.Run("a valid answer is not asked again", func(t *testing.T) {
		model := &askRouteModel{answers: []string{phoenix}}
		e, _ := askRouteDecideEngine(t, model)
		decision, outcome, err := e.decideAskRoute(userCtx("alice"), data, goals, pending, false)
		require.NoError(t, err)
		require.Equal(t, "routed", outcome)
		require.Equal(t, routed, decision)
		require.Len(t, model.prompts, 1)
	})

	// A decision takes as long as the recipe allows and no longer: a routing
	// door that does not answer is a routing failure, never a wait the
	// person's message is held behind. The failure says it ran out of time.
	t.Run("a decision past its deadline is a routing failure", func(t *testing.T) {
		release := make(chan struct{})
		var letGo sync.Once
		t.Cleanup(func() { letGo.Do(func() { close(release) }) })
		e, _ := askRouteDecideEngine(t, &askRouteModel{stall: release})
		e.SetScopedWorkflowRunner(askRoutingRecipeWithin(1))
		started := time.Now()
		done := make(chan error, 1)
		go func() {
			decision, outcome, err := e.decideAskRoute(userCtx("alice"), data, goals, pending, false)
			if err == nil {
				err = fmt.Errorf("routing decided %q: %+v", outcome, decision)
			}
			done <- err
		}()
		select {
		case err := <-done:
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.ErrorContains(t, err, "no routing decision within 1s")
			require.Less(t, time.Since(started), 10*time.Second)
		case <-time.After(15 * time.Second):
			letGo.Do(func() { close(release) })
			<-done
			t.Fatal("the routing decision outlived its deadline")
		}
	})

	t.Run("a routing failure is an error, not a decision", func(t *testing.T) {
		model := &askRouteModel{err: errors.New("no model is available for this call")}
		e, _ := askRouteDecideEngine(t, model)
		decision, outcome, err := e.decideAskRoute(userCtx("alice"), data, goals, pending, false)
		require.ErrorContains(t, err, "no model is available")
		require.Empty(t, outcome)
		require.Equal(t, askRouteDecision{}, decision)
	})

	t.Run("no active goals asks no model", func(t *testing.T) {
		model := &askRouteModel{answers: []string{phoenix}}
		e, _ := askRouteDecideEngine(t, model)
		decision, outcome, err := e.decideAskRoute(userCtx("alice"), askRouteData("hi", nil, nil, nil, nil, time.Now()), nil, nil, false)
		require.NoError(t, err)
		require.Equal(t, "intake", outcome)
		require.Equal(t, askRouteDecision{}, decision)
		require.Empty(t, model.prompts)
	})

	// The recipe chooses the retry; Go holds it to one.
	t.Run("a third decision is refused", func(t *testing.T) {
		model := &askRouteModel{answers: []string{invalid, invalid, phoenix}}
		e, _ := askRouteDecideEngine(t, model)
		e.SetScopedWorkflowRunner(func(ctx context.Context, _ string, _ map[string]any, ops map[string]WorkflowOperation) (any, error) {
			for range 3 {
				if _, err := ops["askRouteDecide"](ctx, map[string]any{"prompt": "askMessageRouting", "previousError": "again"}); err != nil {
					return nil, err
				}
			}
			return "routed", nil
		})
		_, outcome, err := e.decideAskRoute(userCtx("alice"), data, goals, pending, false)
		require.ErrorContains(t, err, "one retry")
		require.Empty(t, outcome)
		require.Len(t, model.prompts, 2, "the third decision reached the model")
	})

	t.Run("routed with no valid decision is an error", func(t *testing.T) {
		e, _ := askRouteDecideEngine(t, &askRouteModel{})
		e.SetScopedWorkflowRunner(func(context.Context, string, map[string]any, map[string]WorkflowOperation) (any, error) {
			return "routed", nil
		})
		decision, outcome, err := e.decideAskRoute(userCtx("alice"), data, goals, pending, false)
		require.Error(t, err)
		require.Empty(t, outcome)
		require.Equal(t, askRouteDecision{}, decision)
	})

	_, retried := data["previousError"]
	require.False(t, retried, "a retry's refusal leaked into the caller's data")
}

// The operations exist only inside decideAskRoute's scope, which supplies the
// goals and the person's message. Called directly they refuse, so no caller
// can spend a model call with a prompt and data of its own choosing.
func TestAskRouteOperationsRefuseOutsideTheirScope(t *testing.T) {
	// The shipped declarations, so each builtin is followed to the executor it
	// names; building the handlers also refuses any native executor the tree
	// declares and the engine lacks.
	registry := newFunctionRegistry()
	_, err := LoadUnifiedBuiltins(nil, registry)
	require.NoError(t, err)
	e := &MemQLEngine{initialized: true, functions: registry}
	require.NoError(t, e.initBuiltinExecutorHandlers())
	declared := map[string]*Function{}
	for _, fn := range registry.Snapshot() {
		if fn != nil {
			declared[fn.Name] = fn
		}
	}
	for _, name := range []string{"askRouteGoals", "askRouteDecide"} {
		fn := declared[name]
		require.NotNil(t, fn, "builtin %s is not declared", name)
		handler := e.builtinExecutorHandlers[fn.Executor]
		require.NotNil(t, handler, "builtin %s names executor %q, which the engine lacks", name, fn.Executor)
		rows, err := handler(userCtx("alice"), map[string]any{"prompt": "askMessageRouting", "previousError": ""}, 0)
		require.EqualError(t, err, "ask routing operation requires its authorized workflow scope")
		require.Empty(t, rows)
	}
}
