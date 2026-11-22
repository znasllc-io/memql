package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/core/airoute"
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

// ---- whether API can serve is asked for the person, job by job (R6) ----

// apiCatalog registers vendor text records that declare a job. clusterAccount
// says whether the cluster's own account is set up at each vendor.
func apiCatalog(clusterAccount map[string]bool, records ...apiRecord) *ProviderRegistry {
	if len(records) == 0 {
		records = []apiRecord{
			{"claudeQuick", VendorAnthropic, "claude-quick", "fast", 300},
			{"claudeMain", VendorAnthropic, "claude-main", "strong", 600},
			{"claudeDeep", VendorAnthropic, "claude-deep", "strong reasoning", 900},
			{"gptQuick", VendorOpenAI, "gpt-quick", "fast", 280},
			{"gptDeep", VendorOpenAI, "gpt-deep", "strong reasoning", 880},
		}
	}
	reg := newProviderRegistry()
	for _, rec := range records {
		entry := vendorRecord(rec.name, rec.vendor, clusterAccount[rec.vendor], "")
		entry.Config.Model = rec.model
		entry.Config.Params = map[string]any{"contextWindow": 200000, "inputCostPerMillion": 1.0, "outputCostPerMillion": 2.0}
		if rec.levels != "" {
			entry.Config.Params["levels"], entry.Config.Params["rank"] = rec.levels, rec.rank
		}
		reg.setEntry(entry)
	}
	return reg
}

type apiRecord struct {
	name, vendor, model, levels string
	rank                        int
}

// apiSummary is the API route as one person sees it on this registry.
func apiSummary(reg *ProviderRegistry, ctx context.Context) routeSummaryJSON {
	api, _ := BuiltinRouteSpec(RouteIdApi, nil)
	readiness := newRouteReadiness((&MemQLEngine{providers: reg}).inferenceDoors(ctx), reg)
	readiness.embeddingReachable = true
	return readiness.summarize(api)
}

func servesNowByJob(s routeSummaryJSON) map[string]string {
	out := map[string]string{}
	for _, job := range s.Jobs {
		out[job.Job] = job.ServesNow
	}
	return out
}

func TestApiIsReadyWithAKeyAndNoVendorAccount(t *testing.T) {
	reg := apiCatalog(nil)
	reg.SetVendorKeys(keysHeldBy("ana", keyAccount(VendorAnthropic, "k1", "ana")))

	s := apiSummary(reg, personContext("ana"))
	require.True(t, s.Ready, s.Reason)
	require.Equal(t, "Vendor models, paid by an API key", s.Description)
	serves := servesNowByJob(s)
	require.Equal(t, "claude-quick, paid by your API key", serves["fast"])
	require.Equal(t, "claude-deep, paid by your API key", serves["strong"], "the strongest record at the level, not the first by name")
	require.Equal(t, "claude-deep, paid by your API key", serves["reasoning"])
}

func TestApiIsReadyWithTheClustersAccountAndNoKey(t *testing.T) {
	reg := apiCatalog(map[string]bool{VendorOpenAI: true})
	openToEverybody(reg)
	for _, name := range []string{"gptQuick", "gptDeep"} {
		entry, _ := reg.Entry(name)
		federatedOpenAI(entry)
	}
	reg.SetVendorKeys(noKeys())

	s := apiSummary(reg, personContext("ana"))
	require.True(t, s.Ready, s.Reason)
	require.Equal(t, "Vendor models, paid by an API key or the cluster's account", s.Description)
	serves := servesNowByJob(s)
	require.Equal(t, "gpt-quick, paid by the cluster's account", serves["fast"])
	require.Equal(t, "gpt-deep, paid by the cluster's account", serves["strong"],
		"the higher-ranked Claude record has no account, so the next one that has serves")
	require.Equal(t, "gpt-deep, paid by the cluster's account", serves["reasoning"])
}

func TestApiSaysWhatItNeedsWhenNothingCanPay(t *testing.T) {
	// No account anywhere.
	reg := apiCatalog(nil)
	reg.SetVendorKeys(noKeys())
	s := apiSummary(reg, personContext("ana"))
	require.False(t, s.Ready)
	require.Equal(t, "Needs an API key or a vendor account", s.Reason)
	for _, job := range s.Jobs[:3] {
		require.False(t, job.Ready, job.Job)
		require.Empty(t, job.ServesNow, job.Job)
	}

	// The cluster has an account and it does not pay for this person.
	reg = apiCatalog(map[string]bool{VendorOpenAI: true})
	for _, name := range []string{"gptQuick", "gptDeep"} {
		entry, _ := reg.Entry(name)
		federatedOpenAI(entry)
	}
	reg.SetVendorKeys(noKeys())
	s = apiSummary(reg, personContext("ana"))
	require.False(t, s.Ready)
	require.Equal(t, "Needs an API key. The cluster's vendor account is not open to you", s.Reason)
}

// The account is named by its KIND, never by a key's name or its fingerprint:
// the kind is enough for the person reading their own route.
func TestServesNowNamesTheModelAndTheAccount(t *testing.T) {
	reg := apiCatalog(map[string]bool{VendorOpenAI: true})
	openToEverybody(reg)
	for _, name := range []string{"gptQuick", "gptDeep"} {
		entry, _ := reg.Entry(name)
		federatedOpenAI(entry)
	}
	// Ben lent Ana a key at Anthropic; she has none of her own there.
	reg.SetVendorKeys(keysHeldBy("ana", keyAccount(VendorAnthropic, "ben-key-7f3a", "ben")))

	readiness := newRouteReadiness((&MemQLEngine{providers: reg}).inferenceDoors(personContext("ana")), reg)
	for _, tc := range []struct {
		level airoute.Level
		entry string
		want  string
	}{
		{airoute.LevelReasoning, "federation:strongest", "claude-deep, paid by a key lent to you"},
		{airoute.LevelFast, "federation:strongest", "claude-quick, paid by a key lent to you"},
		{airoute.LevelStrong, "federation:gptDeep", "gpt-deep, paid by the cluster's account"},
		{airoute.LevelFast, "federation:claudeMain", "claude-main, paid by a key lent to you"},
	} {
		got, ok := readiness.servesNow(tc.level, tc.entry)
		require.True(t, ok, tc.entry)
		require.Equal(t, tc.want, got, tc.entry)
		require.NotContains(t, got, "ben-key-7f3a")
	}
	// A record that does not exist, and one of another kind, serve nothing.
	_, ok := readiness.servesNow(airoute.LevelStrong, "federation:noSuchRecord")
	require.False(t, ok)
}

// One engine, two people, at the same moment: the one with a key is ready and
// the one without is told what to add. Nothing about the first reaches the
// second.
func TestApiReadinessIsThePersons(t *testing.T) {
	reg := apiCatalog(nil)
	keys := keysHeldBy("ana", keyAccount(VendorOpenAI, "k1", "ana"))
	reg.SetVendorKeys(keys)

	ana := apiSummary(reg, personContext("ana"))
	require.True(t, ana.Ready, ana.Reason)
	require.Equal(t, "gpt-deep, paid by your API key", servesNowByJob(ana)["reasoning"])

	ben := apiSummary(reg, personContext("ben"))
	require.False(t, ben.Ready)
	require.Equal(t, "Needs an API key or a vendor account", ben.Reason)
	require.Empty(t, servesNowByJob(ben)["reasoning"])
}

// Accounts are asked once per vendor for one read of the routes, however many
// jobs and records the routes name.
func TestApiReadinessAsksEachVendorsKeysOncePerRead(t *testing.T) {
	reg := apiCatalog(nil)
	keys := noKeys()
	reg.SetVendorKeys(keys)
	_ = apiSummary(reg, personContext("ana"))
	asked := map[string]int{}
	for _, q := range keys.queries {
		asked[q.Vendor]++
	}
	require.Equal(t, map[string]int{VendorAnthropic: 1, VendorOpenAI: 1}, asked)
}

// A level no vendor record declares leaves that job, and so the route, not
// ready, however many accounts the person has.
func TestALevelWithNoRecordMakesApiNotReady(t *testing.T) {
	reg := apiCatalog(nil,
		apiRecord{"claudeQuick", VendorAnthropic, "claude-quick", "fast", 300},
		apiRecord{"claudeMain", VendorAnthropic, "claude-main", "strong", 600},
		apiRecord{"claudeUnranked", VendorAnthropic, "claude-unranked", "", 0},
	)
	reg.SetVendorKeys(keysHeldBy("ana", keyAccount(VendorAnthropic, "k1", "ana")))

	s := apiSummary(reg, personContext("ana"))
	require.False(t, s.Ready)
	require.Equal(t, "Deep reasoning needs a vendor model your account can pay for", s.Reason)
	serves := servesNowByJob(s)
	require.Equal(t, "claude-quick, paid by your API key", serves["fast"])
	require.Equal(t, "claude-main, paid by your API key", serves["strong"])
	require.Empty(t, serves["reasoning"], "a record that declares no level is not offered by a selector")
}
