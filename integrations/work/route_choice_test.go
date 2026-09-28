package work

// The owner's routing choice (the Ask route picker's) rides the goal and run
// ROWS: every node that later executes the run reads it from there, because it
// holds nothing of the node that took the turn. These tests pin the writes.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

func TestCreateGoalWritesTheRequestsRouteChoiceOnTheGoalAndItsRun(t *testing.T) {
	i, eng := newTestIntegration(t)
	// The choice arrives beside the request, never as an argument.
	ctx := memql.ContextWithGoalRouteChoice(callerContext("u-alice"),
		common.RouteChoice{Source: "app:claude-code", Level: "strong", By: "u-alice"})
	input := map[string]any{"conversation": map[string]any{"id": "c1", "turnId": "t1"}}
	if _, err := i.handleCreateGoal(ctx, map[string]any{"statement": "hi", "requestedVia": "ask", "input": input}, 0); err != nil {
		t.Fatalf("createGoal: %v", err)
	}
	want := map[string]any{"source": "app:claude-code", "level": "strong", "by": "u-alice"}
	for _, name := range []string{"createWorkGoal", "createWorkRun"} {
		args := eng.callTo(t, name).Args(t)
		if !reflect.DeepEqual(args["routing"], want) {
			t.Errorf("%s routing = %v, want %v", name, args["routing"], want)
		}
		// NOT in the input: the input is what the model reads, and which model
		// reads it is not the model's business.
		if strings.Contains(fmt.Sprint(args["input"]), "claude-code") {
			t.Errorf("%s leaked the choice into the goal input: %v", name, args["input"])
		}
	}
}

func TestCreateGoalWithNoChoiceOfItsOwnersWritesNoRouting(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"no choice": callerContext("u-alice"),
		// A choice somebody else made is not the owner's, whatever context it
		// travelled on.
		"another person's choice": memql.ContextWithGoalRouteChoice(callerContext("u-alice"),
			common.RouteChoice{Source: "app:claude-code", By: "u-mallory"}),
	} {
		i, eng := newTestIntegration(t)
		if _, err := i.handleCreateGoal(ctx, map[string]any{"statement": "hi"}, 0); err != nil {
			t.Fatalf("%s: createGoal: %v", name, err)
		}
		for _, call := range []string{"createWorkGoal", "createWorkRun"} {
			if got, present := eng.callTo(t, call).Args(t)["routing"]; present {
				t.Errorf("%s: %s carries routing %v for a goal its owner chose nothing for", name, call, got)
			}
		}
	}
}

// ContextWithGoalRouteChoice is an exported seam, so createGoal re-checks the
// grammar rather than trusting whoever stamped it -- before any row exists.
func TestCreateGoalRefusesAChoiceOutsideTheGrammarBeforeWriting(t *testing.T) {
	i, eng := newTestIntegration(t)
	ctx := memql.ContextWithGoalRouteChoice(callerContext("u-alice"), common.RouteChoice{Source: "vendor:anything", By: "u-alice"})
	_, err := i.handleCreateGoal(ctx, map[string]any{"statement": "hi"}, 0)
	if err == nil || !strings.Contains(err.Error(), memql.RouteSourceInvalid) {
		t.Fatalf("err = %v, want a route_source_invalid refusal", err)
	}
	if got := eng.summary(); got != "(none)" {
		t.Errorf("rows were written before the choice was checked: %s", got)
	}
}

// A goal one of a run's steps opens -- a compose, an agent() -- is part of the
// work the person chose a route for, so it carries that run's choice. Only for
// the same person.
func TestADirectGoalOpenedByTheOwnersRunCarriesItsChoice(t *testing.T) {
	stepCtx := func(owner string) context.Context {
		return common.ContextWithRun(callerContext(owner), common.RunContext{
			RunId: "v1:work:run:parent", GoalId: "v1:work:goal:parent", OwnerUserId: owner, StepKey: "reason",
			Routing: common.RouteChoice{Source: "fleet:strongest", Level: "reasoning", By: "u-alice"},
		})
	}
	i, eng := newTestIntegration(t)
	if _, _, err := i.OpenDirectGoal(stepCtx("u-alice"), DirectGoal{OwnerUserId: "u-alice", Statement: "compose it", AutomationName: "materializeFile"}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"source": "fleet:strongest", "level": "reasoning", "by": "u-alice"}
	for _, name := range []string{"createWorkGoal", "createWorkRun"} {
		if got := eng.callTo(t, name).Args(t)["routing"]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s routing = %v, want the parent run's %v", name, got, want)
		}
	}

	i, eng = newTestIntegration(t)
	if _, _, err := i.OpenDirectGoal(stepCtx("u-alice"), DirectGoal{OwnerUserId: "u-bob", Statement: "for bob", AutomationName: "materializeFile"}); err != nil {
		t.Fatal(err)
	}
	if got, present := eng.callTo(t, "createWorkRun").Args(t)["routing"]; present {
		t.Errorf("a goal for somebody else inherited %v", got)
	}
}

// A fork, a replay and a branch continue the SAME work, and a replay must ask
// the same questions its journal answered -- so each keeps its source's choice.
func TestADerivedRunKeepsItsSourcesChoice(t *testing.T) {
	routing := map[string]any{"source": "policy:localFirst", "level": "fast", "by": "u-alice"}
	for _, derive := range []string{"fork", "replay"} {
		i, eng := newTestIntegration(t)
		row := sourceRun("u-alice")
		row["routing"] = routing
		eng.reply("workRunForOwner", row)
		var err error
		if derive == "fork" {
			_, err = i.handleForkRun(callerContext("u-alice"), map[string]any{"runId": "v1:work:run:r-source", "atStepKey": "step-4"}, 0)
		} else {
			_, err = i.handleReplayRun(callerContext("u-alice"), map[string]any{"runId": "v1:work:run:r-source"}, 0)
		}
		if err != nil {
			t.Fatalf("%s: %v", derive, err)
		}
		if got := eng.callTo(t, "createWorkRun").Args(t)["routing"]; !reflect.DeepEqual(got, routing) {
			t.Errorf("%s routing = %v, want the source's %v", derive, got, routing)
		}
	}

	i, eng, store := newActsIntegration(t)
	addPristineRun(store, actRunId, "fetch", "draft", "publish")
	row := actRunRow(runStatusSucceeded)
	row["routing"] = routing
	eng.reply("workRunForOwner", row)
	if _, err := i.handleBranchRun(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "draft"}, 0); err != nil {
		t.Fatalf("branch: %v", err)
	}
	if got := eng.callTo(t, "createWorkRun").Args(t)["routing"]; !reflect.DeepEqual(got, routing) {
		t.Errorf("branch routing = %v, want the source's %v", got, routing)
	}
}
