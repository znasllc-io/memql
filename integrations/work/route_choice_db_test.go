package work

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// THE HOP, with nothing but the row between the nodes (CLAUDE.md: a
// single-node test is a false signal for cross-node behaviour).
//
// An Ask turn's choice arrives on the node that takes the turn. The goal is
// compiled on a PLANNER that receives only the run's graph event, and its
// steps run on an AGENT that loads only the run's journal. The context the
// turn arrived with is cancelled before either reads anything, so the one
// thing that can carry the choice across is the row -- which is the claim.
func TestRouteChoiceDB_TheTurnsChoiceReachesThePlannerAndTheAgentFromTheRow(t *testing.T) {
	t.Setenv("MEMQL_NODE_ID", "planner-receiver")
	_, bff, planners, probe := compileDB(t)
	eng := bff.engine.(*memql.MemQLEngine)
	bus := events.NewBus()
	eng.SetEventBus(bus)
	for _, topic := range []string{"graph.node.created.v1:work:run", "graph.node.updated.v1:work:run"} {
		for _, planner := range planners {
			unsub := bus.Subscribe(topic, planner.HandleRunEvent)
			defer unsub()
		}
	}

	// The turn: the choice rides beside the request on the bff, never in the
	// input the model will read.
	turn := memql.ContextWithGoalRouteChoice(actorCtx("compile-alice"), common.RouteChoice{Source: "app:claude-code", Level: "strong", By: "compile-alice"})
	ctx, cancel := context.WithCancel(turn)
	nodes, err := bff.handleCreateGoal(ctx, map[string]any{
		"statement": "hi", "requestedVia": "ask",
		"input": map[string]any{"conversation": map[string]any{"id": "c1", "turnId": "t1"}},
	}, 0)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	reply := decodeReply(t, nodes)
	want := common.RouteChoice{Source: "app:claude-code", Level: "strong", By: "compile-alice"}

	// The planner: triage and the compile pass see the choice on the run
	// context it built from the row it claimed.
	got := awaitCompile(t, probe)
	if got.run.RunId != reply["runId"] || got.run.Routing != want {
		t.Fatalf("the planner's run context carries routing %+v, want %+v", got.run.Routing, want)
	}
	if _, leaked := got.request.Input["routing"]; leaked || strings.Contains(fmt.Sprint(got.request.Input), "claude-code") {
		t.Fatalf("the choice leaked into the goal input the model reads: %v", got.request.Input)
	}
	finishCompile(t, probe)

	// The agent: the reply step's context is built from the journal alone.
	j, err := automations.LoadRunJournal(context.Background(), eng, reply["runId"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if j.Routing != want {
		t.Fatalf("the agent's journal carries routing %+v, want %+v", j.Routing, want)
	}
}
