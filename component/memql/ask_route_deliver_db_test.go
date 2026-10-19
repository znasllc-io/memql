package memql

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// parkedGoal opens a root run of goal in conversation and parks it on one
// pending request, the way the work spine does: the request first, then the
// run's wait on it.
func parkedGoal(write func(string, map[string]any), conversation, goal, title, kind string, subject map[string]any, options []any) (run, approval string) {
	request := map[string]any{"kind": kind, "subject": subject, "artifactHash": "request"}
	if options != nil {
		request["options"] = options
	}
	return parkedGoalOn(write, conversation, goal, title, request)
}

// parkedGoalOn is parkedGoal with the request's fields set by the caller: its
// kind, subject and artifact hash, and anything else createWorkApproval takes.
func parkedGoalOn(write func(string, map[string]any), conversation, goal, title string, request map[string]any) (run, approval string) {
	run = guidanceRootRun(write, conversation, goal, goal+"-plan", "running")
	approval = id.NewShortId()
	row := map[string]any{"approvalId": approval, "runId": run, "stepKey": "plan", "question": "Which trail?", "requestedAt": time.Now().UTC().Format(time.RFC3339Nano)}
	for key, value := range request {
		row[key] = value
	}
	write("createWorkApproval", row)
	wait := map[string]any{"runId": run, "status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": approval, "approvalKind": request["kind"]}}
	if title != "" {
		wait["title"] = title
	}
	write("updateWorkRun", wait)
	return run, approval
}

type decideApprovalCall struct {
	caller string
	inRun  bool
	args   map[string]any
}

// stubDecideApproval records every work.decideApproval call on e -- who made
// it and with what -- and answers with reply, as the work integration does.
// A nil row answers nothing at all.
func stubDecideApproval(e *MemQLEngine, reply func(args map[string]any) (map[string]any, error)) *[]decideApprovalCall {
	calls := &[]decideApprovalCall{}
	e.builtinExecutorHandlers["integration.work.decideApproval"] = func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		access, _ := auth.AccessFromContext(ctx)
		run, inRun := common.RunFromContext(ctx)
		call := decideApprovalCall{args: args, inRun: inRun && run.RunId != ""}
		if access != nil {
			call.caller = access.UserId
		}
		*calls = append(*calls, call)
		row, err := reply(args)
		if err != nil || row == nil {
			return nil, err
		}
		return askValue(row)
	}
	return calls
}

// resumedReply is the work integration's answer when a decision is recorded
// and the run parked on it resumes.
func resumedReply(run string) func(map[string]any) (map[string]any, error) {
	return func(args map[string]any) (map[string]any, error) {
		return map[string]any{"approvalId": args["approvalId"], "runId": run, "decision": "answered", "runResumed": true}, nil
	}
}

// guidanceAnswer is exactly what a routed message sends a request: guidance,
// never an approval, a permission choice or a limit.
func guidanceAnswer(approval, text string) map[string]any {
	return map[string]any{"approvalId": approval, "decision": "answered", "answer": map[string]any{"feedback": text}}
}

// listGoals lists the active goals of an in-memory transcript, as routing does.
func listGoals(t *testing.T, e *MemQLEngine, ctx context.Context, turns ...AskTurn) []askRouteGoal {
	t.Helper()
	goals, err := e.activeAskGoals(ctx, askTranscript{Turns: turns})
	require.NoError(t, err)
	return goals
}

type goalListing struct {
	Handle, TurnID, GoalID, RunID, Title, Request, Status, Approval string
	Updates                                                         []string
}

func goalListings(goals []askRouteGoal) []goalListing {
	out := make([]goalListing, len(goals))
	for n, goal := range goals {
		out[n] = goalListing{goal.Handle, goal.TurnID, goal.GoalID, goal.RunID, goal.Title, goal.Request, stringArg(goal.Run, "status"), BareShortId(stringArg(goal.Approval, "id")), goal.Updates}
	}
	return out
}

// Routing sees one goal per root run still preparing, working or waiting,
// newest first; a finished, cancelled or stopping run and a delegated child
// are left out. A goal's title is its run's, else its turn's work title, else
// its prompt; its request is the prompt that opened it; its updates are the
// last three messages already delivered to it. A goal parked on a pending
// request carries that request, and one whose request was decided carries none.
func TestActiveAskGoalsListsOnlyActiveRootRuns(t *testing.T) {
	a, _, ctx, conversation, weather, write := guidanceFixture(t)
	actor, _ := auth.AccessFromContext(ctx)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	write("updateWorkRun", map[string]any{"runId": weather, "classification": map[string]any{"workTitle": "Weather in Tucson"}})
	finished := guidanceRootRun(write, conversation, "trail-goal", "trail-plan", "succeeded")
	cancelled := guidanceRootRun(write, conversation, "camp-goal", "camp-plan", "cancelled")
	hike, question := parkedGoal(write, conversation, "hike-goal", "Hike near Tucson", "feedback", map[string]any{"kind": "text"}, nil)
	advice := id.NewShortId()
	write("createWorkRun", map[string]any{"runId": advice, "goalId": "hike-goal", "parentRunId": hike, "automationName": ApprovalDiscussionTemplate, "templateFingerprint": "advice", "triggeredBy": "approval-discussion", "status": "running", "mode": "live", "startedAt": now})
	stopping := guidanceRootRun(write, conversation, "map-goal", "map-plan", "running")
	write("updateWorkRun", map[string]any{"runId": stopping, "cancelRequested": true})
	gear := guidanceRootRun(write, conversation, "gear-goal", "gear-plan", "running")
	write("updateWorkRun", map[string]any{"runId": gear, "status": "waiting", "waitingOn": map[string]any{"kind": "retry", "subject": "section"}})
	packing, answered := parkedGoal(write, conversation, "packing-goal", "", "feedback", map[string]any{"kind": "text"}, nil)
	write("decideWorkApproval", map[string]any{"approvalId": answered, "decision": "answered", "decidedBy": actor.UserId, "decidedAt": now, "answer": map[string]any{"text": "Two days."}})
	report := guidanceRootRun(write, conversation, "report-goal", "", "compiling")

	routed := func(turnID string, targets ...AskRouteTarget) AskTurn {
		return AskTurn{ID: turnID, Prompt: "A routed message.", Routed: &AskRouting{Targets: targets}}
	}
	turns := []AskTurn{
		{ID: "weather", RunID: weather, Prompt: "What's the weather in Tucson this weekend?"},
		{ID: "trail", RunID: finished, Prompt: "Find a trail."},
		{ID: "camp", RunID: cancelled, Prompt: "Book a campsite."},
		{ID: "hike", RunID: hike, Prompt: strings.Repeat("ü", 700), WorkTitle: "Hike"},
		{ID: "why", RunID: advice, ApprovalID: question, Advisor: true, Prompt: "Why this trail?"},
		{ID: "map", RunID: stopping, Prompt: "Print a map."},
		{ID: "gear", RunID: gear, Prompt: strings.Repeat("é", 100)},
		{ID: "packing", RunID: packing, Prompt: "Make a packing list.", WorkTitle: "Packing list"},
		{ID: "report", RunID: report, Prompt: "  Write the Tucson report.  "},
		routed("phoenix", AskRouteTarget{GoalID: "fixture-goal", RunID: weather, Status: routeQueued, Text: "Actually Phoenix."}),
		routed("public", AskRouteTarget{GoalID: "fixture-goal", RunID: weather, Status: routeNotDelivered, Reason: routeReasonLimit, Text: "Over the cap."},
			AskRouteTarget{GoalID: "hike-goal", RunID: hike, Status: routeNotApproved, ApprovalID: "permission", Text: "Only public trails."}),
		routed("celsius", AskRouteTarget{GoalID: "fixture-goal", RunID: weather, Status: routeAnswered, ApprovalID: "unit", Text: "Use Celsius."}),
		routed("detail", AskRouteTarget{GoalID: "fixture-goal", RunID: weather, Status: routeReceived, Text: strings.Repeat("ö", 400)}),
		routed("late", AskRouteTarget{GoalID: "fixture-goal", RunID: weather, Status: routeNotReceived, Reason: routeReasonEnded, Text: "Too late."}),
		routed("budget", AskRouteTarget{GoalID: "fixture-goal", RunID: "v1:work:run:" + weather, Status: routeSaved, ApprovalID: "budget", Text: "Keep it under twenty dollars."}),
		{ID: "tomorrow", RunID: weather, Prompt: "And tomorrow?"},
	}
	goals := listGoals(t, a, ctx, turns...)
	require.Equal(t, []goalListing{
		{"g1", "weather", "fixture-goal", weather, "Weather in Tucson", "What's the weather in Tucson this weekend?", "running", "", []string{"Use Celsius.", strings.Repeat("ö", 299) + "…", "Keep it under twenty dollars."}},
		{"g2", "report", "report-goal", report, "Write the Tucson report.", "Write the Tucson report.", "compiling", "", nil},
		{"g3", "packing", "packing-goal", packing, "Packing list", "Make a packing list.", "waiting", "", nil},
		{"g4", "gear", "gear-goal", gear, strings.Repeat("é", 79) + "…", strings.Repeat("é", 100), "waiting", "", nil},
		{"g5", "hike", "hike-goal", hike, "Hike near Tucson", strings.Repeat("ü", 599) + "…", "waiting", question, []string{"Only public trails."}},
	}, goalListings(goals))
	require.Equal(t, "feedback", goals[4].Approval["kind"], "a goal parked on your question carries that request")
	require.Equal(t, "Which trail?", goals[4].Approval["question"])
}

// One message is checked against at most twelve goals, the newest.
func TestActiveAskGoalsKeepsTheTwelveNewest(t *testing.T) {
	a, _, ctx, conversation, oldest, write := guidanceFixture(t)
	turns := []AskTurn{{ID: "turn-0", RunID: oldest, Prompt: "Message 0"}}
	runs := []string{oldest}
	for n := 1; n <= 12; n++ {
		run := guidanceRootRun(write, conversation, fmt.Sprintf("goal-%d", n), "plan", "running")
		runs = append(runs, run)
		turns = append(turns, AskTurn{ID: fmt.Sprintf("turn-%d", n), RunID: run, Prompt: fmt.Sprintf("Message %d", n)})
	}
	goals := listGoals(t, a, ctx, turns...)
	require.Len(t, goals, 12)
	for n, goal := range goals {
		require.Equal(t, fmt.Sprintf("g%d", n+1), goal.Handle)
		require.Equal(t, runs[12-n], goal.RunID, "newest first")
	}
}

// A goal parked on the person's question, or on a failure question, takes the
// message as their answer, sent as the person with the exact text.
func TestDeliverApprovalTargetsAnswersAQuestion(t *testing.T) {
	failure := []any{map[string]any{"label": "Revise plan", "value": "retry"}, map[string]any{"label": "Abandon", "value": "abandon"}}
	for _, c := range []struct {
		name, kind string
		subject    map[string]any
		options    []any
	}{
		{"your question", "feedback", map[string]any{"kind": "text"}, nil},
		{"a failure question", "planReview", map[string]any{"symptom": "plan", "stepKey": "plan", "errorMessage": "No trail data."}, failure},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, _, ctx, conversation, _, write := guidanceFixture(t)
			actor, _ := auth.AccessFromContext(ctx)
			hike, question := parkedGoal(write, conversation, "hike-goal", "Hike near Tucson", c.kind, c.subject, c.options)
			goals := listGoals(t, a, ctx, AskTurn{ID: "hike", RunID: hike, Prompt: "Plan a hike near Tucson."})
			calls := stubDecideApproval(a, resumedReply(hike))
			delivered, rest := a.deliverApprovalTargets(ctx, goals, "Take the Picacho Peak trail.")
			require.Empty(t, rest)
			require.Equal(t, []decideApprovalCall{{caller: actor.UserId, args: guidanceAnswer(question, "Take the Picacho Peak trail.")}}, *calls)
			require.Equal(t, []AskRouteTarget{{GoalID: "hike-goal", RunID: hike, Title: "Hike near Tucson", Status: routeAnswered, ApprovalID: question, Text: "Take the Picacho Peak trail."}}, delivered)
		})
	}
}

// A permission request takes the message as a correction: the goal
// reconsiders and nothing is granted.
func TestDeliverApprovalTargetsCorrectsAPermissionWithoutAGrant(t *testing.T) {
	for _, c := range []struct {
		name, kind string
		subject    map[string]any
	}{
		{"a permission to act", "sideEffect", map[string]any{"command": "post the trip report"}},
		{"computer access", "scopeElevation", map[string]any{"agentId": "agent"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, _, ctx, conversation, _, write := guidanceFixture(t)
			actor, _ := auth.AccessFromContext(ctx)
			hike, permission := parkedGoal(write, conversation, "hike-goal", "Hike near Tucson", c.kind, c.subject, nil)
			goals := listGoals(t, a, ctx, AskTurn{ID: "hike", RunID: hike, Prompt: "Plan a hike near Tucson."})
			calls := stubDecideApproval(a, resumedReply(hike))
			delivered, rest := a.deliverApprovalTargets(ctx, goals, "Don't post anything; just tell me.")
			require.Empty(t, rest)
			require.Equal(t, []decideApprovalCall{{caller: actor.UserId, args: guidanceAnswer(permission, "Don't post anything; just tell me.")}}, *calls, "answered with guidance, never approved")
			require.Equal(t, []AskRouteTarget{{GoalID: "hike-goal", RunID: hike, Title: "Hike near Tucson", Status: routeNotApproved, ApprovalID: permission, Text: "Don't post anything; just tell me."}}, delivered)
		})
	}
}

// A native computer-access request expires after an hour, yet still takes a
// correction: the message goes to it as guidance, never a grant, and the goal
// reconsiders. An expired question takes no answer that way, so its goal is
// left to the locked phase.
func TestDeliverApprovalTargetsCorrectsAnExpiredComputerAccessRequest(t *testing.T) {
	a, _, ctx, conversation, _, write := guidanceFixture(t)
	actor, _ := auth.AccessFromContext(ctx)
	expired := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	scope := map[string]any{"agentId": "agent", "scope": "observe", "summary": "Read the Downloads folder?",
		"nativeScopeCheckpoint": map[string]any{"version": 1, "continuationId": "continuation", "toolCallId": "call", "requestHash": "request"}}
	files, access := parkedGoalOn(write, conversation, "files-goal", "Tidy the downloads", map[string]any{"kind": "scopeElevation", "subject": scope, "artifactHash": work.ArtifactHash(scope), "expiresAt": expired})
	trail, _ := parkedGoalOn(write, conversation, "trail-goal", "Trail report", map[string]any{"kind": "feedback", "subject": map[string]any{"kind": "text"}, "artifactHash": "question", "expiresAt": expired})
	goals := listGoals(t, a, ctx, AskTurn{ID: "trail", RunID: trail, Prompt: "Write a trail report."}, AskTurn{ID: "files", RunID: files, Prompt: "Tidy my downloads."})
	require.Equal(t, []string{files, trail}, []string{goals[0].RunID, goals[1].RunID})
	require.Equal(t, access, BareShortId(stringArg(goals[0].Approval, "id")), "the expired computer-access request still takes the message")
	require.Nil(t, goals[1].Approval, "an expired question does not")
	calls := stubDecideApproval(a, resumedReply(files))
	delivered, rest := a.deliverApprovalTargets(ctx, goals, "Only look at the PDFs.")
	require.Equal(t, []decideApprovalCall{{caller: actor.UserId, args: guidanceAnswer(access, "Only look at the PDFs.")}}, *calls)
	require.Equal(t, []AskRouteTarget{{GoalID: "files-goal", RunID: files, Title: "Tidy the downloads", Status: routeNotApproved, ApprovalID: access, Text: "Only look at the PDFs."}}, delivered)
	require.Equal(t, goals[1:], rest)
}

// A budget pause saves the message for when the limit is raised. The work
// integration's answer is what says it was saved.
func TestDeliverApprovalTargetsSavesBudgetGuidance(t *testing.T) {
	a, _, ctx, conversation, _, write := guidanceFixture(t)
	actor, _ := auth.AccessFromContext(ctx)
	hike, budget := parkedGoal(write, conversation, "hike-goal", "Hike near Tucson", "budget", map[string]any{"resumeKind": "checkpoint", "continuationId": "continuation"}, nil)
	trail, question := parkedGoal(write, conversation, "trail-goal", "Trail report", "feedback", map[string]any{"kind": "text"}, nil)
	goals := listGoals(t, a, ctx, AskTurn{ID: "hike", RunID: hike, Prompt: "Plan a hike."}, AskTurn{ID: "trail", RunID: trail, Prompt: "Write a trail report."})
	runs := map[string]string{budget: hike, question: trail}
	calls := stubDecideApproval(a, func(args map[string]any) (map[string]any, error) {
		approval := BareShortId(fmt.Sprint(args["approvalId"]))
		return map[string]any{"approvalId": approval, "runId": runs[approval], "decision": "", "runResumed": false, "guidanceSaved": true, "savedGuidance": "Keep it cheap."}, nil
	})
	delivered, rest := a.deliverApprovalTargets(ctx, goals, "Keep it cheap.")
	require.Empty(t, rest)
	require.Equal(t, []decideApprovalCall{
		{caller: actor.UserId, args: guidanceAnswer(question, "Keep it cheap.")},
		{caller: actor.UserId, args: guidanceAnswer(budget, "Keep it cheap.")},
	}, *calls)
	require.Equal(t, []AskRouteTarget{
		{GoalID: "trail-goal", RunID: trail, Title: "Trail report", Status: routeSaved, ApprovalID: question, Text: "Keep it cheap."},
		{GoalID: "hike-goal", RunID: hike, Title: "Hike near Tucson", Status: routeSaved, ApprovalID: budget, Text: "Keep it cheap."},
	}, delivered, "whatever the request, a saved message reads Saved")
}

// A request that refuses the message, or answers nothing, leaves it Not
// delivered (refused) on that goal; the others are still answered.
func TestDeliverApprovalTargetsRecordsARefusal(t *testing.T) {
	a, _, ctx, conversation, _, write := guidanceFixture(t)
	hike, refusing := parkedGoal(write, conversation, "hike-goal", "Hike near Tucson", "feedback", map[string]any{"kind": "text"}, nil)
	camp, silent := parkedGoal(write, conversation, "camp-goal", "Campsite", "feedback", map[string]any{"kind": "text"}, nil)
	trail, question := parkedGoal(write, conversation, "trail-goal", "Trail report", "feedback", map[string]any{"kind": "text"}, nil)
	goals := listGoals(t, a, ctx, AskTurn{ID: "hike", RunID: hike, Prompt: "Plan a hike."}, AskTurn{ID: "camp", RunID: camp, Prompt: "Book a campsite."}, AskTurn{ID: "trail", RunID: trail, Prompt: "Write a trail report."})
	calls := stubDecideApproval(a, func(args map[string]any) (map[string]any, error) {
		switch BareShortId(fmt.Sprint(args["approvalId"])) {
		case refusing:
			return nil, fmt.Errorf("work: this run is no longer waiting on the request being discussed")
		case silent:
			return nil, nil
		}
		return map[string]any{"approvalId": args["approvalId"], "runId": trail, "decision": "answered", "runResumed": true}, nil
	})
	delivered, rest := a.deliverApprovalTargets(ctx, goals, "Take the shorter one.")
	require.Empty(t, rest)
	require.Len(t, *calls, 3)
	require.Equal(t, []AskRouteTarget{
		{GoalID: "trail-goal", RunID: trail, Title: "Trail report", Status: routeAnswered, ApprovalID: question, Text: "Take the shorter one."},
		{GoalID: "camp-goal", RunID: camp, Title: "Campsite", Status: routeNotDelivered, Reason: routeReasonRefused, ApprovalID: silent, Text: "Take the shorter one."},
		{GoalID: "hike-goal", RunID: hike, Title: "Hike near Tucson", Status: routeNotDelivered, Reason: routeReasonRefused, ApprovalID: refusing, Text: "Take the shorter one."},
	}, delivered)
}

// The request phase answers only a request the goal still waits on, read
// fresh just before answering it. A goal whose question was decided elsewhere
// or that was asked to stop since it was listed, one whose request can no
// longer be read, and a goal that never waited on an answerable request, go
// unchanged to the locked phase.
func TestDeliverApprovalTargetsLeavesTheRestToTheLockedPhase(t *testing.T) {
	a, _, ctx, conversation, weather, write := guidanceFixture(t)
	actor, _ := auth.AccessFromContext(ctx)
	hike, answered := parkedGoal(write, conversation, "hike-goal", "Hike near Tucson", "feedback", map[string]any{"kind": "text"}, nil)
	trail, _ := parkedGoal(write, conversation, "trail-goal", "Trail report", "sideEffect", map[string]any{"command": "post the report"}, nil)
	review, _ := parkedGoal(write, conversation, "plan-goal", "Trip plan", "planReview", map[string]any{"plan": "draft"}, nil)
	camp, _ := parkedGoal(write, conversation, "camp-goal", "Campsite", "feedback", map[string]any{"kind": "text"}, nil)
	goals := listGoals(t, a, ctx,
		AskTurn{ID: "weather", RunID: weather, Prompt: "What's the weather?"},
		AskTurn{ID: "hike", RunID: hike, Prompt: "Plan a hike."},
		AskTurn{ID: "trail", RunID: trail, Prompt: "Write a trail report."},
		AskTurn{ID: "plan", RunID: review, Prompt: "Plan the trip."},
		AskTurn{ID: "camp", RunID: camp, Prompt: "Book a campsite."},
	)
	require.Len(t, goals, 5)
	// After listing: the hike question is answered in Nexus, and the person
	// asks the trail report to stop. Both runs are still parked. The campsite
	// goal names a request no read can find.
	write("decideWorkApproval", map[string]any{"approvalId": answered, "decision": "answered", "decidedBy": actor.UserId, "decidedAt": time.Now().UTC().Format(time.RFC3339Nano), "answer": map[string]any{"text": "The short loop."}})
	write("updateWorkRun", map[string]any{"runId": trail, "cancelRequested": true})
	require.Equal(t, camp, goals[0].RunID)
	unreadable := id.NewShortId()
	goals[0].Approval = map[string]any{"id": unreadable, "kind": "feedback", "subject": map[string]any{"kind": "text"}, "decision": ""}
	goals[0].Run = map[string]any{"id": camp, "goalId": "camp-goal", "status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": unreadable, "approvalKind": "feedback"}}
	kind, _, _ := routeDeliveryFor(goals[0].Run, goals[0].Approval)
	require.Equal(t, deliverApproval, kind, "as listed, the campsite goal waits on an answerable request")
	calls := stubDecideApproval(a, resumedReply(""))
	delivered, rest := a.deliverApprovalTargets(ctx, goals, "Actually make it Phoenix.")
	require.Empty(t, *calls, "nothing is answered")
	require.Empty(t, delivered)
	require.Equal(t, goals, rest)
}

// The Review Focus case end to end over the stored conversation: a goal at its
// guidance cap gets Not delivered (limit), and once that is recorded the
// agent's read on another replica still succeeds with what it held before.
func TestDeliverRouteAtTheCapLeavesTheAgentsReadWorking(t *testing.T) {
	a, b, ctx, conversation, run, _ := guidanceFixture(t)
	for n := range 16 {
		routeTurn(t, a, ctx, conversation, AskTurn{ID: fmt.Sprintf("note-%d", n), Prompt: "Note.", Routed: &AskRouting{Targets: []AskRouteTarget{
			{GoalID: "fixture-goal", RunID: run, Status: routeQueued, Text: "Note."},
		}}})
	}
	runtime := guidanceRunContext(ctx, run)
	before, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Len(t, before, 16)

	row, err := a.askRead(ContextWithFreshRead(ctx), conversation)
	require.NoError(t, err)
	transcript, err := approvalTranscript(row)
	require.NoError(t, err)
	goals, err := a.activeAskGoals(ctx, transcript)
	require.NoError(t, err)
	require.Len(t, goals, 1)
	delivered, rest := a.deliverApprovalTargets(ctx, goals, "Use Celsius.")
	require.Empty(t, delivered)
	runs, err := a.workRows(ContextWithFreshRead(ctx), "workRunForOwner", run)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	targets := guidanceTargetsLocked(conversation, transcript, rest, map[string]map[string]any{BareShortId(run): runs[0]}, "Use Celsius.")
	require.Equal(t, []AskRouteTarget{{GoalID: "fixture-goal", RunID: BareShortId(run), Title: "Synthetic work", Status: routeNotDelivered, Reason: routeReasonLimit, Text: "Use Celsius."}}, targets)
	routeTurn(t, a, ctx, conversation, AskTurn{ID: "celsius", Prompt: "Use Celsius.", Routed: &AskRouting{Targets: targets}})

	after, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
