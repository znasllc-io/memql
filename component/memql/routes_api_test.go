package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// routes_api_test.go -- API, the fourth built-in route (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// R4): vendor models only, editable for the cluster as Local and Apps are.

func TestApiIsABuiltInRouteAfterApps(t *testing.T) {
	require.Equal(t, []string{RouteIdAuto, RouteIdLocal, RouteIdApps, RouteIdApi}, BuiltinRouteIds(),
		"the order every route list is drawn in")

	spec, ok := BuiltinRouteSpec(RouteIdApi, nil)
	require.True(t, ok)
	require.Equal(t, RouteSpec{
		Id: "api", Name: "API", Kind: RouteKindApi, Scope: RouteScopeCluster,
		Description: "Vendor models, paid by an API key or the cluster's account",
		Jobs: RouteJobs{
			Fast:      []string{"federation:strongest"},
			Strong:    []string{"federation:strongest"},
			Reasoning: []string{"federation:strongest"},
		},
	}, spec)
	require.Equal(t, "api", RouteKindApi)
	require.True(t, IsBuiltinRouteId(RouteIdApi))
	require.True(t, IsEditableBuiltinRoute(RouteIdApi))

	// The wire: Ask sends route:api, and it parses as a route of that id.
	choice, err := ParseRouteChoice("route:api", "")
	require.NoError(t, err)
	require.Equal(t, "route:api", choice.Source)
	id, isRoute := RouteIdFromSource("route:api")
	require.True(t, isRoute)
	require.Equal(t, RouteIdApi, id)
}

func TestApiShipsStrongestAtEveryJob(t *testing.T) {
	jobs, ok := BuiltinRouteDefault(RouteIdApi)
	require.True(t, ok)
	for _, sources := range [][]string{jobs.Fast, jobs.Strong, jobs.Reasoning} {
		require.Equal(t, []string{"federation:strongest"}, sources)
	}
	require.NoError(t, ValidateBuiltinRouteJobs(RouteIdApi, jobs))
}

// A route named API that listed a local model or an app would say something
// untrue, so saving one is refused, and nothing is written.
func TestSavingApiRefusesANonVendorSource(t *testing.T) {
	store := &testPolicyStore{doc: PolicyDocument{Overrides: map[string]PolicyConfig{}}}
	r := policyFixture(store)
	ctx := policyOwner()

	vendor := []string{"federation:strongest"}
	for source, jobs := range map[string]RouteJobs{
		"fleet:strongest": {Fast: []string{"fleet:strongest"}, Strong: vendor, Reasoning: vendor},
		"app:*":           {Fast: vendor, Strong: []string{"federation:cheapest", "app:*"}, Reasoning: vendor},
		"app:claude-code": {Fast: vendor, Strong: vendor, Reasoning: []string{"app:claude-code"}},
	} {
		err := r.SaveRouteOverride(ctx, 0, RouteIdApi, jobs)
		require.EqualError(t, err, `API lists vendor models only; "`+source+`" is not one`, source)
	}
	require.Equal(t, int64(0), store.doc.Revision, "a refused save moved the revision")
	require.Empty(t, store.doc.Routes, "a refused save wrote a route")

	// The same list is fine on Local, which may name any source.
	require.NoError(t, ValidateBuiltinRouteJobs(RouteIdLocal, RouteJobs{Fast: []string{"fleet:strongest"}, Strong: []string{"app:*"}, Reasoning: vendor}))
	// And what any route refuses, API refuses too.
	require.Error(t, ValidateBuiltinRouteJobs(RouteIdApi, RouteJobs{Fast: vendor, Strong: vendor}))
	require.Error(t, ValidateBuiltinRouteJobs(RouteIdApi, RouteJobs{Fast: []string{"policy:localFirst"}, Strong: vendor, Reasoning: vendor}))
}

// API is the cluster's to change, exactly as Local and Apps are: the same
// gate, the same revision, the same Restore default.
func TestApiCanBeEditedAndRestored(t *testing.T) {
	store := &testPolicyStore{doc: PolicyDocument{Overrides: map[string]PolicyConfig{}}}
	a, b := policyFixture(store), policyFixture(store)
	ctx := policyOwner()

	edited := RouteJobs{
		Fast:      []string{"federation:cheapest"},
		Strong:    []string{"federation:strongest", "federation:cheapest"},
		Reasoning: []string{"federation:strongest"},
	}
	require.NoError(t, a.SaveRouteOverride(ctx, 0, RouteIdApi, edited))

	snapshot, err := b.Snapshot(ctx)
	require.NoError(t, err)
	got, ok := snapshot.RouteOverride(RouteIdApi)
	require.True(t, ok, "the override is not in another node's next snapshot")
	require.Equal(t, edited, got)
	spec, _ := BuiltinRouteSpec(RouteIdApi, &got)
	require.True(t, spec.Customized)
	require.Equal(t, edited, spec.Jobs)

	require.Error(t, a.SaveRouteOverride(ctx, 0, RouteIdApi, edited), "a stale revision saved")
	require.NoError(t, a.ResetRouteOverride(ctx, 1, RouteIdApi))
	snapshot, err = b.Snapshot(ctx)
	require.NoError(t, err)
	_, ok = snapshot.RouteOverride(RouteIdApi)
	require.False(t, ok, "Restore default must remove the override")
	spec, _ = BuiltinRouteSpec(RouteIdApi, nil)
	require.False(t, spec.Customized)
}

func TestNoPersonalRouteIsNamedApi(t *testing.T) {
	for _, name := range []string{"API", "api", " Api "} {
		_, err := NormalizeRouteName(name)
		require.Error(t, err, name)
		require.Contains(t, err.Error(), "built-in route", name)
	}
}

// The route's own line says who pays, and it says only what is true for the
// person reading it: the cluster's account is named when it is set up and pays
// for them (owner decision 2026-11-22, vendor_account_cluster.go).
func TestApiSaysWhoPaysForThisPerson(t *testing.T) {
	api, _ := BuiltinRouteSpec(RouteIdApi, nil)
	both := "Vendor models, paid by an API key or the cluster's account"
	keyOnly := "Vendor models, paid by an API key"
	for name, tc := range map[string]struct {
		doors inferenceDoors
		want  string
	}{
		"the cluster's account pays for this person":     {inferenceDoors{ClusterFederation: true, ClusterAccountOpen: true, Federation: true}, both},
		"the cluster has an account, closed to them":     {inferenceDoors{ClusterFederation: true, KeyAccount: true, Federation: true}, keyOnly},
		"the cluster has no account at any vendor":       {inferenceDoors{KeyAccount: true, Federation: true}, keyOnly},
		"nothing is set up and the person has no key":    {inferenceDoors{}, keyOnly},
		"closed to them, and they have no key of theirs": {inferenceDoors{ClusterFederation: true}, keyOnly},
	} {
		got := routeReadiness{doors: tc.doors}.summarize(api)
		require.Equal(t, tc.want, got.Description, name)
		require.Equal(t, RouteKindApi, got.Kind, name)
	}
	// Local and Apps keep their one line whoever reads it.
	local, _ := BuiltinRouteSpec(RouteIdLocal, nil)
	require.Equal(t, "Local models for everything", routeReadiness{}.summarize(local).Description)
}
