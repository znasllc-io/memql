package memql

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

func guidanceFixture(t *testing.T) (*MemQLEngine, *MemQLEngine, context.Context, string, string, func(string, map[string]any)) {
	t.Helper()
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	ctx := askTestActor()
	conversation := askTestConversation(t, a, ctx)
	runID := id.NewShortId()
	write := func(name string, args map[string]any) {
		t.Helper()
		call, err := parser.RenderCall(name, args)
		require.NoError(t, err)
		_, err = a.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
		require.NoError(t, err)
	}
	write("createWorkRun", map[string]any{"runId": runID, "goalId": "fixture-goal", "automationName": "fixture", "templateFingerprint": "original-plan", "triggeredBy": "manual", "status": "running", "mode": "live", "input": map[string]any{"conversation": map[string]any{"id": conversation}}, "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	require.NoError(t, a.askSave(ctx, conversation, "Guidance fixture", askTranscript{Turns: []AskTurn{{ID: "original", RunID: runID, GoalID: "fixture-goal", Prompt: "Synthetic work", State: "queued", Background: true, Activity: []WorkEvent{}}}}))
	return a, b, ctx, conversation, runID, write
}

func guidanceRunContext(ctx context.Context, run string) context.Context {
	ac, _ := auth.AccessFromContext(ctx)
	return common.ContextWithRun(ctx, common.RunContext{RunId: run, GoalId: "fixture-goal", OwnerUserId: ac.UserId, StepKey: "section"})
}

// guidanceRootRun opens another root run of a goal in a conversation.
func guidanceRootRun(write func(string, map[string]any), conversation, goal, plan, status string) string {
	run := id.NewShortId()
	write("createWorkRun", map[string]any{"runId": run, "goalId": goal, "automationName": "fixture", "templateFingerprint": plan, "triggeredBy": "manual", "status": status, "mode": "live", "input": map[string]any{"conversation": map[string]any{"id": conversation}}, "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	return run
}

// routeTurn records a routed message the way delivery does: a turn with no
// goal of its own whose targets and stop questions name the goals it reached.
func routeTurn(t *testing.T, e *MemQLEngine, ctx context.Context, conversation string, turn AskTurn) {
	t.Helper()
	row, err := e.askRead(ContextWithFreshRead(ctx), conversation)
	require.NoError(t, err)
	transcript, err := approvalTranscript(row)
	require.NoError(t, err)
	turn.State, turn.StartedAt, turn.Activity = "done", time.Now().UTC(), []WorkEvent{}
	transcript.Turns = append(transcript.Turns, turn)
	require.NoError(t, e.askSave(ctx, conversation, stringArg(row, "title"), transcript))
}

// storedTurn reads one turn as it is saved, with no snapshot projection.
func storedTurn(t *testing.T, e *MemQLEngine, ctx context.Context, conversation, turnID string) AskTurn {
	t.Helper()
	row, err := e.askRead(ContextWithFreshRead(ctx), conversation)
	require.NoError(t, err)
	transcript, err := approvalTranscript(row)
	require.NoError(t, err)
	for _, turn := range transcript.Turns {
		if turn.ID == turnID {
			return turn
		}
	}
	t.Fatalf("turn %q is not in the transcript", turnID)
	return AskTurn{}
}

// The executing replica holds none of the recording replica's state. It reads
// the routed turn's target for its own root run, under the turn's id.
func TestReadWorkGuidanceReadsRoutedTargets(t *testing.T) {
	a, b, ctx, conversation, run, write := guidanceFixture(t)
	runtime := guidanceRunContext(ctx, run)
	// Warm the receiving peer before the independent human write.
	before, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Empty(t, before)
	// A settled clarification: the goal receives exactly what was delivered,
	// both of the person's messages, not the last prompt alone.
	text := "Actually, I'm in Phoenix.\n\nThe weather one."
	routeTurn(t, a, ctx, conversation, AskTurn{ID: "phoenix", Prompt: "The weather one.", Routed: &AskRouting{
		Targets: []AskRouteTarget{{GoalID: "fixture-goal", RunID: run, Title: "Weather in Tucson", Status: routeQueued, Text: text}},
		Reply:   "Switching the weather check to Phoenix.",
	}})
	messages, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Equal(t, []WorkGuidance{{ID: "phoenix", Prompt: text}}, messages)
	// A delegated child on the other replica reads its root's guidance.
	child := id.NewShortId()
	write("createWorkRun", map[string]any{"runId": child, "goalId": "fixture-goal", "parentRunId": run, "input": map[string]any{"conversation": map[string]any{"id": conversation}}, "automationName": "child", "templateFingerprint": "child-plan", "triggeredBy": "automation", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	recovered, err := a.ReadWorkGuidance(guidanceRunContext(ctx, child))
	require.NoError(t, err)
	require.Equal(t, messages, recovered)
	// Guidance is read only under the run's own owner and goal.
	actor, _ := auth.AccessFromContext(ctx)
	forged := common.ContextWithRun(ctx, common.RunContext{RunId: run, GoalId: "another-goal", OwnerUserId: actor.UserId, StepKey: "section"})
	_, err = b.ReadWorkGuidance(forged)
	require.ErrorContains(t, err, "owner or goal")
}

// A message delivered through a request (decideApproval) belongs to that
// request and never reaches the agent loop a second time. Neither does one the
// cap refused, one whose run had ended, or another goal's.
func TestReadWorkGuidanceSkipsApprovalTargetsAndOtherGoals(t *testing.T) {
	a, b, ctx, conversation, run, write := guidanceFixture(t)
	other := guidanceRootRun(write, conversation, "other-goal", "other-plan", "running")
	for _, turn := range []AskTurn{
		{ID: "answered", Prompt: "Use Phoenix.", Routed: &AskRouting{Targets: []AskRouteTarget{{GoalID: "fixture-goal", RunID: run, Status: routeAnswered, ApprovalID: "question", Text: "Use Phoenix."}}}},
		// The approval id alone excludes it, whatever its status reads.
		{ID: "through-a-request", Prompt: "Not this way.", Routed: &AskRouting{Targets: []AskRouteTarget{{GoalID: "fixture-goal", RunID: run, Status: routeQueued, ApprovalID: "permission", Text: "Not this way."}}}},
		{ID: "capped", Prompt: "Over the cap.", Routed: &AskRouting{Targets: []AskRouteTarget{{GoalID: "fixture-goal", RunID: run, Status: routeNotDelivered, Reason: routeReasonLimit, Text: "Over the cap."}}}},
		{ID: "too-late", Prompt: "Too late.", Routed: &AskRouting{Targets: []AskRouteTarget{{GoalID: "fixture-goal", RunID: run, Status: routeNotReceived, Reason: routeReasonEnded, Text: "Too late."}}}},
		{ID: "both-goals", Prompt: "Both of them.", Routed: &AskRouting{Targets: []AskRouteTarget{
			{GoalID: "other-goal", RunID: other, Status: routeQueued, Text: "Both of them."},
			{GoalID: "fixture-goal", RunID: run, Status: routeQueued, Text: "Both of them."},
		}}},
	} {
		routeTurn(t, a, ctx, conversation, turn)
	}
	messages, err := b.ReadWorkGuidance(guidanceRunContext(ctx, run))
	require.NoError(t, err)
	require.Equal(t, []WorkGuidance{{ID: "both-goals", Prompt: "Both of them."}}, messages)
	actor, _ := auth.AccessFromContext(ctx)
	elsewhere, err := b.ReadWorkGuidance(common.ContextWithRun(ctx, common.RunContext{RunId: other, GoalId: "other-goal", OwnerUserId: actor.UserId, StepKey: "section"}))
	require.NoError(t, err)
	require.Equal(t, []WorkGuidance{{ID: "both-goals", Prompt: "Both of them."}}, elsewhere, "one message reaches each goal it names")
}

// Acknowledgement is this run's receipt. Received is not a delivery cursor:
// a later loop of the same run still reads the person's direction.
func TestAcknowledgeWorkGuidanceMarksTargetsReceived(t *testing.T) {
	a, b, ctx, conversation, run, write := guidanceFixture(t)
	other := guidanceRootRun(write, conversation, "other-goal", "other-plan", "running")
	routeTurn(t, a, ctx, conversation, AskTurn{ID: "phoenix", Prompt: "Actually Phoenix.", Routed: &AskRouting{Targets: []AskRouteTarget{
		{GoalID: "other-goal", RunID: other, Status: routeQueued, Text: "Actually Phoenix."},
		{GoalID: "fixture-goal", RunID: run, Status: routeQueued, Text: "Actually Phoenix."},
	}}})
	runtime := guidanceRunContext(ctx, run)
	messages, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.NoError(t, b.AcknowledgeWorkGuidance(runtime, []string{messages[0].ID, "optional-answer-elsewhere"}))
	targets := storedTurn(t, a, ctx, conversation, "phoenix").Routed.Targets
	require.Equal(t, routeQueued, targets[0].Status, "another goal's delivery is not this run's receipt")
	require.Equal(t, routeReceived, targets[1].Status)
	again, err := a.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Equal(t, messages, again)
	// An answered permission resumes the same execution and keeps its guidance.
	write("updateWorkRun", map[string]any{"runId": run, "status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": "typed-scope"}})
	write("updateWorkRun", map[string]any{"runId": run, "status": "running", "humanResumeId": "typed-scope", "waitingOn": map[string]any{}})
	resumed, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Equal(t, messages, resumed)
}

// Routed guidance is about the goal, so it stays with the run for its whole
// life (2026-10-19 ruling). A message sent while the run waits on an automatic
// retry is read when the retry resumes it under a new rerun request, and what
// was already received survives that retry, a repair or re-plan with a new
// plan, and a person's re-run of a step.
func TestRoutedGuidanceLastsTheRunsLife(t *testing.T) {
	a, b, ctx, conversation, run, write := guidanceFixture(t)
	actor, _ := auth.AccessFromContext(ctx)
	// The executing loop carries the run's current rerun request, as every
	// executing replica restores it from the run row.
	execution := func(rerunRequest string) context.Context {
		return common.ContextWithRun(ctx, common.RunContext{RunId: run, RerunRequestId: rerunRequest, GoalId: "fixture-goal", OwnerUserId: actor.UserId, StepKey: "section"})
	}
	route := func(turnID, text string) {
		routeTurn(t, a, ctx, conversation, AskTurn{ID: turnID, Prompt: text, Routed: &AskRouting{Targets: []AskRouteTarget{
			{GoalID: "fixture-goal", RunID: run, Title: "Weather in Tucson", Status: routeQueued, Text: text},
		}}})
	}
	route("phoenix", "Actually Phoenix.")
	working, err := b.ReadWorkGuidance(execution(""))
	require.NoError(t, err)
	require.Equal(t, []WorkGuidance{{ID: "phoenix", Prompt: "Actually Phoenix."}}, working)
	require.NoError(t, b.AcknowledgeWorkGuidance(execution(""), []string{"phoenix"}))

	// A step fails and the run waits for an automatic retry; a message sent
	// meanwhile is read once the retry resumes the run.
	write("updateWorkRun", map[string]any{"runId": run, "status": "waiting", "waitingOn": map[string]any{"kind": "retry", "subject": "section"}})
	route("celsius", "Use Celsius.")
	write("updateWorkRun", map[string]any{"runId": run, "status": "running", "waitingOn": map[string]any{}, "rerun": map[string]any{"requestId": "retry-1", "reason": "rerun", "stepKey": "section"}})
	both := []WorkGuidance{{ID: "phoenix", Prompt: "Actually Phoenix."}, {ID: "celsius", Prompt: "Use Celsius."}}
	retried, err := b.ReadWorkGuidance(execution("retry-1"))
	require.NoError(t, err)
	require.Equal(t, both, retried, "a retry keeps what was received and delivers what waited")

	// A repair or re-plan gives the run a new plan; a person re-runs a step.
	write("updateWorkRun", map[string]any{"runId": run, "templateFingerprint": "replanned", "rerun": map[string]any{"requestId": "replan-1", "reason": "replan", "stepKey": "section"}})
	replanned, err := a.ReadWorkGuidance(execution("replan-1"))
	require.NoError(t, err)
	require.Equal(t, both, replanned)
	write("updateWorkRun", map[string]any{"runId": run, "rerun": map[string]any{"requestId": "person-1", "reason": "rerun", "stepKey": "section", "requestedBy": actor.UserId}})
	rerun, err := b.ReadWorkGuidance(execution("person-1"))
	require.NoError(t, err)
	require.Equal(t, both, rerun)
	require.NoError(t, b.AcknowledgeWorkGuidance(execution("person-1"), []string{"celsius"}))
	require.Equal(t, routeReceived, storedTurn(t, a, ctx, conversation, "celsius").Routed.Targets[0].Status)
	require.Equal(t, routeReceived, storedTurn(t, a, ctx, conversation, "phoenix").Routed.Targets[0].Status)
}

// The acknowledgement re-reads its scope under the conversation lock: a child
// moved to another conversation's run while it waited acknowledges nothing.
func TestAcknowledgeWorkGuidanceCannotCrossConversationWhileWaitingForLock(t *testing.T) {
	a, b, ctx, conversation, run, write := guidanceFixture(t)
	secondConversation := askTestConversation(t, a, ctx)
	secondRun := guidanceRootRun(write, secondConversation, "fixture-goal", "original-plan", "running")
	child := id.NewShortId()
	write("createWorkRun", map[string]any{"runId": child, "goalId": "fixture-goal", "parentRunId": run, "automationName": "fixture", "templateFingerprint": "original-plan", "triggeredBy": "manual", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	for _, target := range []struct{ run, conversation string }{{run, conversation}, {secondRun, secondConversation}} {
		routeTurn(t, a, ctx, target.conversation, AskTurn{ID: "same-key", Prompt: "Direction for this conversation.", Routed: &AskRouting{Targets: []AskRouteTarget{
			{GoalID: "fixture-goal", RunID: target.run, Status: routeQueued, Text: "Direction for this conversation."},
		}}})
	}
	runtime := guidanceRunContext(ctx, child)
	items, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Len(t, items, 1)
	hold, err := a.holdAskConversation(ctx, conversation, "write")
	require.NoError(t, err)
	var once sync.Once
	release := func() { once.Do(hold.release) }
	defer release()
	done := make(chan error, 1)
	go func() { done <- b.AcknowledgeWorkGuidance(runtime, []string{items[0].ID}) }()
	require.Eventually(t, func() bool {
		var waiting bool
		err := hold.tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted AND objsubid=1 AND classid::bigint=(($1::bigint >> 32) & 4294967295) AND objid::bigint=($1::bigint & 4294967295))`, hold.key).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond)
	write("updateWorkRun", map[string]any{"runId": child, "parentRunId": secondRun})
	release()
	require.NoError(t, <-done)
	for _, id := range []string{conversation, secondConversation} {
		require.Equal(t, routeQueued, storedTurn(t, a, ctx, id, "same-key").Routed.Targets[0].Status)
	}
}

// The recorder refuses past the cap. A transcript already over it fails loudly
// instead of silently dropping part of a person's direction.
func TestReadWorkGuidanceKeepsItsBounds(t *testing.T) {
	a, b, ctx, conversation, run, write := guidanceFixture(t)
	queue := func(run, turnID, text string) {
		routeTurn(t, a, ctx, conversation, AskTurn{ID: turnID, Prompt: text, Routed: &AskRouting{Targets: []AskRouteTarget{
			{GoalID: "fixture-goal", RunID: run, Status: routeQueued, Text: text},
		}}})
	}
	for n := range 4 {
		queue(run, fmt.Sprintf("long-%d", n), strings.Repeat("a", 4000))
	}
	runtime := guidanceRunContext(ctx, run)
	full, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Len(t, full, 4)
	queue(run, "one-more", "x")
	_, err = b.ReadWorkGuidance(runtime)
	require.ErrorContains(t, err, "saved guidance exceeds its bounded context")

	counted := guidanceRootRun(write, conversation, "fixture-goal", "counted-plan", "running")
	for n := range maxWorkGuidanceMessages {
		queue(counted, fmt.Sprintf("short-%d", n), "Note.")
	}
	sixteen, err := b.ReadWorkGuidance(guidanceRunContext(ctx, counted))
	require.NoError(t, err)
	require.Len(t, sixteen, maxWorkGuidanceMessages)
	queue(counted, "seventeenth", "Note.")
	_, err = b.ReadWorkGuidance(guidanceRunContext(ctx, counted))
	require.ErrorContains(t, err, "saved guidance exceeds its bounded context")
}

// lateOptionalAnswer records the person's late answer to an optional question
// the run asked and then continued past: the question, the continuation
// receipt, then the answer.
func lateOptionalAnswer(write func(string, map[string]any), owner, run, plan string, n int) string {
	approval := id.NewShortId()
	subject := map[string]any{"kind": "text", "feedbackContinuation": map[string]any{"waitSeconds": 60, "templateFingerprint": plan, "rerunRequestId": "", "overrideKey": ""}}
	now := time.Now().UTC()
	write("createWorkApproval", map[string]any{"approvalId": approval, "runId": run, "stepKey": "section", "kind": "feedback", "subject": subject, "artifactHash": work.ArtifactHash(subject), "question": fmt.Sprintf("How much detail for part %d?", n), "requestedAt": now.Add(-3 * time.Minute).Format(time.RFC3339Nano)})
	write("continueWorkFeedback", map[string]any{"approvalId": approval, "continuedAt": now.Add(-time.Minute).Format(time.RFC3339Nano), "versionTime": time.Now().UTC().Format(time.RFC3339Nano)})
	write("decideWorkApproval", map[string]any{"approvalId": approval, "decision": "answered", "decidedBy": owner, "decidedAt": time.Now().UTC().Format(time.RFC3339Nano), "answer": map[string]any{"text": "Brief."}})
	return approval
}

func guidanceIDs(read []WorkGuidance) []string {
	ids := make([]string, len(read))
	for n, item := range read {
		ids[n] = item.ID
	}
	return ids
}

// Routed guidance and the run's late optional answers are bounded each on
// their own, so neither makes the other's read fail: with routed guidance at
// its cap a late answer is still read, and the agent can acknowledge all it
// was handed. Past its own bound, each source still fails the read loudly.
func TestReadWorkGuidanceBoundsLateAnswersOnTheirOwn(t *testing.T) {
	a, b, ctx, conversation, run, write := guidanceFixture(t)
	actor, _ := auth.AccessFromContext(ctx)
	for n := range 16 {
		routeTurn(t, a, ctx, conversation, AskTurn{ID: fmt.Sprintf("note-%d", n), Prompt: "Note.", Routed: &AskRouting{Targets: []AskRouteTarget{
			{GoalID: "fixture-goal", RunID: run, Status: routeQueued, Text: "Note."},
		}}})
	}
	runtime := guidanceRunContext(ctx, run)
	first := lateOptionalAnswer(write, actor.UserId, run, "original-plan", 0)
	read, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Len(t, read, 17)
	require.Equal(t, "optional-answer-"+BareShortId(first), read[16].ID)
	require.Contains(t, read[16].Prompt, "Late answer to an optional question")
	require.NoError(t, b.AcknowledgeWorkGuidance(runtime, guidanceIDs(read)), "the agent acknowledges everything one read handed it")
	require.Equal(t, routeReceived, storedTurn(t, a, ctx, conversation, "note-15").Routed.Targets[0].Status)

	for n := 1; n < 16; n++ {
		lateOptionalAnswer(write, actor.UserId, run, "original-plan", n)
	}
	read, err = b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Len(t, read, 32, "sixteen routed messages and sixteen late answers")
	require.NoError(t, b.AcknowledgeWorkGuidance(runtime, guidanceIDs(read)))
	lateOptionalAnswer(write, actor.UserId, run, "original-plan", 16)
	_, err = b.ReadWorkGuidance(runtime)
	require.ErrorContains(t, err, "saved guidance exceeds its bounded context")
}

// A message sent while the goal is still compiling waits for the run to start
// and then stays with it: the first step reads it, and so does a later attempt
// of the same run.
func TestGuidanceSentWhileCompilingIsReadOnceRunning(t *testing.T) {
	a, b, ctx, conversation, _, write := guidanceFixture(t)
	// Intake opens the run before compile has chosen a plan.
	run := id.NewShortId()
	write("createWorkRun", map[string]any{"runId": run, "goalId": "fixture-goal", "automationName": "work.compile", "templateFingerprint": "", "triggeredBy": "manual", "status": "compiling", "mode": "live", "input": map[string]any{"conversation": map[string]any{"id": conversation}}, "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	routeTurn(t, a, ctx, conversation, AskTurn{ID: "while-compiling", Prompt: "Actually Phoenix.", Routed: &AskRouting{Targets: []AskRouteTarget{
		{GoalID: "fixture-goal", RunID: run, Title: "Weather in Tucson", Status: routeQueued, Text: "Actually Phoenix."},
	}}})
	runtime := guidanceRunContext(ctx, run)
	idle, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Empty(t, idle, "nothing executes while the goal compiles")
	// Compile finishes on another replica: the run gains its plan and starts.
	write("updateWorkRun", map[string]any{"runId": run, "status": "running", "automationName": "weather", "templateFingerprint": "weather-plan"})
	messages, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Equal(t, []WorkGuidance{{ID: "while-compiling", Prompt: "Actually Phoenix."}}, messages)
	require.NoError(t, b.AcknowledgeWorkGuidance(runtime, []string{messages[0].ID}))
	require.Equal(t, routeReceived, storedTurn(t, a, ctx, conversation, "while-compiling").Routed.Targets[0].Status)
	// A later attempt of the same run still has it.
	write("updateWorkRun", map[string]any{"runId": run, "rerun": map[string]any{"requestId": "again", "reason": "rerun"}})
	actor, _ := auth.AccessFromContext(ctx)
	again, err := b.ReadWorkGuidance(common.ContextWithRun(ctx, common.RunContext{RunId: run, RerunRequestId: "again", GoalId: "fixture-goal", OwnerUserId: actor.UserId, StepKey: "section"}))
	require.NoError(t, err)
	require.Equal(t, messages, again)
}

// The snapshot is a read projection over the runs as they are now. A queued
// message reads Not received only when its run ended before reading it; a
// re-plan or re-run is the same run and changes nothing. An unanswered stop
// question whose goal ended reads ended; labels follow the goal's current
// title. Nothing it projects is written back.
func TestSnapshotProjectsTargetsStopsAndTitles(t *testing.T) {
	a, b, ctx, conversation, run, write := guidanceFixture(t)
	hike := guidanceRootRun(write, conversation, "hike-goal", "hike-plan", "running")
	report := guidanceRootRun(write, conversation, "report-goal", "report-plan", "running")
	routeTurn(t, a, ctx, conversation, AskTurn{ID: "earlier", Prompt: "Start with the trail.", Routed: &AskRouting{Targets: []AskRouteTarget{
		{GoalID: "fixture-goal", RunID: run, Title: "Weather in Tucson", Status: routeQueued, Text: "Start with the trail."},
		{GoalID: "hike-goal", RunID: hike, Title: "Hike near Tucson", Status: routeReceived, Text: "Start with the trail."},
	}}})
	routeTurn(t, a, ctx, conversation, AskTurn{ID: "phoenix", Prompt: "Actually Phoenix.", Routed: &AskRouting{
		Targets: []AskRouteTarget{
			{GoalID: "fixture-goal", RunID: run, Title: "Weather in Tucson", Status: routeQueued, Text: "Actually Phoenix."},
			{GoalID: "hike-goal", RunID: hike, Title: "Hike near Tucson", Status: routeQueued, Text: "Actually Phoenix."},
			{GoalID: "report-goal", RunID: report, Title: "Tucson report", Status: routeQueued, Text: "Actually Phoenix."},
		},
		Reply: "Switching all three to Phoenix.",
	}})
	routeTurn(t, a, ctx, conversation, AskTurn{ID: "drop", Prompt: "Never mind the report or the weather.", Routed: &AskRouting{
		Stops: []AskStopQuestion{{GoalID: "report-goal", RunID: report, Title: "Tucson report"}, {GoalID: "fixture-goal", RunID: run, Title: "Weather in Tucson"}},
	}})
	// The weather goal is retitled and re-planned under a new rerun request;
	// the hike is cancelled and the report finishes, neither having read it.
	write("updateWorkRun", map[string]any{"runId": run, "title": "Weather in Phoenix", "classification": map[string]any{"workTitle": "Weather in Tucson"}, "templateFingerprint": "replanned", "rerun": map[string]any{"requestId": "replan-1", "reason": "replan"}})
	write("updateWorkRun", map[string]any{"runId": hike, "status": "cancelled"})
	write("updateWorkRun", map[string]any{"runId": report, "status": "succeeded"})

	call, err := parser.RenderCall("askConversationSnapshot", map[string]any{"conversationId": conversation})
	require.NoError(t, err)
	result, err := b.Execute(ctx, "builtin "+call)
	require.NoError(t, err)
	rows := MaterializeRows(result.OutputPayload())
	require.Len(t, rows, 1)
	snapshot, err := approvalTranscript(rows[0])
	require.NoError(t, err)
	turns := map[string]AskTurn{}
	for _, turn := range snapshot.Turns {
		turns[turn.ID] = turn
	}
	require.Equal(t, "Weather in Phoenix", turns["original"].WorkTitle, "the run's own title wins over the intake title")
	earlier, phoenix, drop := turns["earlier"].Routed, turns["phoenix"].Routed, turns["drop"].Routed
	require.Equal(t, routeQueued, earlier.Targets[0].Status, "a re-plan keeps the run's guidance")
	require.Equal(t, routeReceived, earlier.Targets[1].Status, "a receipt stays a receipt after the goal ends")
	require.Equal(t, routeQueued, phoenix.Targets[0].Status)
	require.Equal(t, "Weather in Phoenix", phoenix.Targets[0].Title)
	require.Equal(t, routeNotReceived, phoenix.Targets[1].Status, "cancelled before reading it")
	require.Equal(t, "Hike near Tucson", phoenix.Targets[1].Title)
	require.Equal(t, routeNotReceived, phoenix.Targets[2].Status, "finished before reading it")
	require.Equal(t, stopDecisionEnded, drop.Stops[0].Decision)
	require.Empty(t, drop.Stops[1].Decision, "a running goal's stop question still waits for the person")
	require.Equal(t, "Weather in Phoenix", drop.Stops[1].Title)

	saved := storedTurn(t, a, ctx, conversation, "phoenix").Routed
	require.Equal(t, routeQueued, saved.Targets[1].Status)
	require.Equal(t, "Weather in Tucson", saved.Targets[0].Title)
	require.Empty(t, storedTurn(t, a, ctx, conversation, "drop").Routed.Stops[0].Decision)
}
