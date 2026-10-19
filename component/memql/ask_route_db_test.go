package memql

// RunAsk routes a person's message to the goals already running in their
// conversation before intake (design record
// docs/superpowers/specs/2026-10-19-ask-message-routing-design.md, "Flow",
// "Apply", "Delivery by goal state" and "Failure handling"). The routing model
// answers from a script, the routing recipe runs statement by statement
// (askRoutingRecipe), and every work builtin routing reaches is stubbed and
// recorded.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// routeFixture is one person's Ask conversation with work already running in
// it, on an engine whose routing model answers from a script.
type routeFixture struct {
	t            *testing.T
	e            *MemQLEngine
	ctx          context.Context
	user         string
	conversation string
	model        *askRouteModel
	// beforeModel runs as each routing call reaches the model.
	beforeModel func()
	// routed is every model call as the router would serve it, with the
	// turn's choice applied (ApplyRunRouting).
	routed []airoute.ResolveRequest
	// What each work builtin was asked, in order.
	intake   []string
	stopped  []string
	answered []map[string]any
	retitled []map[string]any
	// events is what the person's stream was sent.
	events []WorkEvent
	// held names every model call and work builtin routing reached while the
	// conversation's write lock was held. Intake is not checked: it opens its
	// goal under that lock by design, as it always has.
	held []string
	// stamped names every work builtin reached with the turn's route choice
	// on its context; only the routing call may carry it.
	stamped []string
	// runs is each goal's run, for the stubs that act on a goal.
	runs map[string]string
	// intakeAnswer is what a goal intake opens answers.
	intakeAnswer string
	// intakeAcknowledgement, when set, is how intake's goal acknowledged the
	// request: that goal is background work still running.
	intakeAcknowledgement string
	// stopReply, when set, is cancelGoal's answer.
	stopReply map[string]any
}

// newRouteFixture boots a private engine (it is changed: a scripted model, the
// recipe runner, stubbed work builtins) whose routing model gives answers in
// order, and opens a conversation for one person.
func newRouteFixture(t *testing.T, answers ...string) *routeFixture {
	t.Helper()
	e, _, _ := readMergeTestEngine(t)
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	ctx := askTestActor()
	actor, _ := auth.AccessFromContext(ctx)
	f := &routeFixture{t: t, e: e, ctx: ctx, user: actor.UserId, conversation: askTestConversation(t, e, ctx),
		model: &askRouteModel{answers: answers}, runs: map[string]string{}, intakeAnswer: "Here is what I found."}
	// Every routing call reaches the model: an identical prompt answered from
	// the response cache would hide a call the turn should never have made.
	e.aiRuntime.cache = nil
	e.SetScopedWorkflowRunner(askRoutingRecipe)
	e.SetAIResolver(testAIResolver{fn: func(callCtx context.Context, req airoute.ResolveRequest) (ResolvedProvider, error) {
		f.unlocked("model call " + req.PromptName)
		if run, inRun := common.RunFromContext(callCtx); inRun && run.RunId != "" {
			return ResolvedProvider{}, fmt.Errorf("routing ran inside run %s", run.RunId)
		}
		served, err := ApplyRunRouting(callCtx, req)
		if err != nil {
			return ResolvedProvider{}, err
		}
		f.routed = append(f.routed, served)
		// What the router reports of every call it makes.
		airoute.Observe(callCtx, airoute.CallObservation{ID: id.NewShortId(), Phase: "running", PromptName: req.PromptName, Provider: "fleet:router", Model: "router-test", Modality: string(req.Modality)})
		if f.beforeModel != nil {
			f.beforeModel()
		}
		return ResolvedProvider{Client: f.model, Resolution: airoute.Resolution{ProviderName: "fleet:router", Model: "router-test"}}, nil
	}})
	e.builtinExecutorHandlers["integration.work.createGoal"] = func(callCtx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		f.intake = append(f.intake, stringArg(args, "statement"))
		goalID, runID := id.NewShortId(), id.NewShortId()
		if f.intakeAcknowledgement != "" {
			f.write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "intake", "templateFingerprint": "intake", "triggeredBy": "manual", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
			f.write("updateWorkRun", map[string]any{"runId": runID, "classification": map[string]any{"workload": "research", "workTitle": "Background work", "acknowledgement": f.intakeAcknowledgement}})
			raw, _ := json.Marshal(map[string]any{"goalId": goalID, "runId": runID})
			return []memorynodes.MemoryNode{{ID: "intake", Payload: raw}}, nil
		}
		runCtx := common.ContextWithRun(callCtx, common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: f.user, StepKey: "reason"})
		if err := e.RecordWorkProgress(runCtx, WorkEvent{ID: "reply", Kind: "response", Phase: "completed", Text: f.intakeAnswer}); err != nil {
			return nil, err
		}
		f.write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "intake", "templateFingerprint": "intake", "triggeredBy": "manual", "status": "succeeded", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
		raw, _ := json.Marshal(map[string]any{"goalId": goalID, "runId": runID})
		return []memorynodes.MemoryNode{{ID: "intake", Payload: raw}}, nil
	}
	e.builtinExecutorHandlers["integration.work.cancelGoal"] = func(callCtx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		f.checkCaller(callCtx, "cancelGoal")
		goal := BareShortId(stringArg(args, "goalId"))
		if f.stopReply != nil {
			return askValue(f.stopReply)
		}
		f.stopped = append(f.stopped, goal)
		// As the work integration does: every run of the goal is asked to stop.
		if run := f.runs[goal]; run != "" {
			f.write("updateWorkRun", map[string]any{"runId": run, "cancelRequested": true})
		}
		return askValue(map[string]any{"goalId": goal, "runsAsked": 1, "goalClosed": true})
	}
	e.builtinExecutorHandlers["integration.work.decideApproval"] = func(callCtx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		f.checkCaller(callCtx, "decideApproval")
		f.answered = append(f.answered, args)
		approval := BareShortId(stringArg(args, "approvalId"))
		// As the work integration does: the request records the decision.
		f.write("decideWorkApproval", map[string]any{"approvalId": approval, "decision": "answered", "decidedBy": f.user, "decidedAt": time.Now().UTC().Format(time.RFC3339Nano), "answer": args["answer"]})
		return askValue(map[string]any{"approvalId": approval, "decision": "answered", "runResumed": true})
	}
	e.builtinExecutorHandlers["integration.work.retitleRun"] = func(callCtx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		f.checkCaller(callCtx, "retitleRun")
		if run, inRun := common.RunFromContext(callCtx); inRun && run.RunId != "" {
			return nil, fmt.Errorf("work: a title change needs a person's message, not a running job")
		}
		run, title := BareShortId(stringArg(args, "runId")), stringArg(args, "title")
		f.retitled = append(f.retitled, map[string]any{"runId": run, "title": title})
		f.write("updateWorkRun", map[string]any{"runId": run, "title": title})
		return askValue(map[string]any{"runId": run, "title": title})
	}
	return f
}

func (f *routeFixture) write(name string, args map[string]any) {
	f.t.Helper()
	call, err := parser.RenderCall(name, args)
	require.NoError(f.t, err)
	_, err = f.e.Execute(auth.ContextWithInternalOrigin(f.ctx), "mutation "+call)
	require.NoError(f.t, err)
}

// unlocked records name when the conversation's write lock is held: the lock
// is not reentrant, and a model call or work builtin under it would hold every
// other writer of the conversation for as long as the call takes.
func (f *routeFixture) unlocked(name string) {
	probe, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	hold, err := f.e.holdAskConversation(probe, f.conversation, "write")
	if err != nil {
		f.held = append(f.held, name)
		return
	}
	hold.release()
}

// checkCaller records a work builtin reached under the conversation's lock or
// with the turn's route choice: only the routing call carries the choice.
func (f *routeFixture) checkCaller(ctx context.Context, name string) {
	f.unlocked(name)
	if !GoalRouteChoice(ctx, f.user).IsZero() {
		f.stamped = append(f.stamped, name)
	}
}

// stallRouting makes every routing call hang the way a routing door that does
// not answer does, and gives each decision timeoutSeconds, as the recipe
// does. letGo releases a stalled call, so a failing test never leaves one
// behind.
func (f *routeFixture) stallRouting(timeoutSeconds int) (letGo func()) {
	release := make(chan struct{})
	var once sync.Once
	letGo = func() { once.Do(func() { close(release) }) }
	f.t.Cleanup(letGo)
	f.model.stall = release
	f.e.SetScopedWorkflowRunner(askRoutingRecipeWithin(timeoutSeconds))
	return letGo
}

// reconcile is what another turn starting in the conversation does first:
// under the write lock, it projects every turn over the runs as they are now
// and saves the projection.
func (f *routeFixture) reconcile() {
	f.t.Helper()
	release, err := f.e.lockAskConversationKind(f.ctx, f.conversation, "write")
	require.NoError(f.t, err)
	defer release()
	row, err := f.e.askRead(ContextWithFreshRead(f.ctx), f.conversation)
	require.NoError(f.t, err)
	transcript, err := approvalTranscript(row)
	require.NoError(f.t, err)
	f.e.reconcileAskRuns(ContextWithFreshRead(f.ctx), &transcript)
	require.NoError(f.t, f.e.askSave(f.ctx, f.conversation, stringArg(row, "title"), transcript))
}

// record appends a turn to the stored conversation as it is.
func (f *routeFixture) record(turn AskTurn) {
	f.t.Helper()
	row, err := f.e.askRead(ContextWithFreshRead(f.ctx), f.conversation)
	require.NoError(f.t, err)
	transcript, err := approvalTranscript(row)
	require.NoError(f.t, err)
	if turn.Activity == nil {
		turn.Activity = []WorkEvent{}
	}
	if turn.StartedAt.IsZero() {
		turn.StartedAt = time.Now().UTC()
	}
	transcript.Turns = append(transcript.Turns, turn)
	require.NoError(f.t, f.e.askSave(f.ctx, f.conversation, stringArg(row, "title"), transcript))
}

// goal opens a goal in the conversation the way Ask records one: a root run of
// this conversation titled by intake, and the turn that asked for it.
func (f *routeFixture) goal(turnID, prompt, title, status string) (goalID, runID string) {
	f.t.Helper()
	goalID, runID = id.NewShortId(), id.NewShortId()
	f.write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "work", "templateFingerprint": "plan", "triggeredBy": "manual", "status": status, "mode": "live",
		"input": map[string]any{"conversation": map[string]any{"id": f.conversation}}, "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	f.write("updateWorkRun", map[string]any{"runId": runID, "classification": map[string]any{"workTitle": title}})
	f.runs[goalID] = runID
	f.record(AskTurn{ID: turnID, Prompt: prompt, GoalID: goalID, RunID: runID, State: "queued", Background: true, WorkTitle: title})
	return goalID, runID
}

// parked opens a goal parked on one request to the person, the way the work
// spine parks it: the request first, then the run's wait on it.
func (f *routeFixture) parked(turnID, prompt, title string, request map[string]any) (goalID, runID, approvalID string) {
	f.t.Helper()
	goalID, runID = f.goal(turnID, prompt, title, "running")
	approvalID = id.NewShortId()
	row := map[string]any{"approvalId": approvalID, "runId": runID, "stepKey": "plan", "artifactHash": "request", "requestedAt": time.Now().UTC().Format(time.RFC3339Nano)}
	for key, value := range request {
		row[key] = value
	}
	f.write("createWorkApproval", row)
	f.write("updateWorkRun", map[string]any{"runId": runID, "status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": approvalID, "approvalKind": request["kind"]}})
	return goalID, runID, approvalID
}

// ask sends one message as the person, and returns what their stream was sent.
func (f *routeFixture) ask(turnID, message string, route AskRoute, attachments ...string) (chunks []string, answer string, err error) {
	answer, err = f.e.RunAsk(f.ctx, f.conversation, turnID, message, "", route, attachments, func(text string) {
		chunks = append(chunks, text)
	}, func(event WorkEvent) {
		f.events = append(f.events, event)
	})
	return chunks, answer, err
}

// turn is one turn as it is saved.
func (f *routeFixture) turn(turnID string) AskTurn {
	f.t.Helper()
	return storedTurn(f.t, f.e, f.ctx, f.conversation, turnID)
}

// snapshot is the conversation as the person's client reads it.
func (f *routeFixture) snapshot() map[string]AskTurn {
	f.t.Helper()
	nodes, err := f.e.askConversationSnapshotBuiltin(f.ctx, map[string]any{"conversationId": f.conversation}, 0)
	require.NoError(f.t, err)
	var row struct {
		Transcript askTranscript `json:"transcript"`
	}
	require.NoError(f.t, json.Unmarshal(nodes[0].Payload, &row))
	turns := map[string]AskTurn{}
	for _, turn := range row.Transcript.Turns {
		turns[turn.ID] = turn
	}
	return turns
}

// guidance is what the goal's agent reads at its next step, on its own run.
func (f *routeFixture) guidance(goalID, runID string) []WorkGuidance {
	f.t.Helper()
	read, err := f.e.ReadWorkGuidance(common.ContextWithRun(f.ctx, common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: f.user, StepKey: "section"}))
	require.NoError(f.t, err)
	return read
}

// fallbacks are a turn's Activity entries saying routing was skipped.
func fallbacks(turn AskTurn) []WorkEvent {
	var found []WorkEvent
	for _, event := range turn.Activity {
		if event.Kind == "action" && event.Phase == "fallback" && event.Name == "Routing message" {
			found = append(found, event)
		}
	}
	return found
}

// routingCalls are a turn's Activity entries for the routing model call.
func routingCalls(events []WorkEvent) []WorkEvent {
	var found []WorkEvent
	for _, event := range events {
		if event.Kind == "model" && event.Call != nil && event.Call.PromptName == "askMessageRouting" {
			found = append(found, event)
		}
	}
	return found
}

// With no goal running in the conversation nothing is routed: no model is
// asked and the message goes to intake as written, exactly as before. A goal
// that finished, or was asked to stop, is not a destination.
func TestRunAskSkipsRoutingWithoutActiveGoals(t *testing.T) {
	f := newRouteFixture(t)
	chunks, answer, err := f.ask("first", "What's the weather in Tucson?", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, f.intakeAnswer, answer)
	require.Equal(t, []string{f.intakeAnswer}, chunks)
	f.goal("map", "Print a map of the trail.", "Trail map", "succeeded")
	_, stopping := f.goal("camp", "Book a campsite.", "Campsite", "running")
	f.write("updateWorkRun", map[string]any{"runId": stopping, "cancelRequested": true})
	_, _, err = f.ask("second", "And tomorrow?", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, []string{"What's the weather in Tucson?", "And tomorrow?"}, f.intake)
	require.Empty(t, f.model.prompts, "no routing call")
	require.Empty(t, f.routed)
	for _, turnID := range []string{"first", "second"} {
		turn := f.turn(turnID)
		require.Nil(t, turn.Routed, turnID)
		require.Empty(t, fallbacks(turn), turnID)
		require.NotEmpty(t, turn.RunID, turnID)
	}
}

// The owner's example: a goal is checking the weather in Tucson, and "Actually
// I'm in Phoenix" changes that goal instead of starting a second one. The
// message is queued for the goal word for word, the goal is retitled, the
// reply is the whole streamed answer, and the routing call was the person's,
// under their route.
func TestRunAskRoutesACorrectionToTheRunningGoal(t *testing.T) {
	f := newRouteFixture(t, routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "Weather in Phoenix")}, "reply": "Switching the weather check to Phoenix."}))
	goal, run := f.goal("weather", "What's the weather in Tucson?", "Weather in Tucson", "running")
	chunks, answer, err := f.ask("phoenix", "Actually I'm in Phoenix", AskRoute{Source: "policy:localFirst"})
	require.NoError(t, err)
	require.Equal(t, "Switching the weather check to Phoenix.", answer)
	require.Equal(t, []string{answer}, chunks, "the reply is the turn's whole streamed text")
	require.Empty(t, f.intake, "no second goal")
	require.Equal(t, []map[string]any{{"runId": run, "title": "Weather in Phoenix"}}, f.retitled)

	turn := f.turn("phoenix")
	require.Equal(t, "done", turn.State)
	require.Equal(t, answer, turn.Answer)
	require.Empty(t, turn.RunID, "a routed message opens no goal of its own")
	require.Equal(t, &AskRouting{Targets: []AskRouteTarget{{GoalID: goal, RunID: run, Title: "Weather in Phoenix", Status: routeQueued, Text: "Actually I'm in Phoenix"}}}, turn.Routed)
	require.Len(t, routingCalls(turn.Activity), 1, "Activity names the routing call")
	require.Len(t, routingCalls(f.events), 1, "and the person's stream is told of it as it runs")

	require.Len(t, f.routed, 1)
	require.Equal(t, "askMessageRouting", f.routed[0].PromptName)
	require.Equal(t, airoute.LevelFast, f.routed[0].Level)
	require.Equal(t, "localFirst", f.routed[0].Route, "the routing call goes where the person's turn said")
	require.Empty(t, f.stamped, "no other call on the turn carries the route choice")
	require.Empty(t, f.held, "no model call or work builtin ran under the conversation's write lock")

	require.Equal(t, []WorkGuidance{{ID: "phoenix", Prompt: "Actually I'm in Phoenix"}}, f.guidance(goal, run), "the goal's agent reads it at its next step")
	snapshot := f.snapshot()
	require.Equal(t, "Weather in Phoenix", snapshot["weather"].WorkTitle, "the goal's card takes the new title")
	require.Equal(t, routeQueued, snapshot["phoenix"].Routed.Targets[0].Status)
}

// One message corrects a goal and asks something new: the correction goes to
// the goal, the new request to intake in its own words, and the reply comes
// first, before the new request's own answer, which it never becomes part of.
func TestRunAskSplitsAMixedMessage(t *testing.T) {
	f := newRouteFixture(t, routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "Weather in Phoenix")}, "newRequest": "What time is it in Phoenix right now?", "reply": "Switching the weather check to Phoenix."}))
	f.intakeAnswer = "It is 3 PM in Phoenix."
	goal, run := f.goal("weather", "What's the weather in Tucson?", "Weather in Tucson", "running")
	message := "Actually I'm in Phoenix. Also, what time is it there?"
	chunks, answer, err := f.ask("split", message, AskRoute{})
	require.NoError(t, err)
	require.Equal(t, []string{"What time is it in Phoenix right now?"}, f.intake, "intake takes the new request alone")
	require.Equal(t, []string{"Switching the weather check to Phoenix.\n\n", "It is 3 PM in Phoenix."}, chunks, "the reply first, then the new request's answer")
	require.Equal(t, "It is 3 PM in Phoenix.", answer)

	turn := f.turn("split")
	require.Equal(t, "done", turn.State)
	require.NotEmpty(t, turn.RunID)
	require.Equal(t, "It is 3 PM in Phoenix.", turn.Answer, "the reply is never part of the new goal's answer")
	require.Equal(t, &AskRouting{
		Targets:   []AskRouteTarget{{GoalID: goal, RunID: run, Title: "Weather in Phoenix", Status: routeQueued, Text: message}},
		Reply:     "Switching the weather check to Phoenix.",
		Statement: "What time is it in Phoenix right now?",
	}, turn.Routed)
	require.Equal(t, []map[string]any{{"runId": run, "title": "Weather in Phoenix"}}, f.retitled)
	require.Equal(t, []WorkGuidance{{ID: "split", Prompt: message}}, f.guidance(goal, run), "the goal receives the person's whole message, word for word")
	require.Empty(t, f.held)
}

// One message can apply to several related goals: each takes it, in the
// order the decision named them, and only a goal given a new title is
// retitled.
func TestRunAskUpdatesTwoGoals(t *testing.T) {
	f := newRouteFixture(t, routeAnswer(map[string]any{"updates": []any{routeUpdate("g2", "Weather in Phoenix"), routeUpdate("g1", "")}, "reply": "Switching both to Phoenix."}))
	weatherGoal, weather := f.goal("weather", "What's the weather in Tucson?", "Weather in Tucson", "running")
	hikeGoal, hike := f.goal("hike", "Plan a hike near Tucson.", "Hike near Tucson", "compiling")
	message := "I'm in Phoenix, not Tucson."
	chunks, answer, err := f.ask("phoenix", message, AskRoute{})
	require.NoError(t, err)
	require.Equal(t, "Switching both to Phoenix.", answer)
	require.Equal(t, []string{answer}, chunks)
	require.Empty(t, f.intake)
	require.Equal(t, []AskRouteTarget{
		{GoalID: weatherGoal, RunID: weather, Title: "Weather in Phoenix", Status: routeQueued, Text: message},
		{GoalID: hikeGoal, RunID: hike, Title: "Hike near Tucson", Status: routeQueued, Text: message},
	}, f.turn("phoenix").Routed.Targets)
	require.Equal(t, []map[string]any{{"runId": weather, "title": "Weather in Phoenix"}}, f.retitled)
	require.Equal(t, []WorkGuidance{{ID: "phoenix", Prompt: message}}, f.guidance(weatherGoal, weather))
}

// An unclear target is asked about, not guessed: the reply is the question
// and nothing is delivered. The next message answers it, and both of the
// person's messages reach the goal they chose, together and word for word.
func TestRunAskAsksWhenUnclearThenDeliversBoth(t *testing.T) {
	f := newRouteFixture(t,
		routeAnswer(map[string]any{"clarify": "The weather check or the report?", "reply": "Which one do you mean?"}),
		routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "")}, "resolvesClarification": true, "reply": "Making the report shorter."}),
	)
	weatherGoal, weather := f.goal("weather", "What's the weather in Tucson?", "Weather in Tucson", "running")
	reportGoal, report := f.goal("report", "Write the Tucson report.", "Tucson report", "running")

	chunks, answer, err := f.ask("shorter", "Make it shorter", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, "The weather check or the report?", answer)
	require.Equal(t, []string{answer}, chunks)
	asked := f.turn("shorter")
	require.Equal(t, "done", asked.State)
	require.Equal(t, "The weather check or the report?", asked.Answer)
	require.Equal(t, &AskRouting{Clarify: true}, asked.Routed, "a question delivers nothing")
	require.Empty(t, f.guidance(weatherGoal, weather))
	require.Empty(t, f.guidance(reportGoal, report))

	_, answer, err = f.ask("which", "The report", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, "Making the report shorter.", answer)
	require.Len(t, f.model.prompts, 2)
	require.Contains(t, f.model.prompts[1], "YOUR QUESTION WAITING FOR AN ANSWER")
	require.Contains(t, f.model.prompts[1], `Their previous message: "Make it shorter"`)
	require.Equal(t, []AskRouteTarget{{GoalID: reportGoal, RunID: report, Title: "Tucson report", Status: routeQueued, Text: "Make it shorter\n\nThe report"}}, f.turn("which").Routed.Targets)
	require.True(t, f.turn("shorter").Routed.Resolved, "the question is settled")
	require.Equal(t, []WorkGuidance{{ID: "which", Prompt: "Make it shorter\n\nThe report"}}, f.guidance(reportGoal, report))
	require.Empty(t, f.guidance(weatherGoal, weather))
	require.Empty(t, f.intake)
}

// "Never mind the weather one" asks before stopping anything; a later clear
// yes stops the goal, as the person, and the question records it.
func TestRunAskStopAsksThenConfirms(t *testing.T) {
	f := newRouteFixture(t,
		routeAnswer(map[string]any{"stops": []any{"g1"}, "reply": "Stop the weather check? Confirm on its card."}),
		routeAnswer(map[string]any{"confirmStops": []any{"g1"}, "reply": "Stopping the weather check."}),
	)
	goal, run := f.goal("weather", "What's the weather in Tucson?", "Weather in Tucson", "running")
	_, answer, err := f.ask("drop", "Never mind the weather one", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, "Stop the weather check? Confirm on its card.", answer)
	require.Empty(t, f.stopped, "nothing is stopped until the person confirms")
	require.Equal(t, []AskStopQuestion{{GoalID: goal, RunID: run, Title: "Weather in Tucson"}}, f.turn("drop").Routed.Stops)
	require.Equal(t, "done", f.turn("drop").State)

	_, answer, err = f.ask("yes", "Yes", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, "Stopping the weather check.", answer)
	require.Contains(t, f.model.prompts[1], "You asked whether to stop: g1")
	require.Equal(t, []string{goal}, f.stopped)
	require.Equal(t, stopDecisionStopped, f.turn("drop").Routed.Stops[0].Decision)
	yes := f.turn("yes")
	require.Equal(t, "done", yes.State)
	require.Equal(t, answer, yes.Answer)
	require.Equal(t, &AskRouting{}, yes.Routed, "a message that only confirmed a stop is still a routed message")
	require.Empty(t, f.intake)
	require.Empty(t, f.held)
	require.Empty(t, f.stamped)
}

// The stop went through, so its question says stopped, even when another turn
// starting meanwhile saw the goal asked to stop and saved the question as
// ended before routing recorded the answer.
func TestRunAskRecordsAConfirmedStopASnapshotEndedMeanwhile(t *testing.T) {
	f := newRouteFixture(t, routeAnswer(map[string]any{"confirmStops": []any{"g1"}, "reply": "Stopping the weather check."}))
	goal, run := f.goal("weather", "What's the weather in Tucson?", "Weather in Tucson", "running")
	f.record(AskTurn{ID: "drop", Prompt: "Never mind the weather one", Answer: "Stop the weather check?", State: "done",
		Routed: &AskRouting{Stops: []AskStopQuestion{{GoalID: goal, RunID: run, Title: "Weather in Tucson"}}}})
	stop := f.e.builtinExecutorHandlers["integration.work.cancelGoal"]
	f.e.builtinExecutorHandlers["integration.work.cancelGoal"] = func(callCtx context.Context, args map[string]any, target int) ([]memorynodes.MemoryNode, error) {
		rows, err := stop(callCtx, args, target)
		f.reconcile()
		require.Equal(t, stopDecisionEnded, f.turn("drop").Routed.Stops[0].Decision, "the other turn saved the question as ended")
		return rows, err
	}
	_, answer, err := f.ask("yes", "Yes", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, "Stopping the weather check.", answer)
	require.Equal(t, []string{goal}, f.stopped)
	require.Equal(t, stopDecisionStopped, f.turn("drop").Routed.Stops[0].Decision)
}

// A confirmed stop that does not go through is never claimed: the question
// stays open on the goal's card and the reply says what happened.
func TestRunAskSaysWhenAConfirmedStopFails(t *testing.T) {
	f := newRouteFixture(t, routeAnswer(map[string]any{"confirmStops": []any{"g1"}, "reply": "Stopping the weather check."}))
	goal, run := f.goal("weather", "What's the weather in Tucson?", "Weather in Tucson", "running")
	f.record(AskTurn{ID: "drop", Prompt: "Never mind the weather one", Answer: "Stop the weather check?", State: "done",
		Routed: &AskRouting{Stops: []AskStopQuestion{{GoalID: goal, RunID: run, Title: "Weather in Tucson"}}}})
	f.stopReply = map[string]any{"goalId": goal, "runsAsked": 1, "goalClosed": true, "runsRefused": []any{run}}
	_, answer, err := f.ask("yes", "Yes, stop it", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, "Weather in Tucson couldn't be stopped. Use Stop on its card.", answer)
	require.Empty(t, f.turn("drop").Routed.Stops[0].Decision, "the question stays open")
	require.Equal(t, answer, f.turn("yes").Answer)
}

// A goal that ends between listing and delivery does not get the message,
// and when every goal it was for had ended and nothing else was decided, the
// message goes to intake in the person's own words: a late "actually Phoenix"
// still gets Phoenix. The reply says what happened before the new answer.
func TestRunAskRoutesToIntakeWhenEveryTargetEnded(t *testing.T) {
	f := newRouteFixture(t, routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "Weather in Phoenix")}, "reply": "Switching the weather check to Phoenix."}))
	goal, run := f.goal("weather", "What's the weather in Tucson?", "Weather in Tucson", "running")
	// The goal finishes while routing decides.
	f.beforeModel = func() {
		f.write("updateWorkRun", map[string]any{"runId": run, "status": "succeeded", "finishedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	}
	chunks, answer, err := f.ask("phoenix", "Actually I'm in Phoenix", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, []string{"Actually I'm in Phoenix"}, f.intake)
	require.Empty(t, f.retitled, "nothing was delivered, so nothing is retitled")
	reply := "Weather in Tucson already finished, so it didn't get this."
	require.Equal(t, []string{reply + "\n\n", f.intakeAnswer}, chunks)
	require.Equal(t, f.intakeAnswer, answer)
	turn := f.turn("phoenix")
	require.NotEmpty(t, turn.RunID)
	require.Equal(t, f.intakeAnswer, turn.Answer)
	require.Equal(t, &AskRouting{
		Targets: []AskRouteTarget{{GoalID: goal, RunID: run, Title: "Weather in Tucson", Status: routeNotReceived, Reason: routeReasonEnded, Text: "Actually I'm in Phoenix"}},
		Reply:   reply,
	}, turn.Routed)
}

// A model that names a goal routing did not show it, or one goal both updated
// and stopped, is refused, retried once, and then the message goes to intake
// as written, with an Activity entry saying routing was skipped.
func TestRunAskFallsBackAfterAnInvalidRouteTwice(t *testing.T) {
	f := newRouteFixture(t,
		routeAnswer(map[string]any{"updates": []any{routeUpdate("g7", "")}, "reply": "Sent."}),
		routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "")}, "stops": []any{"g1"}, "reply": "Sent."}),
	)
	goal, run := f.goal("weather", "What's the weather in Tucson?", "Weather in Tucson", "running")
	chunks, answer, err := f.ask("phoenix", "Actually I'm in Phoenix", AskRoute{})
	require.NoError(t, err)
	require.Len(t, f.model.prompts, 2, "one decision and one retry")
	require.Equal(t, []string{"Actually I'm in Phoenix"}, f.intake)
	require.Equal(t, f.intakeAnswer, answer)
	require.Equal(t, []string{f.intakeAnswer}, chunks, "no reply is said for a message nothing routed")
	turn := f.turn("phoenix")
	require.Equal(t, &AskRouting{Skipped: "invalid"}, turn.Routed)
	skipped := fallbacks(turn)
	require.Len(t, skipped, 1)
	require.NotEmpty(t, skipped[0].Error)
	require.Empty(t, f.guidance(goal, run))
	require.Empty(t, f.retitled)
	require.Empty(t, f.stopped)
}

// Routing that cannot run -- no model, a timeout -- decides nothing: the
// message goes to intake as written and Activity says routing was skipped.
func TestRunAskFallsBackWhenRoutingIsUnavailable(t *testing.T) {
	f := newRouteFixture(t)
	f.model.err = errors.New("no model is available for this call")
	goal, run := f.goal("weather", "What's the weather in Tucson?", "Weather in Tucson", "running")
	_, answer, err := f.ask("phoenix", "Actually I'm in Phoenix", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, f.intakeAnswer, answer)
	require.Equal(t, []string{"Actually I'm in Phoenix"}, f.intake)
	turn := f.turn("phoenix")
	require.Equal(t, &AskRouting{Skipped: "unavailable"}, turn.Routed)
	skipped := fallbacks(turn)
	require.Len(t, skipped, 1)
	require.Contains(t, skipped[0].Error, "no model is available")
	require.Empty(t, f.guidance(goal, run))
}

// A routing call takes as long as its recipe allows and no longer. A routing
// door that does not answer -- a person's own model that hangs -- is a routing
// failure: the message is not held behind it, it goes to intake as written,
// and Activity says routing was skipped because it ran out of time.
func TestRunAskTakesTheMessageToIntakeWhenRoutingTimesOut(t *testing.T) {
	f := newRouteFixture(t)
	letGo := f.stallRouting(1)
	goal, run := f.goal("weather", "What's the weather in Tucson?", "Weather in Tucson", "running")
	var chunks []string
	var answer string
	var err error
	started := time.Now()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		chunks, answer, err = f.ask("phoenix", "Actually I'm in Phoenix", AskRoute{})
	}()
	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		letGo()
		<-finished
		t.Fatal("the message waited on its routing call for more than 15 seconds")
	}
	elapsed := time.Since(started)
	require.NoError(t, err)
	require.GreaterOrEqual(t, elapsed, time.Second, "the routing call had its second")
	require.Less(t, elapsed, 10*time.Second, "and no more")
	require.Equal(t, []string{"Actually I'm in Phoenix"}, f.intake, "intake takes the message as written")
	require.Equal(t, f.intakeAnswer, answer)
	require.Equal(t, []string{f.intakeAnswer}, chunks)
	turn := f.turn("phoenix")
	require.Equal(t, "done", turn.State)
	require.NotEmpty(t, turn.RunID)
	require.Equal(t, &AskRouting{Skipped: "unavailable"}, turn.Routed)
	skipped := fallbacks(turn)
	require.Len(t, skipped, 1)
	require.Contains(t, skipped[0].Error, "no routing decision within 1s")
	require.Empty(t, f.guidance(goal, run), "nothing was delivered")
}

// A person who leaves while routing decides decided nothing: the turn ends
// interrupted, as any abandoned turn does, with no skip recorded and no goal
// opened. Leaving is not a routing call running out of time.
func TestRunAskLeftWhileRoutingEndsInterrupted(t *testing.T) {
	f := newRouteFixture(t)
	f.stallRouting(30)
	f.goal("weather", "What's the weather in Tucson?", "Weather in Tucson", "running")
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	// The person's stream drops while the routing model is asked.
	f.beforeModel = cancel
	started := time.Now()
	_, err := f.e.RunAsk(ctx, f.conversation, "phoenix", "Actually I'm in Phoenix", "", AskRoute{}, nil, nil, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(started), 10*time.Second)
	turn := f.turn("phoenix")
	require.Equal(t, "interrupted", turn.State)
	require.Nil(t, turn.Routed, "leaving is not a routing failure")
	require.Empty(t, fallbacks(turn))
	require.Empty(t, f.intake, "no goal is opened for a message nobody waits on")
}

// Resending a turn after a dropped stream returns what was saved: the model
// is not asked again and nothing is delivered twice, neither for a message
// that answered a goal's question nor for one that was split and whose new
// goal is still working.
func TestRunAskRetryReturnsTheSavedRoute(t *testing.T) {
	f := newRouteFixture(t,
		routeAnswer(map[string]any{"updates": []any{routeUpdate("g2", "")}, "reply": "Telling the hike plan you want the short loop."}),
		routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "")}, "newRequest": "How hot is it in Phoenix today?", "reply": "Switching the weather check to Phoenix."}),
	)
	_, _, approval := f.parked("hike", "Plan a hike near Tucson.", "Hike near Tucson", map[string]any{"kind": "feedback", "subject": map[string]any{"kind": "text"}, "question": "Which trail?"})
	weatherGoal, weather := f.goal("weather", "What's the weather in Tucson?", "Weather in Tucson", "running")

	_, first, err := f.ask("loop", "The short loop.", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, "Telling the hike plan you want the short loop.", first)
	require.Equal(t, []map[string]any{guidanceAnswer(approval, "The short loop.")}, f.answered, "answered as the person's guidance, never an approval")
	saved := f.turn("loop")
	require.Equal(t, routeAnswered, saved.Routed.Targets[0].Status)

	chunks, again, err := f.ask("loop", "The short loop.", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, first, again)
	require.Equal(t, []string{first}, chunks, "the saved reply is said again")
	require.Len(t, f.model.prompts, 1, "no second routing call")
	require.Len(t, f.answered, 1, "the request is not answered twice")
	require.Equal(t, saved.Routed, f.turn("loop").Routed)

	message := "Actually I'm in Phoenix. How hot is it there?"
	f.intakeAcknowledgement = "I'll look up the heat in Phoenix."
	_, split, err := f.ask("split", message, AskRoute{})
	require.NoError(t, err)
	require.Equal(t, f.intakeAcknowledgement, split)
	require.Equal(t, "queued", f.turn("split").State, "its new goal is still working")
	_, again, err = f.ask("split", message, AskRoute{})
	require.NoError(t, err)
	require.Equal(t, split, again)
	require.Len(t, f.model.prompts, 2, "a resent turn follows its goal and is not routed again")
	require.Equal(t, []string{"How hot is it in Phoenix today?"}, f.intake, "the split request opened one goal")
	require.Equal(t, []WorkGuidance{{ID: "split", Prompt: message}}, f.guidance(weatherGoal, weather), "and the correction reached the goal once")
	require.Empty(t, f.held)
}

// A message that still needs a goal of its own takes the conversation's lock
// again for intake, and works on the conversation as it is then: what routing
// recorded on earlier turns, and what another writer saved while routing ran,
// both stay.
func TestRunAskSplitKeepsWhatOthersWrote(t *testing.T) {
	f := newRouteFixture(t, routeAnswer(map[string]any{"confirmStops": []any{"g1"}, "newRequest": "What's the weather in Phoenix?", "reply": "Stopping the Tucson check."}))
	goal, run := f.goal("weather", "What's the weather in Tucson?", "Weather in Tucson", "running")
	f.record(AskTurn{ID: "drop", Prompt: "Never mind the weather one", Answer: "Stop the weather check?", State: "done",
		Routed: &AskRouting{Stops: []AskStopQuestion{{GoalID: goal, RunID: run, Title: "Weather in Tucson"}}}})
	f.beforeModel = func() {
		// Another turn's observer saves its receipt while routing decides.
		weather := f.turn("weather")
		weather.Answer = "It was 30 C in Tucson."
		require.NoError(t, f.e.askSaveTurn(f.ctx, f.conversation, weather))
	}
	chunks, _, err := f.ask("yes", "Yes, stop it. What's the weather in Phoenix?", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, []string{goal}, f.stopped)
	require.Equal(t, []string{"What's the weather in Phoenix?"}, f.intake)
	require.Equal(t, "Stopping the Tucson check.\n\n", chunks[0])
	require.Equal(t, &AskRouting{Reply: "Stopping the Tucson check.", Statement: "What's the weather in Phoenix?"}, f.turn("yes").Routed)
	require.Equal(t, stopDecisionStopped, f.turn("drop").Routed.Stops[0].Decision, "intake keeps the stop routing recorded")
	require.Equal(t, "It was 30 C in Tucson.", f.turn("weather").Answer, "intake keeps what another writer saved meanwhile")
	require.NotEmpty(t, f.turn("yes").RunID)
}

// A person who leaves once routing has decided still gets what they asked
// for: the decision is applied and recorded whole, and the turn ends done
// with its reply.
func TestRunAskAppliesADecisionThePersonLeftDuring(t *testing.T) {
	f := newRouteFixture(t, routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "")}, "reply": "Telling the hike plan you want the short loop."}))
	goal, run, question := f.parked("hike", "Plan a hike near Tucson.", "Hike near Tucson", map[string]any{"kind": "feedback", "subject": map[string]any{"kind": "text"}, "question": "Which trail?"})
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	answer := f.e.builtinExecutorHandlers["integration.work.decideApproval"]
	f.e.builtinExecutorHandlers["integration.work.decideApproval"] = func(callCtx context.Context, args map[string]any, target int) ([]memorynodes.MemoryNode, error) {
		cancel() // the person's stream drops while the request takes the message
		return answer(callCtx, args, target)
	}
	_, err := f.e.RunAsk(ctx, f.conversation, "loop", "The short loop.", "", AskRoute{}, nil, nil, nil)
	require.NoError(t, err)
	turn := f.turn("loop")
	require.Equal(t, "done", turn.State)
	require.Equal(t, "Telling the hike plan you want the short loop.", turn.Answer)
	require.Equal(t, []AskRouteTarget{{GoalID: goal, RunID: run, Title: "Hike near Tucson", Status: routeAnswered, ApprovalID: question, Text: "The short loop."}}, turn.Routed.Targets)
}

// Routing saves a message that opened no goal of its own as done, with its
// answer, in the same write as what it delivered: a turn whose observer is
// lost after that write is answered from the record when it is resent.
func TestRouteAskTurnSavesTheAnswerWithItsRecord(t *testing.T) {
	f := newRouteFixture(t, routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "Weather in Phoenix")}, "reply": "Switching the weather check to Phoenix."}))
	goal, run := f.goal("weather", "What's the weather in Tucson?", "Weather in Tucson", "running")
	f.record(AskTurn{ID: "phoenix", Prompt: "Actually I'm in Phoenix", State: "streaming"})
	outcome, err := f.e.routeAskTurn(f.ctx, f.conversation, "phoenix", "Actually I'm in Phoenix", common.RouteChoice{}, func(WorkEvent) error { return nil })
	require.NoError(t, err)
	require.False(t, outcome.Intake)
	saved := f.turn("phoenix")
	require.Equal(t, "done", saved.State)
	require.NotNil(t, saved.EndedAt)
	require.Equal(t, "Switching the weather check to Phoenix.", saved.Answer)
	require.Equal(t, outcome.Answer, saved.Answer)
	require.Equal(t, &AskRouting{Targets: []AskRouteTarget{{GoalID: goal, RunID: run, Title: "Weather in Phoenix", Status: routeQueued, Text: "Actually I'm in Phoenix"}}}, saved.Routed)
	require.Equal(t, outcome.Routed, saved.Routed)

	chunks, again, err := f.ask("phoenix", "Actually I'm in Phoenix", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, saved.Answer, again)
	require.Equal(t, []string{again}, chunks)
	require.Len(t, f.model.prompts, 1)
}

// Guidance carries text only and at most 4,000 characters, so a message with
// attachments or a longer one is not routed: it goes to intake as written and
// no model is asked. A message at the limit is routed.
func TestRunAskDoesNotRouteAttachmentsOrLongMessages(t *testing.T) {
	f := newRouteFixture(t, routeAnswer(nil))
	f.goal("weather", "What's the weather in Tucson?", "Weather in Tucson", "running")
	call, err := parser.RenderCall("createArtifact", map[string]any{
		"sourceConceptRef": "v1:library:file:" + id.NewShortId(), "ownerUserId": f.user,
		"lens": "artifact", "kind": "file", "source": "uploaded", "title": "notes.md", "mimeType": "text/markdown",
	})
	require.NoError(t, err)
	result, err := f.e.Execute(auth.ContextWithInternalOrigin(f.ctx), "mutation "+call)
	require.NoError(t, err)
	notes := BareShortId(fmt.Sprint(MaterializeRows(result.OutputPayload())[0]["id"]))

	_, _, err = f.ask("attached", "Read this, it changes the weather one.", AskRoute{}, notes)
	require.NoError(t, err)
	long := strings.Repeat("é", 4001)
	_, _, err = f.ask("long", long, AskRoute{})
	require.NoError(t, err)
	require.Empty(t, f.model.prompts, "neither message is routed")
	require.Equal(t, []string{"Read this, it changes the weather one.", long}, f.intake)
	require.Nil(t, f.turn("attached").Routed)
	require.Nil(t, f.turn("long").Routed)

	// At the limit a message is routed; one the model judges a new request
	// goes to intake untouched, and is no routed message.
	limit := strings.Repeat("é", 4000)
	_, _, err = f.ask("limit", limit, AskRoute{})
	require.NoError(t, err)
	require.Len(t, f.model.prompts, 1)
	require.Equal(t, limit, f.intake[2])
	require.Nil(t, f.turn("limit").Routed)
	require.Empty(t, fallbacks(f.turn("limit")))
}

// A native computer-access request still takes a correction after it expired:
// routing answers it with the message, as guidance and never a grant, rather
// than queueing work guidance its run would not read while it waits. The
// routing call is told the goal waits for the person's permission.
func TestRunAskCorrectsAnExpiredComputerAccessRequest(t *testing.T) {
	f := newRouteFixture(t, routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "")}, "reply": "Telling the cleanup to look only at the PDFs."}))
	scope := map[string]any{"agentId": "agent", "scope": "observe", "summary": "Read the Downloads folder?",
		"nativeScopeCheckpoint": map[string]any{"version": 1, "continuationId": "continuation", "toolCallId": "call", "requestHash": "request"}}
	goal, run, access := f.parked("files", "Tidy my downloads.", "Tidy the downloads", map[string]any{"kind": "scopeElevation", "subject": scope,
		"artifactHash": work.ArtifactHash(scope), "question": "Read the Downloads folder?", "expiresAt": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)})
	_, answer, err := f.ask("pdfs", "Only look at the PDFs.", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, "Telling the cleanup to look only at the PDFs.", answer)
	require.Contains(t, f.model.prompts[0], "State: waiting for your permission: Read the Downloads folder?")
	require.Equal(t, []map[string]any{guidanceAnswer(access, "Only look at the PDFs.")}, f.answered, "answered with guidance, never approved")
	require.Equal(t, []AskRouteTarget{{GoalID: goal, RunID: run, Title: "Tidy the downloads", Status: routeNotApproved, ApprovalID: access, Text: "Only look at the PDFs."}}, f.turn("pdfs").Routed.Targets,
		"delivered through the request, not queued as work guidance")
}

// A question answered through routing already says so on the person's
// message ("To Hike near Tucson · Answered"); the conversation does not show
// it a second time as a "Guidance sent" turn, not even a copy another turn
// saved before the label was recorded. Guidance sent to a request some other
// way still shows.
func TestRunAskAnsweredQuestionIsNotShownTwice(t *testing.T) {
	f := newRouteFixture(t, routeAnswer(map[string]any{"updates": []any{routeUpdate("g2", "")}, "reply": "Telling the hike plan you want the short loop."}))
	hikeGoal, hike, question := f.parked("hike", "Plan a hike near Tucson.", "Hike near Tucson", map[string]any{"kind": "feedback", "subject": map[string]any{"kind": "text"}, "question": "Which trail?"})
	_, _, other := f.parked("camp", "Book a campsite.", "Campsite", map[string]any{"kind": "feedback", "subject": map[string]any{"kind": "text"}, "question": "Which night?"})
	_, _, err := f.ask("loop", "The short loop.", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, []AskRouteTarget{{GoalID: hikeGoal, RunID: hike, Title: "Hike near Tucson", Status: routeAnswered, ApprovalID: question, Text: "The short loop."}}, f.turn("loop").Routed.Targets)
	// Guidance sent to the campsite's question from its request, not from Ask.
	f.write("decideWorkApproval", map[string]any{"approvalId": other, "decision": "answered", "decidedBy": f.user, "decidedAt": time.Now().UTC().Format(time.RFC3339Nano), "answer": map[string]any{"feedback": "Friday."}})

	snapshot := f.snapshot()
	require.NotContains(t, snapshot, "guidance-"+question)
	require.Contains(t, snapshot, "guidance-"+other)
	require.Equal(t, "Guidance sent:\nFriday.", snapshot["guidance-"+other].Prompt)

	f.record(AskTurn{ID: "guidance-" + question, ApprovalID: question, Prompt: "Guidance sent:\nThe short loop.", State: "done", AnswerOnly: true})
	require.NotContains(t, f.snapshot(), "guidance-"+question)
}

// A goal a split message opened asked for what intake received, and routing
// shows it that way; with no title yet, that is its name too.
func TestActiveAskGoalsShowsWhatASplitAsked(t *testing.T) {
	a, _, ctx, conversation, _, write := guidanceFixture(t)
	run := guidanceRootRun(write, conversation, "time-goal", "time-plan", "running")
	goals := listGoals(t, a, ctx, AskTurn{ID: "split", RunID: run, Prompt: "Actually Phoenix. Also, what time is it there?",
		Routed: &AskRouting{Statement: "What time is it in Phoenix right now?"}})
	require.Len(t, goals, 1)
	require.Equal(t, "What time is it in Phoenix right now?", goals[0].Request)
	require.Equal(t, "What time is it in Phoenix right now?", goals[0].Title)
}

// A turn's observer saves its own copy of the turn while its work runs, and
// that copy is older than what others wrote to the turn's routing since: an
// agent acknowledging the message, a person answering its stop question.
// Saving the copy keeps what they wrote.
func TestAskSaveTurnKeepsTheRoutingRecord(t *testing.T) {
	a, b, ctx, conversation, run, _ := guidanceFixture(t)
	observed := AskTurn{ID: "split", Prompt: "Actually Phoenix. Also, never mind the report.", State: "streaming", StartedAt: time.Now().UTC(), Activity: []WorkEvent{}, Routed: &AskRouting{
		Targets: []AskRouteTarget{{GoalID: "fixture-goal", RunID: run, Title: "Weather in Tucson", Status: routeQueued, Text: "Actually Phoenix."}},
		Stops:   []AskStopQuestion{{GoalID: "fixture-goal", RunID: run, Title: "Weather in Tucson"}},
	}}
	row, err := a.askRead(ContextWithFreshRead(ctx), conversation)
	require.NoError(t, err)
	transcript, err := approvalTranscript(row)
	require.NoError(t, err)
	transcript.Turns = append(transcript.Turns, observed)
	require.NoError(t, a.askSave(ctx, conversation, stringArg(row, "title"), transcript))

	runtime := guidanceRunContext(ctx, run)
	read, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.NoError(t, b.AcknowledgeWorkGuidance(runtime, []string{read[0].ID}))
	_, err = b.askStopDecisionBuiltin(ctx, map[string]any{"conversationId": conversation, "turnId": "split", "runId": run, "decision": "keep"}, 0)
	require.NoError(t, err)

	observed.Answer = "It is 3 PM in Phoenix."
	require.NoError(t, a.askSaveTurn(ctx, conversation, observed))
	saved := storedTurn(t, a, ctx, conversation, "split")
	require.Equal(t, "It is 3 PM in Phoenix.", saved.Answer, "the observer's own fields are saved")
	require.Equal(t, routeReceived, saved.Routed.Targets[0].Status, "the acknowledgement stays")
	require.Equal(t, stopDecisionKept, saved.Routed.Stops[0].Decision, "the person's answer stays")

	// A turn with no routing yet takes its observer's.
	plain := AskTurn{ID: "plain", Prompt: "Hi", State: "streaming", StartedAt: time.Now().UTC(), Activity: []WorkEvent{}}
	routeTurn(t, a, ctx, conversation, plain)
	plain.Routed = &AskRouting{Skipped: "invalid"}
	require.NoError(t, a.askSaveTurn(ctx, conversation, plain))
	require.Equal(t, &AskRouting{Skipped: "invalid"}, storedTurn(t, a, ctx, conversation, "plain").Routed)
}
