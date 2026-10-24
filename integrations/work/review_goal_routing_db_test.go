package work

import (
	"context"
	"errors"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

func TestRepositoryReviewGoalRoutingAcrossReplicas(t *testing.T) {
	first, second := openActsDB(t), openActsDB(t)
	for _, mode := range []string{"owner-analysis", "maintainer-review"} {
		t.Run(mode, func(t *testing.T) {
			a, b := freshActsDBPair(t, first, second)
			ctx := actorCtx(a.owner)
			choice := common.RouteChoice{Source: "route:apps", By: a.owner, NoPaidProviders: true}
			origin := memql.ContextWithGoalRouteChoice(ctx, choice)
			if mode == "owner-analysis" {
				// A repository request made inside the owner's Ask run must
				// retain that run's choice just like a direct Ask admission.
				origin = common.ContextWithRun(ctx, common.RunContext{RunId: "parent", GoalId: "parent-goal", OwnerUserId: a.owner, Routing: choice})
			}
			goal := DirectGoal{OwnerUserId: a.owner, Statement: "Review the scoped repository change", AutomationName: "repositoryRecipe", RequestedVia: "pipeline", TriggeredBy: "repository-change"}
			open := func(i *Integration, c context.Context, key string) (ReviewGoalReceipt, error) {
				if mode == "owner-analysis" {
					return i.OpenAnalysisGoal(c, key, goal)
				}
				return i.OpenReviewGoal(c, key, goal, map[string]any{"proposal": "fixed-bytes"}, "Allow this repository change?")
			}
			assertRouting := func(receipt ReviewGoalReceipt) {
				t.Helper()
				read := memql.ContextWithFreshRead(ctx) // no originating Ask/run context on this replica
				row, err := b.i.store().goalForOwner(read, receipt.GoalID)
				if err != nil || common.RouteChoiceFrom(row["routing"]) != choice {
					t.Fatalf("goal lost the owner's Apps choice: %v %v", row["routing"], err)
				}
				run, err := b.i.store().runForOwner(read, receipt.RunID)
				if err != nil || common.RouteChoiceFrom(run["routing"]) != choice {
					t.Fatalf("receiving run lost the owner's Apps choice: %v %v", run["routing"], err)
				}
				peer := common.ContextWithRun(ctx, common.RunContext{RunId: receipt.RunID, GoalId: receipt.GoalID, OwnerUserId: a.owner, StepKey: "review", Routing: common.RouteChoiceFrom(run["routing"])})
				request, err := memql.ApplyRunRouting(peer, airoute.ResolveRequest{Level: airoute.LevelReasoning, Modality: airoute.ModalityStructured, PromptName: "znasRepositoryReview"})
				if err != nil || request.PersonRoute != "apps" || request.PersonRouteOwner != a.owner || !request.NoPaidProviders {
					t.Fatalf("structured reviewer did not inherit the persisted Apps route: %+v %v", request, err)
				}
				if err := memql.CheckAppExecutionDestination(peer, b.eng, receipt.RunID); err != nil {
					t.Fatalf("same-owner Apps consent lost across replicas: %v", err)
				}
				var required *memql.ExecutionDestinationRequired
				if err := memql.CheckExecutionDestination(peer, b.eng, receipt.RunID, "computer"); !errors.As(err, &required) {
					t.Fatalf("Apps route granted native computer tools: %v", err)
				}
			}
			receipt, err := open(a.i, origin, "repository-route")
			if err != nil {
				t.Fatal(err)
			}
			assertRouting(receipt)
			if mode == "maintainer-review" {
				run, err := b.i.store().runForOwner(memql.ContextWithFreshRead(ctx), receipt.RunID)
				if err != nil || rowString(run, "status") != runStatusWaiting || rowString(rowMap(run, "waitingOn"), "subject") != receipt.ApprovalID {
					t.Fatalf("route retention released an unapproved request: %v %v", run, err)
				}
			}
			// Recovery has only durable owner-bound rows, not the first
			// process's context. It must retain, never silently reset, routing.
			if recovered, err := open(b.i, ctx, "repository-route"); err != nil || recovered != receipt {
				t.Fatalf("receiving replica did not recover exact request: %+v %v", recovered, err)
			}
			changed := memql.ContextWithGoalRouteChoice(ctx, common.RouteChoice{Source: "route:local", Destination: "computer", By: a.owner})
			if _, err := open(b.i, changed, "repository-route"); err == nil {
				t.Fatal("repeated request replaced its accepted routing and destination")
			}
			assertRouting(receipt)

			original := a.i.engine
			a.i.engine = reviewGoalWriteFailure{original, "createWorkRun"}
			partial, err := open(a.i, origin, "partial-repository-route")
			a.i.engine = original
			if err == nil {
				t.Fatal("injected run write failure was ignored")
			}
			if recovered, err := open(b.i, ctx, "partial-repository-route"); err != nil || recovered != partial {
				t.Fatalf("partial bootstrap did not recover accepted route: %+v %v", recovered, err)
			}
			assertRouting(partial)
		})
	}
}

func TestRepositoryReviewGoalDoesNotBorrowDestinationConsent(t *testing.T) {
	first, second := openActsDB(t), openActsDB(t)
	for _, tc := range []struct {
		name, by, destination string
	}{
		{name: "another owner's Apps choice", by: "foreign-requester", destination: "computer"},
		{name: "Apps keeps the Workbench boundary", destination: "workbench"},
		{name: "Auto grants no app destination"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := freshActsDBPair(t, first, second)
			by := tc.by
			if by == "" {
				by = a.owner
			}
			choice := common.RouteChoice{Source: "route:apps", By: by, Destination: tc.destination}
			if tc.name == "Auto grants no app destination" {
				choice = common.RouteChoice{}
			}
			ctx := actorCtx(a.owner)
			origin := memql.ContextWithGoalRouteChoice(ctx, choice)
			receipt, err := a.i.OpenAnalysisGoal(origin, "destination-boundary", DirectGoal{OwnerUserId: a.owner, Statement: "Review the repository", AutomationName: "repositoryRecipe", RequestedVia: "pipeline", TriggeredBy: "repository-change"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := b.i.store().runForOwner(memql.ContextWithFreshRead(ctx), receipt.RunID)
			if err != nil {
				t.Fatal(err)
			}
			stored := common.RouteChoiceFrom(run["routing"])
			if tc.by != "" && !stored.IsZero() {
				t.Fatalf("maintainer borrowed requester's choice: %+v", stored)
			}
			if tc.destination == "workbench" && stored != choice {
				t.Fatalf("explicit Workbench boundary was dropped: %+v", stored)
			}
			var required *memql.ExecutionDestinationRequired
			if err := memql.CheckAppExecutionDestination(ctx, b.eng, receipt.RunID); !errors.As(err, &required) {
				t.Fatalf("Apps execution bypassed its destination decision: %v", err)
			}
			if err := memql.CheckExecutionDestination(ctx, b.eng, receipt.RunID, "computer"); !errors.As(err, &required) {
				t.Fatalf("native computer execution borrowed destination consent: %v", err)
			}
		})
	}
}
