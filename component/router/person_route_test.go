package router

// A person's route through the router (design record 2026-10-18-routes).
//
// Every test resolves through ResolveFor with nothing but the run context the
// executing node built from the run row -- exactly as in production, where the
// planner compiling and the agent running a step each read the choice off the
// row and share no memory with the node that took the turn.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// routeRouter is the shipped three-door default -- fleet:strongest, app:*, a
// vendor -- with the VENDOR FIRST in the rule's chain, so a route that leaked
// back to the rules would show as a vendor call rather than as a coincidence.
func routeRouter(t *testing.T, models []memql.FleetModel, doors []memql.AppDoor) (*Router, *countingCloud, *stubAppInference) {
	t.Helper()
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(&stubFleetInference{models: models})
	apps := &stubAppInference{doors: doors}
	providers.SetAppInference(apps)
	cloud := &countingCloud{}
	providers.RegisterWithParamsForTest("vendor", "OpenAI", "gpt", map[string]any{"contextWindow": 200000}, cloud)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{
		"defaultChain": {"vendor", memql.FleetStrongest, memql.AppWildcard},
	})
	return New(providers, policies, testRules(t, defaultRule("defaultChain")), nil, nil), cloud, apps
}

func chatRequest(level airoute.Level) ResolveRequest {
	return ResolveRequest{Level: level, Modality: airoute.ModalityChat, UserId: "alice",
		Needs: airoute.Needs{MinContextTokens: 4000}}
}

func routeChoice(source string) context.Context {
	return chosenRun("reply", common.RouteChoice{Source: source, By: "alice"})
}

// Local serves from the fleet, and when the fleet is gone it refuses NAMING
// THE ROUTE -- it never reaches the vendor the rule would have picked first.
func TestTheLocalRouteServesFromTheFleetAndNeverFallsThrough(t *testing.T) {
	r, cloud, _ := routeRouter(t, []memql.FleetModel{fleetModel("qwen3.5:9b", true)}, nil)
	got, err := r.ResolveFor(routeChoice("route:local"), chatRequest(airoute.LevelStrong))
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	if got.Resolution.ProviderName != "fleet:qwen3.5:9b" || got.Resolution.Decision.Route != memql.RouteIdLocal {
		t.Fatalf("resolved %q under route %q, want the fleet model under local",
			got.Resolution.ProviderName, got.Resolution.Decision.Route)
	}
	if got.Resolution.Decision.Rule != "" {
		t.Fatalf("rule %q decided a call made under a route", got.Resolution.Decision.Rule)
	}

	r, cloud, _ = routeRouter(t, []memql.FleetModel{fleetModel("qwen3.5:9b", false)}, nil)
	_, err = r.ResolveFor(routeChoice("route:local"), chatRequest(airoute.LevelStrong))
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) || refusal.Code != work.RefusalEveryDoorShut {
		t.Fatalf("err = %v, want every_door_shut", err)
	}
	if refusal.Route != "Local" || refusal.Decision.Route != memql.RouteIdLocal ||
		!strings.Contains(err.Error(), "[route Local: a route never falls through") {
		t.Fatalf("the refusal does not name the route: route=%q decision=%q msg=%q",
			refusal.Route, refusal.Decision.Route, err.Error())
	}
	if cloud.calls != 0 {
		t.Fatalf("the vendor was called %d time(s) for a run on the Local route", cloud.calls)
	}
}

// Apps means apps: served by the app when one is signed in, refused when none
// is -- with a fleet model online and a vendor first in the rule's chain.
func TestTheAppsRouteNeverReachesAnotherSource(t *testing.T) {
	r, cloud, apps := routeRouter(t, []memql.FleetModel{fleetModel("qwen3.5:9b", true)},
		[]memql.AppDoor{openApp("claude-code")})
	got, err := r.ResolveFor(routeChoice("route:apps"), chatRequest(airoute.LevelFast))
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	if got.Resolution.Decision.Door != airoute.DoorApp || got.Resolution.Decision.Route != memql.RouteIdApps {
		t.Fatalf("door=%q route=%q, want an app under apps", got.Resolution.Decision.Door, got.Resolution.Decision.Route)
	}
	chat, ok := got.Client.(common.ChatAIProvider)
	if !ok {
		t.Fatalf("client %T serves no chat", got.Client)
	}
	// On the background lane, as the other dispatching tests in this package
	// do: the interactive lane's process-wide rate ceiling is shared by every
	// test here, and a test that only proves which door answered should not
	// spend another's budget.
	if _, err := chat.CallChat(memql.ContextWithBackgroundLane(context.Background()), []common.ChatMessage{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("CallChat: %v", err)
	}
	if apps.calls != 1 || cloud.calls != 0 {
		t.Fatalf("app calls=%d vendor calls=%d, want the app alone", apps.calls, cloud.calls)
	}

	r, cloud, _ = routeRouter(t, []memql.FleetModel{fleetModel("qwen3.5:9b", true)}, []memql.AppDoor{shutApp("claude-code")})
	_, err = r.ResolveFor(routeChoice("route:apps"), chatRequest(airoute.LevelFast))
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) || refusal.Route != "Apps" {
		t.Fatalf("err = %v, want a refusal naming Apps", err)
	}
	for _, d := range refusal.Doors {
		if d.Door != airoute.DoorApp {
			t.Fatalf("the Apps route considered %q (%s), a source it does not name", d.Name, d.Door)
		}
	}
	if cloud.calls != 0 {
		t.Fatalf("the vendor was called %d time(s) for a run on the Apps route", cloud.calls)
	}
}

// A reasoning call whose sources are all shut DEGRADES INSIDE THE ROUTE, through
// the route's own list for the level below, and says so on the record.
func TestARouteDegradesThroughItsOwnLists(t *testing.T) {
	r, cloud, _ := routeRouter(t, []memql.FleetModel{fleetModel("qwen3.5:9b", true)}, nil)
	r.policies.SetRouteOverrideForTest(memql.RouteIdLocal, memql.RouteJobs{
		Fast:      []string{memql.FleetFastest},
		Strong:    []string{memql.FleetStrongest},
		Reasoning: []string{"fleet:qwen3.8:27b"}, // offered by no machine
	})
	got, err := r.ResolveFor(routeChoice("route:local"), chatRequest(airoute.LevelReasoning))
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	d := got.Resolution.Decision
	if !d.Degraded || d.Level != airoute.LevelReasoning || d.ServedLevel != airoute.LevelStrong {
		t.Fatalf("decision %+v, want reasoning served at strong and marked degraded", d)
	}
	if got.Resolution.ProviderName != "fleet:qwen3.5:9b" || cloud.calls != 0 {
		t.Fatalf("served by %q (vendor calls %d), want the route's strong source", got.Resolution.ProviderName, cloud.calls)
	}
}

// Embeddings, vision and audio never follow a route: an embedding call made in
// a run on the Apps route resolves through the rules, because no route is
// applied to it at all.
func TestARouteNeverGovernsEmbeddings(t *testing.T) {
	req := ResolveRequest{Level: airoute.LevelEmbeddings, Modality: airoute.ModalityEmbedding, UserId: "alice"}
	got, err := memql.ApplyRunRouting(routeChoice("route:apps"), req)
	if err != nil {
		t.Fatalf("ApplyRunRouting: %v", err)
	}
	if got.PersonRoute != "" || got.ExplicitProvider != "" {
		t.Fatalf("an embedding call took route %q / pin %q", got.PersonRoute, got.ExplicitProvider)
	}
	vision := ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityVision, UserId: "alice"}
	if got, _ := memql.ApplyRunRouting(routeChoice("route:apps"), vision); got.PersonRoute != "" {
		t.Fatalf("a vision call took route %q", got.PersonRoute)
	}
}

// fakeRouteEngine answers the router's one read of a personal route: the row
// for routeForOwner when the owner and id match, nothing otherwise.
type fakeRouteEngine struct {
	owner, id string
	jobs      memql.RouteJobs
	queries   []string
}

func (f *fakeRouteEngine) Execute(_ context.Context, query string) (*memql.ExecuteResult, error) {
	f.queries = append(f.queries, query)
	if !strings.Contains(query, "routeForOwner") || !strings.Contains(query, f.owner) || !strings.Contains(query, f.id) {
		return memql.NewResultWithOutput([]map[string]any{}), nil
	}
	return memql.NewResultWithOutput([]map[string]any{{
		"id": memql.RouteConceptID + ":" + f.id, "ownerUserId": f.owner, "name": "Out of credits",
		"fast": toAny(f.jobs.Fast), "strong": toAny(f.jobs.Strong), "reasoning": toAny(f.jobs.Reasoning), "active": true,
	}}), nil
}

func toAny(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

// A PERSONAL route is read for its owner, on whichever node runs the call, and
// walked like a built-in one. Somebody else's route id resolves to nothing.
func TestAPersonalRouteIsReadForItsOwnerAndWalked(t *testing.T) {
	r, cloud, _ := routeRouter(t, []memql.FleetModel{fleetModel("qwen3.5:9b", true)}, nil)
	engine := &fakeRouteEngine{owner: "alice", id: "8d3c", jobs: memql.RouteJobs{
		Fast: []string{memql.FleetFastest}, Strong: []string{memql.FleetStrongest}, Reasoning: []string{memql.FleetStrongest},
	}}
	r.engine = engine
	got, err := r.ResolveFor(routeChoice("route:8d3c"), chatRequest(airoute.LevelStrong))
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	if got.Resolution.Decision.Route != "8d3c" || got.Resolution.ProviderName != "fleet:qwen3.5:9b" || cloud.calls != 0 {
		t.Fatalf("route=%q provider=%q vendor calls=%d", got.Resolution.Decision.Route, got.Resolution.ProviderName, cloud.calls)
	}
	if len(engine.queries) == 0 || !strings.Contains(engine.queries[0], "ownerUserId") {
		t.Fatalf("the route was not read for its owner: %v", engine.queries)
	}

	// Bob's run naming Alice's route id reads nothing and refuses plainly --
	// not as a shut door a machine waking could fix.
	bob := common.ContextWithRun(context.Background(), common.RunContext{
		RunId: "v1:work:run:r2", OwnerUserId: "bob", StepKey: "reply", Mode: common.RunModeLive,
		Routing: common.RouteChoice{Source: "route:8d3c", By: "bob"},
	})
	_, err = r.ResolveFor(bob, chatRequest(airoute.LevelStrong))
	var refusal *InferenceUnavailable
	if err == nil || errors.As(err, &refusal) || !strings.Contains(err.Error(), "not one of its owner's routes") {
		t.Fatalf("err = %v, want a plain error saying the route is not the owner's", err)
	}
}

// Ask streams, and on the Apps route an app answers the streaming turn: whole,
// as one chunk, then Done -- never "unavailable" because the source is an app.
func TestAnAppAnswersAStreamingTurnOnTheAppsRoute(t *testing.T) {
	r, cloud, apps := routeRouter(t, nil, []memql.AppDoor{openApp("codex")})
	req := chatRequest(airoute.LevelStrong)
	req.Modality = airoute.ModalityStreamingChat
	got, err := r.ResolveFor(routeChoice("route:apps"), req)
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	stream, ok := got.Client.(common.ChatStreamProvider)
	if !ok {
		t.Fatalf("client %T does not stream", got.Client)
	}
	ch, err := stream.CallChatStream(memql.ContextWithBackgroundLane(context.Background()), []common.ChatMessage{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("CallChatStream: %v", err)
	}
	var text strings.Builder
	done := false
	for chunk := range ch {
		text.WriteString(chunk.Content)
		done = done || chunk.Done
	}
	if text.String() != "app answer" || !done || apps.calls != 1 || cloud.calls != 0 {
		t.Fatalf("streamed %q done=%v app calls=%d vendor calls=%d", text.String(), done, apps.calls, cloud.calls)
	}
}
