package memql

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// vendor_account_cluster_test.go -- the cluster's own vendor account is not a
// pool for everybody (owner decision 2026-11-22, recorded at the end of
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md).
//
// The owner's rule is bring-your-own for everything a person does with a
// model. A cluster account, where its owner connected one, pays for the
// cluster's own work, and for a person's calls only when that person may use
// it. Everybody else pays with a key, a machine or an app of their own.

// gateFor is a gate that opens the cluster's account to the people named, and
// counts what it was asked.
type gateFor struct {
	mu      sync.Mutex
	allowed map[string]bool
	asked   []string
}

func openTo(people ...string) *gateFor {
	g := &gateFor{allowed: map[string]bool{}}
	for _, p := range people {
		g.allowed[p] = true
	}
	return g
}

func (g *gateFor) gate(_ context.Context, userId string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, userId)
	return g.allowed[userId]
}

func (g *gateFor) asks() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.asked)
}

// openToEverybody opens the cluster's account to every person, for a test
// that is about something else: the order of accounts, a key's address, a
// stream. Tests about who may use it are in this file.
func openToEverybody(reg *ProviderRegistry) {
	reg.SetClusterAccountGate(func(context.Context, string) bool { return true })
}

func refsOf(accounts []VendorAccount) string {
	refs := make([]string, 0, len(accounts))
	for _, a := range accounts {
		refs = append(refs, a.Ref())
	}
	return strings.Join(refs, " ")
}

func clusterPassedOver(passed []VendorAccountPassedOver) (string, bool) {
	for _, p := range passed {
		if p.Account.Kind == VendorAccountCluster {
			return p.Reason, true
		}
	}
	return "", false
}

func TestTheClustersAccountPaysForAPersonOnlyWhenTheyMayUseIt(t *testing.T) {
	reg := newProviderRegistry()
	gate := openTo("olga")
	reg.SetClusterAccountGate(gate.gate)
	entry := vendorEntry(VendorAnthropic, true, "")

	accounts, passed := reg.VendorAccountsFor(personContext("olga"), VendorAccountQuery{ActingUserId: "olga", Chosen: true}, entry)
	if refsOf(accounts) != "cluster" {
		t.Fatalf("accounts = %q, want the cluster's account for a person who may use it", refsOf(accounts))
	}
	if reason, skipped := clusterPassedOver(passed); skipped {
		t.Fatalf("the cluster's account was passed over for her: %q", reason)
	}

	accounts, passed = reg.VendorAccountsFor(personContext("ana"), VendorAccountQuery{ActingUserId: "ana", Chosen: true}, entry)
	if len(accounts) != 0 {
		t.Fatalf("accounts = %q, want nothing: the cluster's account is not hers to spend", refsOf(accounts))
	}
	reason, skipped := clusterPassedOver(passed)
	if !skipped || reason != ClusterAccountClosedReason {
		t.Fatalf("passed over = %+v, want the cluster's account with the reason it is closed to her", passed)
	}
	// The reason is on a record every signed-in person can read.
	if strings.Contains(reason, "ana") {
		t.Fatalf("the reason names the person: %q", reason)
	}
}

func TestTheClustersOwnWorkIsPaidWithoutAskingAnybody(t *testing.T) {
	for name, tc := range map[string]struct {
		ctx    context.Context
		userId string
	}{
		"an automation":                    {context.Background(), "system:automation:nightly"},
		"the cluster's actor":              {auth.ContextWithSystemActor(context.Background(), "sweep"), ""},
		"the operator's stream":            {context.Background(), "cluster:operator"},
		"a connector":                      {auth.ContextWithConnectorActor(context.Background(), "shopify"), ""},
		"a job wired as the cluster's own": {auth.ContextForClusterWork(context.Background(), "safety-classifier"), ""},
		// The safety check screens a person's command; the check is the
		// cluster's work, whoever it screens for.
		"a job wired as the cluster's own, run for a person": {auth.ContextForClusterWork(personContext("ana"), "safety-classifier"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			reg := newProviderRegistry()
			gate := openTo()
			reg.SetClusterAccountGate(gate.gate)
			accounts, _ := reg.VendorAccountsFor(tc.ctx, VendorAccountQuery{ActingUserId: tc.userId}, vendorEntry(VendorOpenAI, true, ""))
			if refsOf(accounts) != "cluster" {
				t.Fatalf("accounts = %q, want the cluster's account for its own work", refsOf(accounts))
			}
			if gate.asks() != 0 {
				t.Fatalf("the gate was asked %d times about work no person is behind", gate.asks())
			}
		})
	}
}

// A CALL THAT NAMES NOBODY IS NOT THE CLUSTER'S WORK (owner decision
// 2026-11-23). A context nobody stamped, and the anonymous reader, say neither
// whom the call is for nor that it is the cluster's, so the cluster's account
// does not pay for them, and nobody is asked: there is nobody to ask about.
// The reason says why without naming anybody, on a record every signed-in
// person can read.
func TestACallThatNamesNobodyIsNotPaidByTheClustersAccount(t *testing.T) {
	for name, tc := range map[string]struct {
		ctx    context.Context
		userId string
	}{
		"nobody stamped anything":       {context.Background(), ""},
		"the anonymous reader":          {auth.ContextWithAnonymousActor(context.Background()), auth.AnonymousUserId},
		"the anonymous reader, unnamed": {auth.ContextWithAnonymousActor(context.Background()), ""},
	} {
		t.Run(name, func(t *testing.T) {
			reg := newProviderRegistry()
			gate := openTo()
			reg.SetClusterAccountGate(gate.gate)
			entry := vendorEntry(VendorOpenAI, true, "")
			q := VendorAccountQuery{ActingUserId: tc.userId}

			accounts, passed := reg.VendorAccountsFor(tc.ctx, q, entry)
			if len(accounts) != 0 {
				t.Fatalf("accounts = %q, want nothing: the call says neither whom it is for nor that it is the cluster's", refsOf(accounts))
			}
			if reason, skipped := clusterPassedOver(passed); !skipped || reason != ClusterAccountUnnamedReason {
				t.Fatalf("passed over = %+v, want the cluster's account with the reason the call names nobody", passed)
			}
			if _, ok := reg.VendorAccountUsable(tc.ctx, q, entry, VendorAccountCluster); ok {
				t.Fatal("the cluster's account was spent at the call for a call that names nobody")
			}
			if reg.ClusterAccountClosedTo(tc.ctx, tc.userId) != ClusterAccountUnnamedReason {
				t.Fatalf("closed reason = %q, want %q", reg.ClusterAccountClosedTo(tc.ctx, tc.userId), ClusterAccountUnnamedReason)
			}
			if gate.asks() != 0 {
				t.Fatalf("the gate was asked %d times about a call that names nobody", gate.asks())
			}
		})
	}
	if strings.Contains(ClusterAccountUnnamedReason, "anonymous") {
		t.Fatalf("the reason names a kind of caller: %q", ClusterAccountUnnamedReason)
	}
}

// A request that names nobody, made under a person's own context, is that
// person's call: the router fills an empty acting id from the context, and
// the account rule reads it the same way. It is judged by their permission,
// never paid unasked.
func TestACallThatNamesNobodyIsJudgedByThePersonItsContextCarries(t *testing.T) {
	reg := newProviderRegistry()
	gate := openTo("olga")
	reg.SetClusterAccountGate(gate.gate)
	entry := vendorEntry(VendorAnthropic, true, "")

	if accounts, _ := reg.VendorAccountsFor(personContext("olga"), VendorAccountQuery{}, entry); refsOf(accounts) != "cluster" {
		t.Fatalf("accounts = %q, want the cluster's account for a person who may use it", refsOf(accounts))
	}
	accounts, passed := reg.VendorAccountsFor(personContext("ana"), VendorAccountQuery{}, entry)
	if len(accounts) != 0 {
		t.Fatalf("accounts = %q, want nothing for a person the cluster's account is closed to", refsOf(accounts))
	}
	if reason, _ := clusterPassedOver(passed); reason != ClusterAccountClosedReason {
		t.Fatalf("reason = %q, want %q", reason, ClusterAccountClosedReason)
	}
	gate.mu.Lock()
	asked := append([]string(nil), gate.asked...)
	gate.mu.Unlock()
	if strings.Join(asked, " ") != "olga ana" {
		t.Fatalf("the gate was asked about %q, want each person the context carried", asked)
	}
}

// A registry nobody installed a gate on gives the cluster's account to no
// person. The engine installs its gate when it starts; this is what a
// registry built some other way does, and it is the narrowing direction.
func TestWithNoGateTheClustersAccountServesOnlyItsOwnWork(t *testing.T) {
	reg := newProviderRegistry()
	entry := vendorEntry(VendorAnthropic, true, "")
	if accounts, _ := reg.VendorAccountsFor(personContext("ana"), VendorAccountQuery{ActingUserId: "ana", Chosen: true}, entry); len(accounts) != 0 {
		t.Fatalf("accounts = %q for a person, with no gate installed", refsOf(accounts))
	}
	own := auth.ContextWithSystemActor(context.Background(), "sweep")
	if accounts, _ := reg.VendorAccountsFor(own, VendorAccountQuery{}, entry); refsOf(accounts) != "cluster" {
		t.Fatalf("accounts = %q for the cluster's own work, with no gate installed", refsOf(accounts))
	}
	if accounts, _ := reg.VendorAccountsFor(context.Background(), VendorAccountQuery{}, entry); len(accounts) != 0 {
		t.Fatalf("accounts = %q for a call that names nobody, with no gate installed", refsOf(accounts))
	}
}

func TestAPersonWhoMayNotUseTheClustersAccountStillPaysWithAKey(t *testing.T) {
	reg := newProviderRegistry()
	reg.SetClusterAccountGate(openTo().gate)
	keys := newFakeVendorKeys()
	keys.usable = []VendorAccount{keyAccount(VendorAnthropic, "mine", "ana"), keyAccount(VendorAnthropic, "lent", "ben")}
	reg.SetVendorKeys(keys)

	accounts, passed := reg.VendorAccountsFor(personContext("ana"), VendorAccountQuery{ActingUserId: "ana", Chosen: true}, vendorEntry(VendorAnthropic, true, ""))
	if refsOf(accounts) != "key:mine key:lent" {
		t.Fatalf("accounts = %q, want her key and the key lent to her, and not the cluster's account", refsOf(accounts))
	}
	if _, skipped := clusterPassedOver(passed); !skipped {
		t.Fatalf("passed over = %+v, want the cluster's account beside its reason", passed)
	}
}

// K10's second ask: the chain a wrapper walks names "record@cluster", and that
// reference is judged again when the call is made. A permission taken away
// between the resolution and the call is not spent.
func TestTheClustersAccountIsJudgedAgainAtTheCall(t *testing.T) {
	reg := newProviderRegistry()
	gate := openTo("olga")
	reg.SetClusterAccountGate(gate.gate)
	entry := vendorEntry(VendorAnthropic, true, "")

	if _, ok := reg.VendorAccountUsable(personContext("olga"), VendorAccountQuery{ActingUserId: "olga", Chosen: true}, entry, VendorAccountCluster); !ok {
		t.Fatal("the cluster's account was refused at the call to a person who may use it")
	}
	if _, ok := reg.VendorAccountUsable(personContext("ana"), VendorAccountQuery{ActingUserId: "ana", Chosen: true}, entry, VendorAccountCluster); ok {
		t.Fatal("the cluster's account was spent at the call for a person who may not use it")
	}
	if _, ok := reg.VendorAccountUsable(auth.ContextWithSystemActor(context.Background(), "sweep"), VendorAccountQuery{}, entry, VendorAccountCluster); !ok {
		t.Fatal("the cluster's account was refused at the call to the cluster's own work")
	}
	if _, ok := reg.VendorAccountUsable(context.Background(), VendorAccountQuery{}, entry, VendorAccountCluster); ok {
		t.Fatal("the cluster's account was spent at the call for a call that names nobody")
	}
	if _, ok := reg.VendorAccountUsable(personContext("olga"), VendorAccountQuery{ActingUserId: "olga", Chosen: true}, vendorEntry(VendorAnthropic, false, ""), VendorAccountCluster); ok {
		t.Fatal("an account the cluster does not have was usable")
	}
}

// A visitor's call borrows a person's authority (a shopper form, an access
// code, a deployed workflow). No key pays for it, and the cluster's account
// pays only when the person it borrows from may use that account.
func TestAVisitorsCallIsJudgedByThePersonItBorrowsFrom(t *testing.T) {
	reg := newProviderRegistry()
	reg.SetClusterAccountGate(openTo("olga").gate)
	keys := newFakeVendorKeys()
	keys.usable = []VendorAccount{keyAccount(VendorOpenAI, "k1", "ana")}
	reg.SetVendorKeys(keys)
	entry := vendorEntry(VendorOpenAI, true, "")

	accounts, _ := reg.VendorAccountsFor(personContext("olga"), VendorAccountQuery{ActingUserId: "olga", ForVisitor: true}, entry)
	if refsOf(accounts) != "cluster" {
		t.Fatalf("accounts = %q, want the cluster's account for a visitor of somebody who may use it", refsOf(accounts))
	}
	accounts, _ = reg.VendorAccountsFor(personContext("ana"), VendorAccountQuery{ActingUserId: "ana", ForVisitor: true}, entry)
	if len(accounts) != 0 {
		t.Fatalf("accounts = %q, want nothing for a visitor of somebody who may not use the cluster's account", refsOf(accounts))
	}
	if len(keys.queries) != 0 {
		t.Fatal("a visitor's call asked for a person's keys")
	}
}

// One resolution asks once per vendor however many of the vendor's records the
// route names, and never where the cluster has no account to open.
func TestTheGateIsAskedOncePerAnswerAndOnlyWhereThereIsAnAccount(t *testing.T) {
	reg := newProviderRegistry()
	gate := openTo("olga")
	reg.SetClusterAccountGate(gate.gate)

	answer := reg.VendorKeysAt(personContext("olga"), VendorAccountQuery{Vendor: VendorAnthropic, ActingUserId: "olga", Chosen: true})
	if gate.asks() != 0 {
		t.Fatalf("the gate was asked %d times before any record needed its answer", gate.asks())
	}
	if accounts, _ := reg.VendorAccountsWith(vendorEntry(VendorAnthropic, false, ""), answer); len(accounts) != 0 {
		t.Fatalf("accounts = %q at a record the cluster has no account for", refsOf(accounts))
	}
	if gate.asks() != 0 {
		t.Fatalf("the gate was asked %d times about an account the cluster does not have", gate.asks())
	}
	for range 3 {
		if accounts, _ := reg.VendorAccountsWith(vendorEntry(VendorAnthropic, true, ""), answer); refsOf(accounts) != "cluster" {
			t.Fatalf("accounts = %q, want the cluster's account", refsOf(accounts))
		}
	}
	if gate.asks() != 1 {
		t.Fatalf("the gate was asked %d times for one answer, want once", gate.asks())
	}
}

// The vendor door is the person's own: the same cluster, asked by two people,
// answers each of them.
func TestTheVendorDoorOpensOnTheClustersAccountOnlyForThoseWhoMayUseIt(t *testing.T) {
	reg := newProviderRegistry()
	reg.setEntry(federatedOpenAI(vendorRecord("gpt", VendorOpenAI, true, "")))
	reg.SetVendorKeys(noKeys())
	reg.SetClusterAccountGate(openTo("olga").gate)

	olga := reg.VendorDoorFor(personContext("olga"), "olga")
	if !olga.ClusterSetUp || !olga.Cluster || !olga.Chosen || !olga.Auto {
		t.Fatalf("door = %+v, want the cluster's account to serve a person who may use it", olga)
	}
	ana := reg.VendorDoorFor(personContext("ana"), "ana")
	if !ana.ClusterSetUp {
		t.Fatalf("door = %+v, want it to say the cluster has an account", ana)
	}
	if ana.Cluster || ana.Chosen || ana.Auto || ana.ChosenStreams || ana.AutoStreams {
		t.Fatalf("door = %+v, want the vendor door shut for a person who may not use the cluster's account and has no key", ana)
	}
	own := reg.VendorDoorFor(auth.ContextWithSystemActor(context.Background(), "sweep"), "")
	if !own.Cluster || !own.Chosen || !own.Auto {
		t.Fatalf("door = %+v, want the cluster's account to serve the cluster's own work", own)
	}
	nobody := reg.VendorDoorFor(context.Background(), "")
	if !nobody.ClusterSetUp || nobody.Cluster || nobody.Chosen || nobody.Auto {
		t.Fatalf("door = %+v, want the cluster's account set up and shut to a call that names nobody", nobody)
	}

	// A key of her own opens it again, on a route she chooses.
	reg.SetVendorKeys(keysHeldBy("ana", keyAccount(VendorOpenAI, "k1", "ana")))
	ana = reg.VendorDoorFor(personContext("ana"), "ana")
	if !ana.Chosen || !ana.Key || ana.Cluster || ana.Auto {
		t.Fatalf("door = %+v, want her key to serve a route she chooses, and nothing else", ana)
	}
}

// A route of vendors that cannot serve says what would open it FOR THE PERSON
// READING. Where the cluster has an account that is not theirs to spend, "a
// vendor account" is not something they can add: a key is.
func TestARouteOfVendorsSaysTheClustersAccountIsNotOpenToThePerson(t *testing.T) {
	reg := newProviderRegistry()
	gpt := federatedOpenAI(vendorRecord("gpt", VendorOpenAI, true, ""))
	// One model that serves every job: a vendor selector offers a record only
	// at the levels it declares.
	gpt.Config.Params = map[string]any{"levels": "fast strong reasoning", "rank": 500}
	reg.setEntry(gpt)
	reg.SetVendorKeys(noKeys())
	reg.SetClusterAccountGate(openTo("olga").gate)
	vendorsOnly := RouteSpec{Id: "mine", Name: "Mine", Kind: RouteKindCustom, Jobs: RouteJobs{
		Fast:      []string{"federation:cheapest"},
		Strong:    []string{"federation:strongest"},
		Reasoning: []string{"federation:strongest"},
	}}
	summaryFor := func(person string) routeSummaryJSON {
		readiness := newRouteReadiness((&MemQLEngine{providers: reg}).inferenceDoors(personContext(person)), reg)
		readiness.embeddingReachable = true
		return readiness.summarize(vendorsOnly)
	}

	if summary := summaryFor("olga"); !summary.Ready {
		t.Fatalf("summary = %+v, want the route ready for a person the cluster's account pays for", summary)
	}
	summary := summaryFor("ana")
	if summary.Ready || summary.Reason != "Needs an API key. The cluster's vendor account is not open to you" {
		t.Fatalf("summary = ready %v, reason %q; want not ready, and the one thing she can do about it", summary.Ready, summary.Reason)
	}
}
