package work

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

func TestExecutionDestinationDBAcrossReplicas(t *testing.T) {
	first, second := openActsDB(t), openActsDB(t)
	for _, tc := range []struct {
		name string
		run  func(*testing.T, actsDB, actsDB)
	}{
		{"ExecutionDestinationDB_AnotherReplicaRequiresAndReadsTheDecision", testExecutionDestinationDB_AnotherReplicaRequiresAndReadsTheDecision},
		{"ExecutionDestinationDBWorkbenchRequestParksAndReadsSiblingDecision", testExecutionDestinationDBWorkbenchRequestParksAndReadsSiblingDecision},
		{"AppsAutoRetainsComputerToolBoundary", testAppsAutoRetainsComputerToolBoundary},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, first, second) })
	}
}

func testAppsAutoRetainsComputerToolBoundary(t *testing.T, first, second actsDB) {
	a, b := freshActsDBPair(t, first, second)
	b.owner = a.owner
	for _, tc := range []struct {
		name, source, destination, by string
		allow                         bool
	}{
		{"owner selected Apps and Auto", "route:apps", "auto", a.owner, true},
		{"owner selected Apps and implicit Auto", "route:apps", "", a.owner, true},
		{"Workbench remains a boundary", "route:apps", "workbench", a.owner, false},
		{"Auto route grants no app destination", "", "auto", a.owner, false},
		{"another person's choice grants nothing", "route:apps", "auto", "stranger", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := a.openRun(t, map[string]any{"routing": map[string]any{"source": tc.source, "destination": tc.destination, "by": tc.by}})
			ctx := actorCtx(a.owner)
			if err := memql.CheckAppExecutionDestination(ctx, b.eng, run); (err == nil) != tc.allow {
				t.Fatalf("app admission = %v, want allowed %v", err, tc.allow)
			}
			if err := memql.CheckExecutionDestination(ctx, b.eng, run, "computer"); err == nil {
				t.Fatal("app route granted native computer tools")
			}
			if err := memql.CheckAppExecutionDestination(actorCtx("stranger"), b.eng, run); err == nil {
				t.Fatal("another person borrowed app consent")
			}
			a.write(t, "updateWorkRun", map[string]any{"runId": run, "status": "cancelled"})
			if err := memql.CheckAppExecutionDestination(ctx, b.eng, run); err == nil {
				t.Fatal("cancelled work opened an app")
			}
		})
	}
}

func testExecutionDestinationDB_AnotherReplicaRequiresAndReadsTheDecision(t *testing.T, first, second actsDB) {
	a, b := freshActsDBPair(t, first, second)
	b.owner = a.owner
	for _, destination := range []string{"workbench", "computer", "auto"} {
		t.Run(destination, func(t *testing.T) {
			runID := a.openRun(t, map[string]any{"routing": map[string]any{"destination": destination, "by": a.owner}})
			target := "computer"
			if destination == "computer" {
				target = "workbench"
			}
			// No originating request context is carried onto this replica.
			ctx := actorCtx(a.owner)
			var required *memql.ExecutionDestinationRequired
			if err := b.i.CheckExecutionDestination(ctx, runID, target); !errors.As(err, &required) {
				t.Fatalf("missing decision: %v", err)
			}
			subject := map[string]any{"executionDestination": required.Subject(), "summary": "Use the other destination because a required capability is unavailable"}
			approval := newRowId(approvalConcept)
			a.write(t, "createWorkApproval", map[string]any{"approvalId": approval, "runId": runID, "kind": "scopeElevation", "subject": subject, "artifactHash": workstate.ArtifactHash(subject), "requestedAt": rfc(time.Now()), "expiresAt": rfc(time.Now().Add(time.Hour))})
			if err := b.i.CheckExecutionDestination(ctx, runID, target); !errors.As(err, &required) {
				t.Fatalf("pending decision admitted: %v", err)
			}
			a.write(t, "decideWorkApproval", map[string]any{"approvalId": approval, "decision": "approved", "decidedBy": a.owner, "decidedAt": rfc(time.Now())})
			if err := b.i.CheckExecutionDestination(ctx, runID, target); err != nil {
				t.Fatalf("fresh approved receipt on sibling: %v", err)
			}
			if err := b.i.CheckExecutionDestination(actorCtx("stranger"), runID, target); err == nil {
				t.Fatal("stranger borrowed destination approval")
			}
			other := a.openRun(t, map[string]any{"routing": map[string]any{"destination": destination, "by": a.owner}})
			if err := b.i.CheckExecutionDestination(ctx, other, target); !errors.As(err, &required) {
				t.Fatalf("another run borrowed approval: %v", err)
			}
		})
	}
}

func TestExecutionDestinationAcceptedOnBothRowsAndInheritedByChildren(t *testing.T) {
	i, eng := newTestIntegration(t)
	ctx := callerContext("u-alice")
	if _, err := i.handleCreateGoal(ctx, map[string]any{"statement": "Build the report", "destination": "computer"}, 0); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"createWorkGoal", "createWorkRun"} {
		routing := common.RouteChoiceFrom(eng.callTo(t, name).Args(t)["routing"])
		if routing.Destination != "computer" || routing.By != "u-alice" {
			t.Fatalf("%s: %+v", name, routing)
		}
	}
	i, eng = newTestIntegration(t)
	ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "parent", GoalId: "goal", OwnerUserId: "u-alice", Routing: common.RouteChoice{Destination: "computer", By: "u-alice"}})
	if _, err := i.handleCreateGoal(ctx, map[string]any{"statement": "Child", "destination": "auto"}, 0); err == nil {
		t.Fatal("child relaxed destination")
	}
	if eng.summary() != "(none)" {
		t.Fatal("rows written before refusal")
	}
	if _, err := i.handleCreateGoal(ctx, map[string]any{"statement": "Child"}, 0); err != nil {
		t.Fatal(err)
	}
	if common.RouteChoiceFrom(eng.callTo(t, "createWorkRun").Args(t)["routing"]).Destination != "computer" {
		t.Fatal("child lost destination")
	}
}

func testExecutionDestinationDBWorkbenchRequestParksAndReadsSiblingDecision(t *testing.T, first, second actsDB) {
	for _, decision := range []string{"approved", "rejected"} {
		t.Run(decision, func(t *testing.T) {
			a, b := freshActsDBPair(t, first, second)
			b.owner = a.owner
			runID := a.openRun(t, map[string]any{"routing": map[string]any{"destination": "computer", "by": a.owner}})
			person := actorCtx(a.owner)
			row, err := a.i.store().runForOwner(memql.ContextWithFreshRead(person), runID)
			if err != nil {
				t.Fatal(err)
			}
			ctx := common.ContextWithRun(person, common.RunContext{RunId: runID, GoalId: rowString(row, "goalId"), OwnerUserId: a.owner, StepKey: "reason"})
			a.writeVersion(t, runID, runID, "reason", 0, 1, nil, "", map[string]any{"status": "running"})
			reason := "The selected computer is disconnected"
			raw, _ := json.Marshal(map[string]any{"reason": reason})
			saved := []common.ChatMessage{{Role: "system", Content: "Instructions"}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: "done", Name: "composeFile"}, {ID: "move", Name: "requestWorkbenchUse", Arguments: string(raw)}}}, {Role: "tool", ToolCallId: "done", Name: "composeFile", Content: `{"outputFileId":"already-written"}`}, {Role: "tool", ToolCallId: "move", Name: "requestWorkbenchUse", Content: common.UnexecutedWorkToolResult}}
			if err := a.eng.SaveWorkContinuation(ctx, saved); err != nil {
				t.Fatal(err)
			}
			nodes, err := a.i.handleRequestWorkbenchUse(ctx, map[string]any{"reason": "The selected computer is disconnected"}, 0)
			if err != nil {
				t.Fatal(err)
			}
			receipt := decodeReply(t, nodes)
			if receipt["status"] != "awaiting_user" {
				t.Fatalf("permission was not requested: %+v", receipt)
			}
			approval := receipt["approvalId"].(string)
			approvals := a.query(t, a.owner, "query "+call("workApprovalForOwner", map[string]any{"approvalId": approval}))
			if len(approvals) != 1 || rowMap(rowMap(approvals[0], "subject"), "nativeScopeCheckpoint") == nil {
				t.Fatal("destination lost its saved tool checkpoint")
			}

			row, err = b.i.store().runForOwner(memql.ContextWithFreshRead(person), runID)
			if err != nil || rowString(row, "status") != "waiting" {
				t.Fatalf("not parked: %v %v", row, err)
			}
			if _, err = b.i.handleDecideApproval(person, map[string]any{"approvalId": approval, "decision": decision}, 0); err != nil {
				t.Fatal(err)
			}
			if decision == "approved" {
				current, readErr := b.i.store().runForOwner(memql.ContextWithFreshRead(person), runID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				restored, restoreErr := b.eng.RestoreWorkContinuation(ctx, nil)
				if restoreErr != nil || len(restored) != len(saved) || restored[2].Content != saved[2].Content || rowString(rowMap(current, "rerun"), "requestId") != "" {
					t.Fatalf("approval discarded completed effects: %v %v %+v", restored, restoreErr, current)
				}
			}
			nodes, err = a.i.handleRequestWorkbenchUse(ctx, map[string]any{"reason": "The selected computer is disconnected"}, 0)
			if decision == "rejected" {
				if err == nil {
					t.Fatal("declined destination was offered again")
				}
			} else if err != nil || decodeReply(t, nodes)["status"] != "approved" {
				t.Fatalf("did not resume with permission: %v %v", nodes, err)
			}
		})
	}
}
