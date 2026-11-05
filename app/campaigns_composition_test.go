package app

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/campaigns"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

// campaigns_composition_test.go -- what starting, cancelling and retrying an
// email composition takes from the work runtime and the app doors, measured at
// the adapters, and their wiring on every node.

// openedGoal is one direct goal the starter opened, with the route choice the
// goal would carry as the work spine reads it off the context.
type openedGoal struct {
	goal   workspine.DirectGoal
	choice common.RouteChoice
	actor  string
}

// recordingGoalOpener stands in for the work integration's OpenDirectGoal.
type recordingGoalOpener struct {
	mu     sync.Mutex
	opened []openedGoal
	goalID string
	err    error
}

func (o *recordingGoalOpener) OpenDirectGoal(ctx context.Context, g workspine.DirectGoal) (string, string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	opened := openedGoal{goal: g, choice: memqlengine.GoalRouteChoice(ctx, g.OwnerUserId)}
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		opened.actor = ac.UserId
	}
	o.opened = append(o.opened, opened)
	return o.goalID, "v1:work:run:composition-run", o.err
}

func TestCompositionGoalStarterUsesAppsRouteChosenByOwner(t *testing.T) {
	const owner = "v1:identity:user:composer-1"
	opener := &recordingGoalOpener{goalID: "v1:work:goal:composition-1"}
	starter := &compositionGoalStarter{opener: func() compositionGoalOpener { return opener }}

	// The person asks from a conversation on another route, inside a run of
	// their own: the composition's goal is on the Apps route all the same.
	ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), owner), common.RunContext{
		RunId: "v1:work:run:ask-1", GoalId: "v1:work:goal:ask-1", OwnerUserId: owner,
		Routing: common.RouteChoice{Source: "route:local", Level: "fast", By: owner},
	})
	ctx = memqlengine.ContextWithGoalRouteChoice(ctx, common.RouteChoice{Source: "fleet:fastest", Level: "fast", By: owner})
	statement := `Write the email "Spring news" from the Article layout`
	goal, err := starter.StartComposition(ctx, campaigns.CompositionGoal{
		OwnerUserID: owner, AccountID: "org-1", TemplateID: "template-1", Statement: statement,
	})
	if err != nil || goal != "v1:work:goal:composition-1" {
		t.Fatalf("StartComposition = %q, %v", goal, err)
	}
	if len(opener.opened) != 1 {
		t.Fatalf("%d goals opened", len(opener.opened))
	}
	got := opener.opened[0]
	if got.choice.Source != "route:apps" || got.choice.Level != "strong" || got.choice.By != owner {
		t.Errorf("route choice = %+v, want route:apps at strong, chosen by the owner", got.choice)
	}
	// What the work spine re-checks it with: the picker's own vocabulary.
	if _, err := memqlengine.ParseRouteChoice(got.choice.Source, got.choice.Level); err != nil {
		t.Errorf("the choice does not parse: %v", err)
	}
	g := got.goal
	if g.AutomationName != "composeEmailRun" || g.RequestedVia != "email" {
		t.Errorf("goal runs %q requested via %q", g.AutomationName, g.RequestedVia)
	}
	if g.OwnerUserId != owner || g.Statement != statement || got.actor != owner {
		t.Errorf("goal owner %q statement %q, opened as %q", g.OwnerUserId, g.Statement, got.actor)
	}
	if !reflect.DeepEqual(g.AccountIds, []string{"org-1"}) || !reflect.DeepEqual(g.Input, map[string]any{"templateId": "template-1"}) {
		t.Errorf("goal accounts %v input %v", g.AccountIds, g.Input)
	}

	// The opener's refusal is the starter's.
	opener.err, opener.goalID = errors.New("work: the goal could not be written"), ""
	if _, err := starter.StartComposition(ctx, campaigns.CompositionGoal{OwnerUserID: owner, AccountID: "org-1", TemplateID: "template-1", Statement: statement}); err == nil || !strings.Contains(err.Error(), "could not be written") {
		t.Errorf("a refused goal: err = %v", err)
	}
	// A node with no work runtime opens nothing, and says so.
	none := &compositionGoalStarter{opener: func() compositionGoalOpener { return nil }}
	if _, err := none.StartComposition(ctx, campaigns.CompositionGoal{OwnerUserID: owner, AccountID: "org-1", TemplateID: "template-1", Statement: statement}); err == nil {
		t.Error("a starter with no work runtime answered a goal")
	}
}

// fakeAppDoors stands in for the provider registry's AppDoors.
type fakeAppDoors struct {
	doors []memqlengine.AppDoor
	err   error
	asked []string
}

func (f *fakeAppDoors) AppDoors(_ context.Context, actingUserId string) ([]memqlengine.AppDoor, error) {
	f.asked = append(f.asked, actingUserId)
	return f.doors, f.err
}

func TestCompositionAppReachabilityCountsAnyReachableDoor(t *testing.T) {
	const owner = "v1:identity:user:composer-1"
	door := func(machines ...memqlengine.AppMachine) memqlengine.AppDoor {
		return memqlengine.AppDoor{AppId: "claude-code", Machines: machines}
	}
	for _, tc := range []struct {
		name  string
		doors []memqlengine.AppDoor
		want  bool
	}{
		{"no machines", nil, false},
		{"a machine that is off", []memqlengine.AppDoor{door(memqlengine.AppMachine{Online: false, LocalStream: true})}, false},
		{"online but held nowhere", []memqlengine.AppDoor{door(memqlengine.AppMachine{Online: true})}, false},
		{"held by this replica", []memqlengine.AppDoor{door(memqlengine.AppMachine{Online: true, LocalStream: true})}, true},
		// What a bff sees: the agent holding the machine is reached by a
		// forward. Reachable, never Runnable there, and enough.
		{"held by another agent", []memqlengine.AppDoor{door(memqlengine.AppMachine{Online: true, Forwardable: true})}, true},
		{"one of several", []memqlengine.AppDoor{door(memqlengine.AppMachine{Online: false}), door(memqlengine.AppMachine{Online: true, Forwardable: true})}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lister := &fakeAppDoors{doors: tc.doors}
			reach := &compositionAppReachability{doors: func() compositionAppDoors { return lister }}
			got, err := reach.HasReachableApp(context.Background(), owner)
			if err != nil || got != tc.want {
				t.Fatalf("HasReachableApp = %v, %v; want %v", got, err, tc.want)
			}
			if len(lister.asked) != 1 || lister.asked[0] != owner {
				t.Errorf("asked about %v, want the person", lister.asked)
			}
		})
	}
	failing := &compositionAppReachability{doors: func() compositionAppDoors { return &fakeAppDoors{err: errors.New("the fleet cannot be read")} }}
	if ok, err := failing.HasReachableApp(context.Background(), owner); ok || err == nil {
		t.Errorf("a failed read = %v, %v", ok, err)
	}
	none := &compositionAppReachability{doors: func() compositionAppDoors { return nil }}
	if ok, err := none.HasReachableApp(context.Background(), owner); ok || err != nil {
		t.Errorf("no app doors on this node = %v, %v", ok, err)
	}
}

func TestCompositionGoalCancellerAsksTheWorkCancel(t *testing.T) {
	const owner = "v1:identity:user:composer-1"
	var (
		asked []map[string]any
		actor string
	)
	handler := procedureCapability(func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		asked = append(asked, args)
		if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
			actor = ac.UserId
		}
		return nil, nil
	})
	canceller := &compositionGoalCanceller{cancel: func() procedureCapability { return handler }}
	if err := canceller.CancelGoal(auth.ContextWithUserActor(context.Background(), owner), "v1:work:goal:g-1", "cancelled by person"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(asked, []map[string]any{{"goalId": "v1:work:goal:g-1", "reason": "cancelled by person"}}) || actor != owner {
		t.Errorf("asked %v as %q", asked, actor)
	}
	refusing := &compositionGoalCanceller{cancel: func() procedureCapability {
		return func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
			return nil, errors.New("work: no goal is readable by this caller")
		}
	}}
	if err := refusing.CancelGoal(context.Background(), "v1:work:goal:g-1", "cancelled by person"); err == nil || !strings.Contains(err.Error(), "readable") {
		t.Errorf("a refused cancel: err = %v", err)
	}
	none := &compositionGoalCanceller{cancel: func() procedureCapability { return nil }}
	if err := none.CancelGoal(context.Background(), "v1:work:goal:g-1", "cancelled by person"); err == nil {
		t.Error("a canceller with no work runtime cancelled something")
	}
}

// The seams reach the real work runtime and the engine's app doors, resolved
// when a call is made: on this engine the work integration is registered after
// the seams are made, as integrationsCore registers it after engineAndBus wires
// the campaigns worker.
func TestCampaignCompositionSeamsReachTheWorkRuntime(t *testing.T) {
	engine, err := memqlengine.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	if err := engine.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	a := &App{engine: engine, Logger: slog.Default()}
	starter, canceller, reach := a.campaignCompositionSeams()
	if err := engine.RegisterIntegration(workspine.New(engine, slog.Default())); err != nil {
		t.Fatal(err)
	}
	const owner = "v1:identity:user:composer-1"
	person := auth.ContextWithUserActor(context.Background(), owner)

	// The work spine's own cancel: inside a run it is refused, in its words.
	inRun := common.ContextWithRun(person, common.RunContext{RunId: "v1:work:run:r-1", GoalId: "v1:work:goal:g-1", OwnerUserId: owner})
	if err := canceller.CancelGoal(inRun, "v1:work:goal:g-1", "cancelled by person"); err == nil || !strings.Contains(err.Error(), "closing a goal requires the person's interactive decision") {
		t.Errorf("CancelGoal did not reach the work spine's cancelGoal: %v", err)
	}
	// Its own OpenDirectGoal: a goal with no statement is refused, in its words.
	if _, err := starter.StartComposition(person, campaigns.CompositionGoal{OwnerUserID: owner, AccountID: "org-1", TemplateID: "template-1"}); err == nil || !strings.Contains(err.Error(), "a direct goal needs") {
		t.Errorf("StartComposition did not reach the work spine's OpenDirectGoal: %v", err)
	}
	// The engine's app doors: nothing is installed on this engine, so nothing
	// can be reached.
	if ok, err := reach.HasReachableApp(person, owner); ok || err != nil {
		t.Errorf("HasReachableApp = %v, %v", ok, err)
	}
}

// Every node can start a composition (a client calls the builtin on a bff;
// Ask's assistant on an agent), so the seams are installed where the campaigns
// worker is built, on every node type.
func TestCampaignCompositionIsWiredOnEveryNode(t *testing.T) {
	src := readAppFile(t, "engine.go")
	build, wire := strings.Index(src, "campaignWorker := campaigns.NewWorker("), strings.Index(src, "a.wireCampaignComposition(campaignWorker)")
	if build < 0 || wire < build {
		t.Fatal("engine.go does not call a.wireCampaignComposition(campaignWorker) after building the campaigns worker: campaignComposeEmail would refuse on every node")
	}
	if register := strings.Index(src, "a.engine.RegisterIntegration(campaignWorker)"); register < 0 || wire > register {
		t.Error("the composition seams should be installed before the campaigns worker is registered, so no call reaches a worker without them")
	}
}
