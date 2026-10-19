package memql

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
)

type stopFixtureState struct {
	a, b              *MemQLEngine
	ctx               context.Context
	conversation, run string
	write             func(string, map[string]any)
	cancelled         []map[string]any
}

// stopFixture is a conversation in which Ask asked whether to stop a running
// goal. work.cancelGoal is stubbed on the first replica and records each call.
func stopFixture(t *testing.T) *stopFixtureState {
	t.Helper()
	a, b, ctx, conversation, run, write := guidanceFixture(t)
	f := &stopFixtureState{a: a, b: b, ctx: ctx, conversation: conversation, run: run, write: write}
	routeTurn(t, a, ctx, conversation, AskTurn{ID: "drop", Prompt: "Never mind the weather one.", Routed: &AskRouting{
		Stops: []AskStopQuestion{{GoalID: "fixture-goal", RunID: run, Title: "Weather in Tucson"}},
		Reply: "Stop Weather in Tucson? Confirm on its card.",
	}})
	a.builtinExecutorHandlers["integration.work.cancelGoal"] = func(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		f.cancelled = append(f.cancelled, args)
		return askValue(map[string]any{"goalId": args["goalId"], "runsAsked": 1, "goalClosed": true})
	}
	return f
}

// decide answers a stop question through the public DSL ingress, as the OS does.
func (f *stopFixtureState) decide(e *MemQLEngine, ctx context.Context, turnID, runID, decision string) (map[string]any, error) {
	call, err := parser.RenderCall("askStopDecision", map[string]any{"conversationId": f.conversation, "turnId": turnID, "runId": runID, "decision": decision})
	if err != nil {
		return nil, err
	}
	result, err := e.Execute(ctx, "builtin "+call)
	if err != nil {
		return nil, err
	}
	rows := MaterializeRows(result.OutputPayload())
	if len(rows) != 1 {
		return nil, fmt.Errorf("askStopDecision returned %d rows", len(rows))
	}
	// The row's own fields, without the node envelope every builtin result carries.
	row := map[string]any{}
	for _, key := range []string{"conversationId", "turnId", "runId", "decision"} {
		row[key] = rows[0][key]
	}
	return row, nil
}

func (f *stopFixtureState) savedDecision(t *testing.T, turnID string, n int) string {
	t.Helper()
	return storedTurn(t, f.a, f.ctx, f.conversation, turnID).Routed.Stops[n].Decision
}

func TestAskStopDecisionStopsTheGoal(t *testing.T) {
	f := stopFixture(t)
	row, err := f.decide(f.a, f.ctx, "drop", f.run, "stop")
	require.NoError(t, err)
	require.Equal(t, map[string]any{"conversationId": BareShortId(f.conversation), "turnId": "drop", "runId": BareShortId(f.run), "decision": "stopped"}, row)
	require.Len(t, f.cancelled, 1)
	require.Equal(t, "fixture-goal", BareShortId(fmt.Sprint(f.cancelled[0]["goalId"])))
	require.Equal(t, "Stopped from Ask.", f.cancelled[0]["reason"])
	require.Equal(t, stopDecisionStopped, f.savedDecision(t, "drop", 0))
	// A model never stops a person's goal: the builtin is not an agent tool.
	require.False(t, f.a.workCapabilityAllowed(f.ctx, &Function{Name: "askStopDecision", Enabled: true, FunctionKind: "builtin", Origin: "os/ask.memql"}))
}

// Stop is recorded only once the goal is closed and every run took the request.
func TestAskStopDecisionRecordsNothingWhenTheGoalDoesNotStop(t *testing.T) {
	f := stopFixture(t)
	for _, reply := range []map[string]any{
		{"goalId": "fixture-goal", "runsAsked": 1, "goalClosed": false},
		{"goalId": "fixture-goal", "runsAsked": 0, "goalClosed": true, "runsRefused": []string{f.run}},
	} {
		f.a.builtinExecutorHandlers["integration.work.cancelGoal"] = func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
			return askValue(reply)
		}
		_, err := f.decide(f.a, f.ctx, "drop", f.run, "stop")
		require.Error(t, err, "%v", reply)
		require.Empty(t, f.savedDecision(t, "drop", 0))
	}
	f.a.builtinExecutorHandlers["integration.work.cancelGoal"] = func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
		return nil, fmt.Errorf("work: no goal is readable by this caller")
	}
	_, err := f.decide(f.a, f.ctx, "drop", f.run, "stop")
	require.Error(t, err)
	require.Empty(t, f.savedDecision(t, "drop", 0))
}

func TestAskStopDecisionKeeps(t *testing.T) {
	f := stopFixture(t)
	row, err := f.decide(f.a, f.ctx, "drop", f.run, "keep")
	require.NoError(t, err)
	require.Equal(t, "kept", row["decision"])
	require.Empty(t, f.cancelled, "keeping a goal changes nothing about it")
	require.Equal(t, stopDecisionKept, f.savedDecision(t, "drop", 0))
}

func TestAskStopDecisionIsIdempotentAndRefusesAChange(t *testing.T) {
	f := stopFixture(t)
	first, err := f.decide(f.a, f.ctx, "drop", f.run, "stop")
	require.NoError(t, err)
	// A retry on another replica, holding no stub, returns the saved answer
	// without asking the goal to stop a second time.
	again, err := f.decide(f.b, f.ctx, "drop", f.run, "stop")
	require.NoError(t, err)
	require.Equal(t, first, again)
	require.Len(t, f.cancelled, 1)
	_, err = f.decide(f.b, f.ctx, "drop", f.run, "keep")
	require.ErrorContains(t, err, "already decided")
	require.Equal(t, stopDecisionStopped, f.savedDecision(t, "drop", 0))

	// Kept first: a later stop is refused before anything is cancelled.
	hike := guidanceRootRun(f.write, f.conversation, "hike-goal", "hike-plan", "running")
	routeTurn(t, f.a, f.ctx, f.conversation, AskTurn{ID: "drop-hike", Prompt: "Drop the hike too?", Routed: &AskRouting{
		Stops: []AskStopQuestion{{GoalID: "hike-goal", RunID: hike, Title: "Hike near Tucson"}},
	}})
	kept, err := f.decide(f.a, f.ctx, "drop-hike", hike, "keep")
	require.NoError(t, err)
	require.Equal(t, "kept", kept["decision"])
	_, err = f.decide(f.a, f.ctx, "drop-hike", hike, "stop")
	require.ErrorContains(t, err, "already decided")
	require.Len(t, f.cancelled, 1)
	require.Equal(t, stopDecisionKept, f.savedDecision(t, "drop-hike", 0))
}

func TestAskStopDecisionRefusesInsideARun(t *testing.T) {
	f := stopFixture(t)
	actor, _ := auth.AccessFromContext(f.ctx)
	inside := common.ContextWithRun(f.ctx, common.RunContext{RunId: f.run, GoalId: "fixture-goal", OwnerUserId: actor.UserId, StepKey: "section"})
	_, err := f.decide(f.a, inside, "drop", f.run, "stop")
	require.ErrorContains(t, err, "person")
	_, err = f.a.askStopDecisionBuiltin(inside, map[string]any{"conversationId": f.conversation, "turnId": "drop", "runId": f.run, "decision": "keep"}, 0)
	require.ErrorContains(t, err, "person")
	require.Empty(t, f.cancelled)
	require.Empty(t, f.savedDecision(t, "drop", 0))
}

func TestAskStopDecisionRefusesWhatItCannotDecide(t *testing.T) {
	f := stopFixture(t)
	refused := []struct {
		name                string
		ctx                 context.Context
		turn, run, decision string
	}{
		{"signed out", context.Background(), "drop", f.run, "stop"},
		{"another person's conversation", askTestActor(), "drop", f.run, "stop"},
		{"a turn with no stop question", f.ctx, "original", f.run, "stop"},
		{"a goal it did not ask about", f.ctx, "drop", "another-run", "stop"},
		{"neither stop nor keep", f.ctx, "drop", f.run, "pause"},
	}
	for _, c := range refused {
		_, err := f.decide(f.a, c.ctx, c.turn, c.run, c.decision)
		require.Error(t, err, c.name)
		_, err = f.a.askStopDecisionBuiltin(c.ctx, map[string]any{"conversationId": f.conversation, "turnId": c.turn, "runId": c.run, "decision": c.decision}, 0)
		require.Error(t, err, c.name)
	}
	require.Empty(t, f.cancelled)
	require.Empty(t, f.savedDecision(t, "drop", 0))
}
