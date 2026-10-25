package memql

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// guidanceTurns are earlier routed messages a run already holds as work
// guidance, one turn each.
func guidanceTurns(run string, texts ...string) []AskTurn {
	turns := make([]AskTurn, len(texts))
	for n, text := range texts {
		turns[n] = AskTurn{ID: fmt.Sprintf("earlier-%d", n), Prompt: text, Routed: &AskRouting{Targets: []AskRouteTarget{
			{GoalID: "goal", RunID: run, Status: routeReceived, Text: text},
		}}}
	}
	return turns
}

func repeatedText(text string, n int) []string {
	texts := make([]string, n)
	for i := range texts {
		texts[i] = text
	}
	return texts
}

// thisConversation is the conversation these tests route a message in.
const thisConversation = "conversation"

// conversationRun is a run started from thisConversation: it names the
// conversation on its input, which is where the agent's read finds it.
func conversationRun(fields map[string]any) map[string]any {
	run := map[string]any{"input": map[string]any{"conversation": map[string]any{"id": thisConversation}}}
	for key, value := range fields {
		run[key] = value
	}
	return run
}

// readerLoad is what the agent's read hands it for run: every turn's work
// guidance for that run, counted the way ReadWorkGuidance bounds it.
func readerLoad(transcript askTranscript, run map[string]any) (messages, runes int) {
	for n := range transcript.Turns {
		if target := workGuidanceTarget(&transcript.Turns[n], run); target != nil {
			messages++
			runes += utf8.RuneCountInString(target.Text)
		}
	}
	return messages, runes
}

// Every row of the spec's delivery table, plus a goal that is no longer
// active. Guidance is read by the agent loop; approval goes through the
// request the goal waits on; none is not delivered at all.
func TestRouteDeliveryForEveryState(t *testing.T) {
	run := func(status string, wait map[string]any) map[string]any {
		run := map[string]any{"id": "run", "goalId": "goal", "status": status}
		if wait != nil {
			run["waitingOn"] = wait
		}
		return run
	}
	parked := func(approvalKind string) map[string]any {
		return run("waiting", map[string]any{"kind": "approval", "subject": "request", "approvalKind": approvalKind})
	}
	request := func(kind string, subject map[string]any, options []any) map[string]any {
		return map[string]any{"id": "v1:work:approval:request", "kind": kind, "subject": subject, "options": options, "decision": ""}
	}
	failure := []any{map[string]any{"label": "Revise plan", "value": "retry"}, map[string]any{"label": "Abandon", "value": "abandon"}}
	stopping := parked("feedback")
	stopping["cancelRequested"] = true
	for _, c := range []struct {
		name           string
		run, approval  map[string]any
		kind           routeDelivery
		status, reason string
	}{
		{"working", run("running", nil), nil, deliverGuidance, routeQueued, ""},
		{"preparing", run("compiling", nil), nil, deliverGuidance, routeQueued, ""},
		{"waiting on a retry", run("waiting", map[string]any{"kind": "retry", "subject": "section"}), nil, deliverGuidance, routeQueued, ""},
		{"waiting on a re-plan", run("waiting", map[string]any{"kind": "replan", "subject": "section"}), nil, deliverGuidance, routeQueued, ""},
		{"waiting on a repair", run("waiting", map[string]any{"kind": "repair", "subject": "section"}), nil, deliverGuidance, routeQueued, ""},
		{"waiting on a timer", run("waiting", map[string]any{"kind": "timer", "subject": "section"}), nil, deliverGuidance, routeQueued, ""},
		{"waiting on something external", run("waiting", map[string]any{"kind": "external", "subject": "section"}), nil, deliverGuidance, routeQueued, ""},
		{"waiting on a sub-run", run("waiting", map[string]any{"kind": "subrun", "subject": "child"}), nil, deliverGuidance, routeQueued, ""},
		{"waiting for a model to be reachable", parked("inferenceUnavailable"), request("inferenceUnavailable", map[string]any{"code": "every_door_shut"}, nil), deliverGuidance, routeQueued, ""},
		{"waiting on your question", parked("feedback"), request("feedback", map[string]any{"kind": "text"}, nil), deliverApproval, routeAnswered, ""},
		{"waiting on a failure question", parked("planReview"), request("planReview", map[string]any{"symptom": "plan", "stepKey": "section", "errorMessage": "no route"}, failure), deliverApproval, routeAnswered, ""},
		{"waiting on a permission to act", parked("sideEffect"), request("sideEffect", map[string]any{"command": "send the email"}, nil), deliverApproval, routeNotApproved, ""},
		{"waiting on computer access", parked("scopeElevation"), request("scopeElevation", map[string]any{"agentId": "agent"}, nil), deliverApproval, routeNotApproved, ""},
		{"waiting for progress direction", parked("feedback"), request("feedback", map[string]any{"progressLimit": true, "resumeKind": "checkpoint", "continuationId": "continuation"}, failure), deliverApproval, routeAnswered, ""},
		{"at its overall limit", parked("feedback"), request("feedback", map[string]any{"progressLimit": true, "continuationAllowed": false}, failure), deliverNone, routeNotDelivered, routeReasonReview},
		{"retired budget checkpoint", parked("budget"), request("budget", map[string]any{"resumeKind": "checkpoint", "continuationId": "continuation"}, nil), deliverNone, routeNotDelivered, routeReasonReview},
		{"stopped by a budget rule with no checkpoint", parked("budget"), request("budget", map[string]any{"symptom": "budget", "stepKey": "section"}, failure), deliverNone, routeNotDelivered, routeReasonReview},
		{"waiting on a plan review", parked("planReview"), request("planReview", map[string]any{"plan": "draft"}, nil), deliverNone, routeNotDelivered, routeReasonReview},
		{"waiting on a draft review", parked("feedback"), request("feedback", map[string]any{"kind": "text", "reviewType": "document"}, nil), deliverNone, routeNotDelivered, routeReasonReview},
		{"waiting on a procedure promotion", parked("procedurePromotion"), request("procedurePromotion", map[string]any{"constructId": "procedure"}, nil), deliverNone, routeNotDelivered, routeReasonReview},
		{"waiting on a request it could not read", parked("feedback"), nil, deliverNone, routeNotDelivered, routeReasonReview},
		{"waiting on another request than the one read", parked("feedback"), map[string]any{"id": "other", "kind": "feedback", "subject": map[string]any{"kind": "text"}, "decision": ""}, deliverNone, routeNotDelivered, routeReasonReview},
		{"waiting on no request in particular", run("waiting", map[string]any{"kind": "approval", "approvalKind": "feedback"}), map[string]any{"kind": "feedback", "subject": map[string]any{"kind": "text"}, "decision": ""}, deliverNone, routeNotDelivered, routeReasonReview},
		{"waiting on a question in the older form", run("waiting", map[string]any{"kind": "feedback", "subject": "request"}), request("feedback", map[string]any{"kind": "text"}, nil), deliverNone, routeNotDelivered, routeReasonReview},
		{"finished", run("succeeded", nil), nil, deliverNone, routeNotReceived, routeReasonEnded},
		{"failed", run("failed", nil), nil, deliverNone, routeNotReceived, routeReasonEnded},
		{"cancelled", run("cancelled", nil), nil, deliverNone, routeNotReceived, routeReasonEnded},
		{"asked to stop while it waits on your question", stopping, request("feedback", map[string]any{"kind": "text"}, nil), deliverNone, routeNotReceived, routeReasonEnded},
		{"unreadable", nil, nil, deliverNone, routeNotReceived, routeReasonEnded},
	} {
		t.Run(c.name, func(t *testing.T) {
			kind, status, reason := routeDeliveryFor(c.run, c.approval)
			require.Equal(t, c.kind, kind)
			require.Equal(t, c.status, status)
			require.Equal(t, c.reason, reason)
		})
	}
}

// One message remains bounded; lifetime journal growth is not an intake limit.
func TestGuidanceTargetsLockedBoundsEachMessage(t *testing.T) {
	run := conversationRun(map[string]any{"id": "run", "goalId": "goal", "status": "running"})
	goals := []askRouteGoal{{Handle: "g1", TurnID: "weather", GoalID: "goal", RunID: "run", Title: "Weather in Tucson", Run: run}}
	fresh := map[string]map[string]any{"run": run}
	deliver := func(earlier []AskTurn, text string) AskRouteTarget {
		t.Helper()
		targets := guidanceTargetsLocked(thisConversation, askTranscript{Turns: earlier}, goals, fresh, text)
		require.Len(t, targets, 1)
		return targets[0]
	}
	queued := func(text string) AskRouteTarget {
		return AskRouteTarget{GoalID: "goal", RunID: "run", Title: "Weather in Tucson", Status: routeQueued, Text: text}
	}
	limited := func(text string) AskRouteTarget {
		return AskRouteTarget{GoalID: "goal", RunID: "run", Title: "Weather in Tucson", Status: routeNotDelivered, Reason: routeReasonLimit, Text: text}
	}

	require.Equal(t, queued("Use Celsius."), deliver(guidanceTurns("run", repeatedText("Note.", 15)...), "Use Celsius."), "the sixteenth message fits")
	require.Equal(t, queued("Use Celsius."), deliver(guidanceTurns("run", repeatedText("Note.", 16)...), "Use Celsius."), "later guidance stays deliverable")

	// Runes, not bytes: every letter here is two bytes.
	twenty := strings.Repeat("ü", 20)
	full := []string{strings.Repeat("é", 4000), strings.Repeat("é", 4000), strings.Repeat("é", 4000), strings.Repeat("é", 3990)}
	require.Equal(t, queued(twenty), deliver(guidanceTurns("run", full...), twenty), "15,990 runes and 20 more")
	exact := []string{strings.Repeat("é", 4000), strings.Repeat("é", 4000), strings.Repeat("é", 4000), strings.Repeat("é", 3980)}
	require.Equal(t, queued(twenty), deliver(guidanceTurns("run", exact...), twenty), "15,980 runes and 20 more is exactly 16,000")

	require.Equal(t, queued(strings.Repeat("é", 4000)), deliver(nil, strings.Repeat("é", 4000)))
	require.Equal(t, limited(strings.Repeat("é", 4001)), deliver(nil, strings.Repeat("é", 4001)), "one message is at most 4,000 runes")
}

// Only guidance for the selected run participates in its chronological journal.
func TestGuidanceTargetsLockedCountsWhatTheReaderReads(t *testing.T) {
	run := conversationRun(map[string]any{"id": "run", "goalId": "goal", "status": "running"})
	goals := []askRouteGoal{{Handle: "g1", TurnID: "weather", GoalID: "goal", RunID: "run", Title: "Weather in Tucson", Run: run}}
	fresh := map[string]map[string]any{"run": run}
	long := strings.Repeat("a", 4000)
	routed := func(id string, target AskRouteTarget) AskTurn {
		return AskTurn{ID: id, Routed: &AskRouting{Targets: []AskRouteTarget{target}}}
	}
	transcript := askTranscript{Turns: guidanceTurns("run", repeatedText("Note.", 14)...)}
	transcript.Turns = append(transcript.Turns,
		routed("canonical", AskRouteTarget{GoalID: "goal", RunID: "v1:work:run:run", Status: routeQueued, Text: "Note."}),
		routed("answered", AskRouteTarget{GoalID: "goal", RunID: "run", Status: routeAnswered, ApprovalID: "question", Text: long}),
		routed("not-approved", AskRouteTarget{GoalID: "goal", RunID: "run", Status: routeNotApproved, ApprovalID: "permission", Text: long}),
		routed("saved", AskRouteTarget{GoalID: "goal", RunID: "run", Status: routeSaved, ApprovalID: "budget", Text: long}),
		routed("through-a-request", AskRouteTarget{GoalID: "goal", RunID: "run", Status: routeQueued, ApprovalID: "permission", Text: long}),
		routed("capped", AskRouteTarget{GoalID: "goal", RunID: "run", Status: routeNotDelivered, Reason: routeReasonLimit, Text: long}),
		routed("too-late", AskRouteTarget{GoalID: "goal", RunID: "run", Status: routeNotReceived, Reason: routeReasonEnded, Text: long}),
		AskTurn{ID: "plain", Prompt: long, Answer: long},
	)
	for n := range 4 {
		transcript.Turns = append(transcript.Turns, routed(fmt.Sprintf("elsewhere-%d", n), AskRouteTarget{GoalID: "other", RunID: "other-run", Status: routeQueued, Text: long}))
	}
	messages, runes := readerLoad(transcript, run)
	require.Equal(t, []int{15, 75}, []int{messages, runes})

	first := guidanceTargetsLocked(thisConversation, transcript, goals, fresh, "Use Celsius.")
	require.Equal(t, []AskRouteTarget{{GoalID: "goal", RunID: "run", Title: "Weather in Tucson", Status: routeQueued, Text: "Use Celsius."}}, first)
	transcript.Turns = append(transcript.Turns, AskTurn{ID: "celsius", Prompt: "Use Celsius.", Routed: &AskRouting{Targets: first}})
	messages, runes = readerLoad(transcript, run)
	require.Equal(t, []int{16, 87}, []int{messages, runes})

	second := guidanceTargetsLocked(thisConversation, transcript, goals, fresh, "Use Fahrenheit.")
	require.Equal(t, []AskRouteTarget{{GoalID: "goal", RunID: "run", Title: "Weather in Tucson", Status: routeQueued, Text: "Use Fahrenheit."}}, second)

	// Many individually bounded messages remain deliverable.
	var arriving askTranscript
	for n := range 40 {
		targets := guidanceTargetsLocked(thisConversation, arriving, goals, fresh, strings.Repeat("é", 1+(n*1777)%4000))
		require.Len(t, targets, 1)
		require.Equal(t, routeQueued, targets[0].Status)
		arriving.Turns = append(arriving.Turns, AskTurn{ID: fmt.Sprintf("arriving-%d", n), Routed: &AskRouting{Targets: targets}})
	}
	messages, _ = readerLoad(arriving, run)
	require.Equal(t, 40, messages)

}

// Lifetime history is paged, so another bounded message remains deliverable.
// The per-message limit still rejects oversized guidance without changing what
// the agent can read from the retained journal.
func TestDeliverRouteBeyondFormerLifetimeCapsKeepsGuidanceReadable(t *testing.T) {
	run := conversationRun(map[string]any{"id": "run", "goalId": "goal", "status": "running"})
	goals := []askRouteGoal{{Handle: "g1", TurnID: "weather", GoalID: "goal", RunID: "run", Title: "Weather in Tucson", Request: "What's the weather in Tucson?", Run: run}}
	for _, c := range []struct {
		name      string
		earlier   []string
		text      string
		oversized bool
	}{
		{"sixteen messages already", repeatedText("Note.", 16), "Use Celsius.", false},
		{"15,990 runes and a 20-rune message", []string{strings.Repeat("é", 4000), strings.Repeat("é", 4000), strings.Repeat("é", 4000), strings.Repeat("é", 3990)}, strings.Repeat("ü", 20), false},
		{"one oversized message", repeatedText("Note.", 16), strings.Repeat("é", 4001), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			transcript := askTranscript{Turns: guidanceTurns("run", c.earlier...)}
			beforeMessages, beforeRunes := readerLoad(transcript, run)
			kind, _, _ := routeDeliveryFor(run, nil)
			require.Equal(t, deliverGuidance, kind)
			// No request is pending, so the request phase must not call anything.
			delivered, rest := (&MemQLEngine{}).deliverApprovalTargets(context.Background(), goals, c.text)
			require.Empty(t, delivered)
			require.Equal(t, goals, rest)
			targets := guidanceTargetsLocked(thisConversation, transcript, rest, map[string]map[string]any{"run": run}, c.text)
			want := AskRouteTarget{GoalID: "goal", RunID: "run", Title: "Weather in Tucson", Status: routeQueued, Text: c.text}
			if c.oversized {
				want.Status, want.Reason = routeNotDelivered, routeReasonLimit
			}
			require.Equal(t, []AskRouteTarget{want}, targets)
			transcript.Turns = append(transcript.Turns, AskTurn{ID: "later", Prompt: c.text, Routed: &AskRouting{Targets: targets}})
			messages, runes := readerLoad(transcript, run)
			if c.oversized {
				require.Equal(t, []int{beforeMessages, beforeRunes}, []int{messages, runes})
			} else {
				require.Equal(t, []int{beforeMessages + 1, beforeRunes + len([]rune(c.text))}, []int{messages, runes})
			}
		})
	}
}

// Under the lock each goal is judged by its run as it is now. One that ended
// did not receive the message. One still parked on the request it was listed
// with is not given guidance: a review that takes none is Not delivered, and
// an answerable request the request phase did not answer was refused. Every
// other active goal -- working, preparing, resumed, or parked on something
// routing never saw -- takes work guidance.
func TestGuidanceTargetsLockedJudgesTheFreshRun(t *testing.T) {
	parked := func(id, approvalKind string) map[string]any {
		return conversationRun(map[string]any{"id": id, "goalId": "goal-" + id, "status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": "request-" + id, "approvalKind": approvalKind}})
	}
	request := func(id, kind string, subject map[string]any) map[string]any {
		return map[string]any{"id": "v1:work:approval:request-" + id, "kind": kind, "subject": subject, "decision": ""}
	}
	goal := func(id string, approval map[string]any) askRouteGoal {
		return askRouteGoal{Handle: "g-" + id, TurnID: "turn-" + id, GoalID: "goal-" + id, RunID: id, Title: "Goal " + id, Approval: approval}
	}
	question := request("question", "feedback", map[string]any{"kind": "text"})
	review := request("review", "planReview", map[string]any{"plan": "draft"})
	door := request("door", "inferenceUnavailable", map[string]any{"code": "every_door_shut"})
	resumed := request("resumed", "feedback", map[string]any{"kind": "text"})
	moved := request("moved", "feedback", map[string]any{"kind": "text"})
	fresh := map[string]map[string]any{
		"finished":  conversationRun(map[string]any{"id": "finished", "goalId": "goal-finished", "status": "succeeded"}),
		"stopping":  conversationRun(map[string]any{"id": "stopping", "goalId": "goal-stopping", "status": "running", "cancelRequested": true}),
		"review":    parked("review", "planReview"),
		"question":  parked("question", "feedback"),
		"door":      parked("door", "inferenceUnavailable"),
		"resumed":   conversationRun(map[string]any{"id": "resumed", "goalId": "goal-resumed", "status": "running", "waitingOn": map[string]any{}}),
		"moved":     parked("elsewhere", "feedback"),
		"new-wait":  parked("new-wait", "planReview"),
		"compiling": conversationRun(map[string]any{"id": "compiling", "goalId": "goal-compiling", "status": "compiling"}),
	}
	fresh["moved"]["id"] = "moved"
	goals := []askRouteGoal{
		goal("finished", nil), goal("missing", nil), goal("stopping", nil),
		goal("review", review), goal("question", question), goal("door", door),
		goal("resumed", resumed), goal("moved", moved), goal("new-wait", nil), goal("compiling", nil),
	}
	targets := guidanceTargetsLocked(thisConversation, askTranscript{}, goals, fresh, "Actually Phoenix.")
	target := func(id, status, reason, approval string) AskRouteTarget {
		return AskRouteTarget{GoalID: "goal-" + id, RunID: id, Title: "Goal " + id, Status: status, Reason: reason, ApprovalID: approval, Text: "Actually Phoenix."}
	}
	require.Equal(t, []AskRouteTarget{
		target("finished", routeNotReceived, routeReasonEnded, ""),
		target("missing", routeNotReceived, routeReasonEnded, ""),
		target("stopping", routeNotReceived, routeReasonEnded, ""),
		target("review", routeNotDelivered, routeReasonReview, "request-review"),
		target("question", routeNotDelivered, routeReasonRefused, "request-question"),
		target("door", routeQueued, "", ""),
		target("resumed", routeQueued, "", ""),
		target("moved", routeQueued, "", ""),
		target("new-wait", routeQueued, "", ""),
		target("compiling", routeQueued, "", ""),
	}, targets)
}

// The agent reads a run's work guidance only through the conversation on the
// run's own input. A run whose own conversation is another one -- or that has
// none, because it was started outside Ask and appears only in a private
// conversation opened for it -- would never read a message recorded here, so
// it is refused. A goal that ended, or one still parked on the request it was
// listed with, is judged by that first.
func TestGuidanceTargetsLockedReachesOnlyThisConversationsRuns(t *testing.T) {
	run := func(id, conversation string, fields map[string]any) map[string]any {
		run := map[string]any{"id": id, "goalId": "goal-" + id, "status": "running"}
		if conversation != "" {
			run["input"] = map[string]any{"conversation": map[string]any{"id": conversation}}
		}
		for key, value := range fields {
			run[key] = value
		}
		return run
	}
	review := map[string]any{"id": "v1:work:approval:request-review", "kind": "planReview", "subject": map[string]any{"plan": "draft"}, "decision": ""}
	fresh := map[string]map[string]any{
		"here":      run("here", thisConversation, nil),
		"canonical": run("canonical", "v1:os:askConversation:"+thisConversation, nil),
		"elsewhere": run("elsewhere", "another-conversation", nil),
		"outside":   run("outside", "", nil),
		"ended":     run("ended", "another-conversation", map[string]any{"status": "succeeded"}),
		"review":    run("review", "another-conversation", map[string]any{"status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": "request-review", "approvalKind": "planReview"}}),
	}
	goal := func(id string, approval map[string]any) askRouteGoal {
		return askRouteGoal{Handle: "g-" + id, TurnID: "turn-" + id, GoalID: "goal-" + id, RunID: id, Title: "Goal " + id, Approval: approval}
	}
	goals := []askRouteGoal{goal("here", nil), goal("canonical", nil), goal("elsewhere", nil), goal("outside", nil), goal("ended", nil), goal("review", review)}
	target := func(id, status, reason, approval string) AskRouteTarget {
		return AskRouteTarget{GoalID: "goal-" + id, RunID: id, Title: "Goal " + id, Status: status, Reason: reason, ApprovalID: approval, Text: "Actually Phoenix."}
	}
	want := []AskRouteTarget{
		target("here", routeQueued, "", ""),
		target("canonical", routeQueued, "", ""),
		target("elsewhere", routeNotDelivered, routeReasonRefused, ""),
		target("outside", routeNotDelivered, routeReasonRefused, ""),
		target("ended", routeNotReceived, routeReasonEnded, ""),
		target("review", routeNotDelivered, routeReasonReview, "request-review"),
	}
	require.Equal(t, want, guidanceTargetsLocked(thisConversation, askTranscript{}, goals, fresh, "Actually Phoenix."))
	require.Equal(t, want, guidanceTargetsLocked("v1:os:askConversation:"+thisConversation, askTranscript{}, goals, fresh, "Actually Phoenix."))
	for _, routed := range guidanceTargetsLocked("", askTranscript{}, goals[:4], fresh, "Actually Phoenix.") {
		require.Equal(t, routeNotDelivered, routed.Status, "no conversation is a run's own conversation: %s", routed.RunID)
	}
}
