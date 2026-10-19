package memql

// Applying a routing decision (design record
// docs/superpowers/specs/2026-10-19-ask-message-routing-design.md, "Apply"):
// the pure pieces -- the honest reply, which messages are routed at all, the
// stop questions and clarification routing is shown, and what a settled
// clarification delivers.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The model's reply is a proposal. It stands when every target was delivered;
// otherwise Go says plainly what happened, one sentence per goal in the order
// the message went to them.
func TestHonestRouteReply(t *testing.T) {
	model := "Switching the weather check to Phoenix."
	sent := func(title, status string) AskRouteTarget {
		return AskRouteTarget{GoalID: "goal-" + title, RunID: "run-" + title, Title: title, Status: status, Text: "Actually Phoenix."}
	}
	missed := func(title, status, reason string) AskRouteTarget {
		target := sent(title, status)
		target.Reason = reason
		return target
	}
	for _, c := range []struct {
		name    string
		targets []AskRouteTarget
		want    string
	}{
		{"every target delivered keeps the model's reply", []AskRouteTarget{sent("A", routeQueued), sent("B", routeReceived), sent("C", routeAnswered), sent("D", routeNotApproved), sent("E", routeSaved)}, model},
		{"nothing delivered to anyone keeps the model's reply", nil, model},
		{"one goal ended", []AskRouteTarget{sent("A", routeQueued), missed("B", routeNotReceived, routeReasonEnded)}, "Sent to A. B already finished, so it didn't get this."},
		{"a review", []AskRouteTarget{missed("A", routeNotDelivered, routeReasonReview)}, "A is waiting on a review that doesn't take messages. Open it to decide."},
		{"the guidance limit", []AskRouteTarget{missed("A", routeNotDelivered, routeReasonLimit)}, "A can't take more messages."},
		{"a refusal", []AskRouteTarget{missed("A", routeNotDelivered, routeReasonRefused)}, "A couldn't take this message."},
		{"every delivered status reads Sent", []AskRouteTarget{sent("A", routeAnswered), sent("B", routeNotApproved), sent("C", routeSaved), sent("D", routeReceived), missed("E", routeNotDelivered, routeReasonRefused)},
			"Sent to A. Sent to B. Sent to C. Sent to D. E couldn't take this message."},
	} {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, honestRouteReply(model, c.targets))
		})
	}
}

// When a confirmed stop does not go through, the reply cannot claim it did:
// it says where the message went and how each stop went.
func TestHonestStopReply(t *testing.T) {
	weather := askRouteGoal{Handle: "g1", GoalID: "weather-goal", RunID: "weather", Title: "Weather in Tucson"}
	report := askRouteGoal{Handle: "g2", GoalID: "report-goal", RunID: "report", Title: "Tucson report"}
	targets := []AskRouteTarget{{GoalID: "hike-goal", RunID: "hike", Title: "Hike plan", Status: routeQueued, Text: "Skip the report."}}
	require.Equal(t, "Sent to Hike plan. Stopping Tucson report. Weather in Tucson couldn't be stopped. Use Stop on its card.",
		honestStopReply(targets, []askRouteGoal{report}, []askRouteGoal{weather}))
	require.Equal(t, "Weather in Tucson couldn't be stopped. Use Stop on its card.", honestStopReply(nil, nil, []askRouteGoal{weather}))
}

// Only a new message of at most 4,000 characters (the guidance limit) and no
// attachments (guidance carries text only) is routed. The limit counts
// characters, not bytes.
func TestAskRouteEligible(t *testing.T) {
	require.True(t, askRouteEligible("Actually, I'm in Phoenix.", nil))
	require.True(t, askRouteEligible(strings.Repeat("é", 4000), nil), "4,000 characters is within the limit")
	require.False(t, askRouteEligible(strings.Repeat("é", 4001), nil))
	require.False(t, askRouteEligible("Read this.", []AskAttachment{{ArtifactID: "notes", Name: "notes.md"}}))
}

// A message that answers routing's own question delivers both of the person's
// messages, word for word; the earlier one alone when together they would
// pass the guidance limit.
func TestAskRouteResolvedText(t *testing.T) {
	require.Equal(t, "Make it shorter\n\nThe report", askRouteResolvedText("Make it shorter", "The report"))
	// 3,988 + 2 for the paragraph break + 10 for "The report".
	earlier := strings.Repeat("é", 3988)
	require.True(t, askRouteResolvedText(earlier, "The report") == earlier+"\n\nThe report", "exactly 4,000 characters together")
	earlier += "é"
	require.True(t, askRouteResolvedText(earlier, "The report") == earlier, "4,001 characters together")
}

// Routing is shown the goals whose stop question still waits for the person:
// any earlier stop question about the goal's run that nobody answered.
func TestAskPendingStops(t *testing.T) {
	goals := []askRouteGoal{{Handle: "g1", RunID: "weather"}, {Handle: "g2", RunID: "report"}, {Handle: "g3", RunID: "hike"}}
	turns := []AskTurn{
		{ID: "asked", Routed: &AskRouting{Stops: []AskStopQuestion{{GoalID: "weather-goal", RunID: "v1:work:run:weather"}}}},
		{ID: "kept", Routed: &AskRouting{Stops: []AskStopQuestion{{GoalID: "report-goal", RunID: "report", Decision: stopDecisionKept}}}},
		{ID: "elsewhere", Routed: &AskRouting{Stops: []AskStopQuestion{{GoalID: "trail-goal", RunID: "trail"}}}},
		{ID: "plain", Prompt: "Plan a hike.", RunID: "hike"},
	}
	pending, handles := askPendingStops(turns, goals)
	require.Equal(t, map[string]bool{"g1": true}, pending)
	require.Equal(t, []string{"g1"}, handles)
	pending, handles = askPendingStops(nil, goals)
	require.Empty(t, pending)
	require.NotNil(t, handles, "the prompt takes a list, never null")
}

// Routing's own question is pending only while it is the person's previous
// message and nothing settled it. A projected answer turn is not something the
// person said.
func TestAskPendingClarification(t *testing.T) {
	question := AskTurn{ID: "question", Prompt: "Make it shorter", Answer: "The weather check or the report?", State: "done", Routed: &AskRouting{Clarify: true}}
	require.Equal(t, "question", askPendingClarification([]AskTurn{{ID: "earlier", Prompt: "Hi"}, question}).ID)
	require.Equal(t, "question", askPendingClarification([]AskTurn{question, {ID: "guidance-a1", Prompt: "Guidance sent:\nUse Celsius.", AnswerOnly: true}}).ID)
	settled := question
	settled.Routed = &AskRouting{Clarify: true, Resolved: true}
	require.Nil(t, askPendingClarification([]AskTurn{settled}))
	require.Nil(t, askPendingClarification([]AskTurn{question, {ID: "later", Prompt: "Something else"}}), "a later message moved on")
	require.Nil(t, askPendingClarification([]AskTurn{{ID: "plain", Prompt: "Make it shorter", Answer: "Done."}}))
	require.Nil(t, askPendingClarification(nil))
}

// A goal opened by a split message asked for what intake received, not for
// the whole message.
func TestAskTurnRequest(t *testing.T) {
	require.Equal(t, "Plan a hike.", askTurnRequest(AskTurn{Prompt: "Plan a hike."}))
	split := AskTurn{Prompt: "Actually Phoenix. Also, what time is it there?", Routed: &AskRouting{Statement: "What time is it in Phoenix right now?"}}
	require.Equal(t, "What time is it in Phoenix right now?", askTurnRequest(split))
	require.Equal(t, "Make it shorter", askTurnRequest(AskTurn{Prompt: "Make it shorter", Routed: &AskRouting{Clarify: true}}))
}
