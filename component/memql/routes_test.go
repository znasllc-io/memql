package memql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// The route grammar on the wire (design record 2026-10-18-routes): route:auto
// IS Auto and is stored as Auto, the built-in ids and a personal route's bare
// id parse, and anything that could not be a route id is refused at the turn.
func TestRouteChoiceGrammar(t *testing.T) {
	for _, ok := range []string{"route:local", "route:apps", "route:8d3c2b1a-0f4e-4c7a-9b2d-1e3f5a7c9d0b"} {
		got, err := ParseRouteChoice(ok, "")
		require.NoError(t, err, ok)
		require.Equal(t, ok, got.Source)
	}
	auto, err := ParseRouteChoice("route:auto", "")
	require.NoError(t, err)
	require.True(t, auto.IsZero(), "route:auto must be stored as Auto, an empty choice")

	for _, bad := range []string{"route:has space", "route:a/b", "route:-dash-first"} {
		_, err := ParseRouteChoice(bad, "")
		var refusal *RouteChoiceError
		require.True(t, errors.As(err, &refusal), bad)
		require.Equal(t, RouteSourceInvalid, refusal.Code, bad)
	}
}

// A route's jobs are held to the router's own grammar AND to the three source
// kinds a route may name. Search and memory is never a route's.
func TestRouteJobsValidation(t *testing.T) {
	good := RouteJobs{Fast: []string{"fleet:fastest"}, Strong: []string{"app:claude-code", "fleet:strongest"}, Reasoning: []string{"federation:strongest"}}
	require.NoError(t, ValidateRouteJobs(good))

	cases := map[string]RouteJobs{
		"an empty job":         {Fast: nil, Strong: []string{"fleet:strongest"}, Reasoning: []string{"fleet:strongest"}},
		"five sources":         {Fast: []string{"fleet:a", "fleet:b", "fleet:c", "fleet:d", "fleet:e"}, Strong: []string{"app:*"}, Reasoning: []string{"app:*"}},
		"a policy":             {Fast: []string{"policy:localFirst"}, Strong: []string{"app:*"}, Reasoning: []string{"app:*"}},
		"the embedder":         {Fast: []string{"embedder:active"}, Strong: []string{"app:*"}, Reasoning: []string{"app:*"}},
		"a bare provider name": {Fast: []string{"streamClaudeSonnet"}, Strong: []string{"app:*"}, Reasoning: []string{"app:*"}},
		"a repeated source":    {Fast: []string{"app:*", "app:*"}, Strong: []string{"app:*"}, Reasoning: []string{"app:*"}},
		"an unknown app":       {Fast: []string{"app:gemini-cli"}, Strong: []string{"app:*"}, Reasoning: []string{"app:*"}},
		"the retired wildcard": {Fast: []string{"fleet:*"}, Strong: []string{"app:*"}, Reasoning: []string{"app:*"}},
	}
	for name, jobs := range cases {
		require.Error(t, ValidateRouteJobs(jobs), name)
	}
}

func TestRouteNames(t *testing.T) {
	got, err := NormalizeRouteName("  Out   of credits ")
	require.NoError(t, err)
	require.Equal(t, "Out of credits", got)
	for _, bad := range []string{"", "   ", strings.Repeat("x", MaxRouteNameLength+1), "local", "APPS", "Auto"} {
		_, err := NormalizeRouteName(bad)
		require.Error(t, err, bad)
	}
}

// The route half of the routing document shares the policies' revision and
// CAS: an override saved on one node is in the next decision's snapshot on
// another, a stale revision is refused, and Restore default removes it.
func TestRouteOverridesShareTheRoutingDocument(t *testing.T) {
	store := &testPolicyStore{doc: PolicyDocument{Overrides: map[string]PolicyConfig{}}}
	a, b := policyFixture(store), policyFixture(store)
	ctx := policyOwner()

	apps := RouteJobs{Fast: []string{"app:codex"}, Strong: []string{"app:claude-code"}, Reasoning: []string{"app:claude-code"}}
	require.NoError(t, a.SaveRouteOverride(ctx, 0, RouteIdApps, apps))

	snapshot, err := b.Snapshot(ctx)
	require.NoError(t, err)
	got, ok := snapshot.RouteOverride(RouteIdApps)
	require.True(t, ok)
	require.Equal(t, apps, got)
	spec, _ := BuiltinRouteSpec(RouteIdApps, &got)
	require.True(t, spec.Customized)

	// One revision for routes AND policies: a policy save at the revision a
	// route save already moved past is refused, and so is the reverse.
	require.Error(t, b.Save(ctx, 0, PolicyConfig{Name: "localFirst", Primary: "fleet:strongest"}))
	require.NoError(t, b.Save(ctx, 1, PolicyConfig{Name: "localFirst", Primary: "fleet:fastest"}))
	require.Error(t, a.SaveRouteOverride(ctx, 1, RouteIdApps, apps))

	require.NoError(t, a.ResetRouteOverride(ctx, 2, RouteIdApps))
	snapshot, err = b.Snapshot(ctx)
	require.NoError(t, err)
	_, ok = snapshot.RouteOverride(RouteIdApps)
	require.False(t, ok, "Restore default must remove the override")

	// Auto has no jobs; personal ids are not cluster routes; and only an owner
	// or developer may change a route for the cluster.
	require.Error(t, a.SaveRouteOverride(ctx, 3, RouteIdAuto, apps))
	require.Error(t, a.SaveRouteOverride(ctx, 3, "8d3c", apps))
	writer := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "w", Role: auth.RoleWriter})
	require.Error(t, a.SaveRouteOverride(writer, 3, RouteIdLocal, apps))
}

func TestAutoBalanceSettings(t *testing.T) {
	store := &testPolicyStore{doc: PolicyDocument{Overrides: map[string]PolicyConfig{}}}
	r := policyFixture(store)
	ctx := policyOwner()

	got, customized := r.AutoBalanceSetting()
	require.Equal(t, RecommendedAutoBalance, got)
	require.False(t, customized)

	require.Error(t, r.SaveAutoBalance(ctx, 0, AutoBalance{Speed: 101, Cost: 50, Quality: 50}))
	require.NoError(t, r.SaveAutoBalance(ctx, 0, AutoBalance{Speed: 90, Cost: 10, Quality: 40}))
	snapshot, err := r.Snapshot(ctx)
	require.NoError(t, err)
	got, customized = snapshot.AutoBalanceSetting()
	require.True(t, customized)
	require.Equal(t, AutoBalance{Speed: 90, Cost: 10, Quality: 40}, got)

	require.NoError(t, r.ResetAutoBalance(ctx, 1))
	snapshot, err = r.Snapshot(ctx)
	require.NoError(t, err)
	got, customized = snapshot.AutoBalanceSetting()
	require.False(t, customized)
	require.Equal(t, RecommendedAutoBalance, got)
}

// What the route picker shows: each route judged against the same moment,
// with the one sentence that says what is missing.
func TestRouteReadinessSummaries(t *testing.T) {
	local, _ := BuiltinRouteSpec(RouteIdLocal, nil)
	apps, _ := BuiltinRouteSpec(RouteIdApps, nil)
	auto, _ := BuiltinRouteSpec(RouteIdAuto, nil)

	fleetOnly := routeReadiness{doors: inferenceDoors{LocalEligible: true, EligibleModelIds: []string{"qwen3.5:9b"}}, embedder: "fleet:qwen3-embedding:0.6b", embeddingReachable: true}
	s := fleetOnly.summarize(local)
	require.True(t, s.Ready, s.Reason)
	require.Len(t, s.Jobs, 4)
	require.Equal(t, "Strongest local model", s.Jobs[1].ServesNow)
	require.False(t, s.Jobs[3].Editable, "Search and memory is never a route's to change")
	require.Equal(t, []string{"embedder:active"}, s.Jobs[3].Sources)

	s = fleetOnly.summarize(apps)
	require.False(t, s.Ready)
	require.Equal(t, "No app is ready on your machines", s.Reason)

	appsNoEmbedder := routeReadiness{doors: inferenceDoors{AppEligible: true, RunnableApps: []string{"claude-code", "codex"}}}
	s = appsNoEmbedder.summarize(apps)
	require.False(t, s.Ready)
	require.Equal(t, "Needs an embedding model", s.Reason)
	require.Equal(t, "Claude Code or Codex", s.Jobs[0].ServesNow)

	s = appsNoEmbedder.summarize(auto)
	require.False(t, s.Ready)
	require.Equal(t, "Needs an embedding model", s.Reason)
	require.Empty(t, s.Jobs)

	nothing := routeReadiness{}
	require.Equal(t, "Needs a source that can answer", nothing.summarize(auto).Reason)
}

// The route reaches every call of the run through the run context: a person's
// route sets PersonRoute and its owner, overrules an author's pin, and gives
// way to the executing step's own model override.
func TestApplyRunRoutingCarriesAPersonsRoute(t *testing.T) {
	ctx := common.ContextWithRun(context.Background(), common.RunContext{RunId: "r1", StepKey: "reply",
		Routing: common.RouteChoice{Source: "route:apps", By: "alice"}})
	got, err := ApplyRunRouting(ctx, airoute.ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityChat, ExplicitProvider: "chat54Mini", PinnedBy: ""})
	require.NoError(t, err)
	require.Equal(t, RouteIdApps, got.PersonRoute)
	require.Equal(t, "alice", got.PersonRouteOwner)
	require.Empty(t, got.ExplicitProvider, "a person's route overrules an author's pin")

	ov := common.ContextWithRun(context.Background(), common.RunContext{RunId: "r1", StepKey: "reply",
		Routing:  common.RouteChoice{Source: "route:apps", By: "alice"},
		Override: &common.StepOverride{Model: "fleet:qwen3.5:9b"}})
	got, err = ApplyRunRouting(ov, airoute.ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityChat})
	require.NoError(t, err)
	require.Empty(t, got.PersonRoute, "a step's own override is more specific than the run's route")
}

func TestUserBudgetStates(t *testing.T) {
	require.Equal(t, "none", UserBudget{}.State())
	require.Equal(t, "under", UserBudget{Limit: 100, Set: true, Spent: 79.99}.State())
	require.Equal(t, "near", UserBudget{Limit: 100, Set: true, Spent: 80}.State())
	require.Equal(t, "over", UserBudget{Limit: 100, Set: true, Spent: 100}.State())

	start, next := monthWindow(time.Date(2026, 12, 31, 23, 59, 0, 0, time.FixedZone("x", -8*3600)))
	require.Equal(t, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), start, "the month is the UTC month")
	require.Equal(t, time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC), next)
}
