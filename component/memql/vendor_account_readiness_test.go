package memql

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// vendor_account_readiness_test.go -- whether a vendor can serve a person is
// asked FOR THAT PERSON, their API keys included (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decision A8).

// vendorRecord is a text record of a vendor under its own name. available says
// whether the cluster's own account is set up for it.
func vendorRecord(name, vendor string, available bool, origin string) *ProviderConfigEntry {
	entry := vendorEntry(vendor, available, origin)
	entry.Config.Name = name
	return entry
}

// streamingVendorClient is a vendor text client that can answer Ask's plain
// stream. Readiness never calls it.
type streamingVendorClient struct{ scriptedVendorClient }

func (*streamingVendorClient) CallChatStream(context.Context, []common.ChatMessage) (<-chan common.StreamChunk, error) {
	panic("readiness must not dispatch")
}

// federatedOpenAI marks an OpenAI record as one whose cluster account is
// workload identity federation, the way the Providers page reads it.
func federatedOpenAI(entry *ProviderConfigEntry) *ProviderConfigEntry {
	entry.Config.Auth = map[string]string{
		authKeyIdentityProviderID:     "test-provider",
		authKeyOpenAIServiceAccountID: "test-account",
		authKeyOpenAITokenFile:        "/not-read-by-readiness",
	}
	return entry
}

// keysHeldBy is a key seam in which one person has these usable keys for a
// route they choose, none of them switched on for Auto, and nobody else has
// any.
func keysHeldBy(holder string, usable ...VendorAccount) *fakeVendorKeys {
	keys := newFakeVendorKeys()
	keys.holder = holder
	keys.usable = usable
	keys.autoUsable = &[]VendorAccount{}
	return keys
}

// noKeys is a key seam in which nobody has a key.
func noKeys() *fakeVendorKeys { return keysHeldBy("nobody") }

// A person whose only way to a vendor is an API key can be served on a route
// they choose. Auto spends the key only once its owner switches that on.
func TestAPersonWithAKeyAndNoFederationCanBeServed(t *testing.T) {
	reg := newProviderRegistry()
	reg.setEntry(vendorRecord("claude", VendorAnthropic, false, ""))
	keys := keysHeldBy("ana", keyAccount(VendorAnthropic, "k1", "ana"))
	reg.SetVendorKeys(keys)

	door := reg.VendorDoorFor(personContext("ana"), "ana")
	if !door.Chosen || door.Auto {
		t.Fatalf("door = %+v, want open on a chosen route and shut under Auto", door)
	}
	if !door.Key || door.Cluster {
		t.Fatalf("door = %+v, want a key to be what pays and no cluster account", door)
	}

	// The owner switches Auto on for the key.
	keys.autoUsable = &[]VendorAccount{keyAccount(VendorAnthropic, "k1", "ana")}
	if door := reg.VendorDoorFor(personContext("ana"), "ana"); !door.Chosen || !door.Auto {
		t.Fatalf("door = %+v, want open under Auto too once the key is on for it", door)
	}

	// Somebody with no key, on the same cluster, is not served.
	if door := reg.VendorDoorFor(personContext("ben"), "ben"); door.Chosen || door.Auto || door.Key {
		t.Fatalf("door = %+v for a person with no key and no cluster account, want shut", door)
	}
}

// The cluster's own account serves every person and the cluster's own work,
// on any route, with no key anywhere.
func TestFederationAloneServesEveryone(t *testing.T) {
	reg := newProviderRegistry()
	// Everyone who may use the cluster's account, which here is everybody.
	// Who may is vendor_account_cluster_test.go's subject.
	openToEverybody(reg)
	reg.setEntry(federatedOpenAI(vendorRecord("gpt", VendorOpenAI, true, "")))
	reg.SetVendorKeys(noKeys())

	for name, tc := range map[string]struct {
		ctx    context.Context
		userId string
	}{
		"a person":               {personContext("ana"), "ana"},
		"another person":         {personContext("ben"), "ben"},
		"the cluster's own work": {auth.ContextForClusterWork(context.Background(), "safety-classifier"), ""},
		"an automation":          {auth.ContextWithSystemActor(context.Background(), "sweep"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			door := reg.VendorDoorFor(tc.ctx, tc.userId)
			if !door.Chosen || !door.Auto || !door.Cluster {
				t.Fatalf("door = %+v, want the cluster's account to serve on any route", door)
			}
			if door.Key {
				t.Fatalf("door = %+v, want no key reported where nobody has one", door)
			}
		})
	}
}

// With no cluster account and no key, the vendor door is shut, and a route
// that names only vendors says what would open it.
func TestNeitherMeansTheVendorDoorIsShut(t *testing.T) {
	reg := newProviderRegistry()
	reg.setEntry(vendorRecord("claude", VendorAnthropic, false, ""))
	reg.setEntry(vendorRecord("gpt", VendorOpenAI, false, ""))
	reg.SetVendorKeys(noKeys())

	door := reg.VendorDoorFor(personContext("ana"), "ana")
	if door.Chosen || door.Auto || door.Key || door.Cluster || door.ChosenStreams {
		t.Fatalf("door = %+v, want everything shut", door)
	}

	readiness := newRouteReadiness((&MemQLEngine{providers: reg}).inferenceDoors(personContext("ana")), reg)
	readiness.embeddingReachable = true
	if _, ok := readiness.servesNow(airoute.LevelFast, "federation:cheapest"); ok {
		t.Fatal("a vendor source serves now with no account that can pay")
	}
	vendorsOnly := RouteSpec{Id: "mine", Name: "Mine", Kind: RouteKindCustom, Jobs: RouteJobs{
		Fast:      []string{"federation:cheapest"},
		Strong:    []string{"federation:strongest"},
		Reasoning: []string{"federation:strongest"},
	}}
	summary := readiness.summarize(vendorsOnly)
	if summary.Ready || summary.Reason != "Needs an API key or a vendor account" {
		t.Fatalf("summary = ready %v, reason %q; want not ready, naming both ways to open it", summary.Ready, summary.Reason)
	}
}

// The cluster's own work never spends a key, so a key never opens the door
// for it, and the keys are not even read.
func TestTheClustersOwnWorkNeverCountsAKey(t *testing.T) {
	for name, tc := range map[string]struct {
		ctx    context.Context
		userId string
	}{
		"no acting person":      {context.Background(), ""},
		"an automation":         {context.Background(), "system:automation:nightly"},
		"the cluster's actor":   {auth.ContextWithSystemActor(context.Background(), "sweep"), ""},
		"the operator's stream": {context.Background(), "cluster:operator"},
		"a visitor's call":      {auth.ContextWithVisitorBorrow(personContext("ana")), "ana"},
	} {
		t.Run(name, func(t *testing.T) {
			reg := newProviderRegistry()
			reg.setEntry(vendorRecord("claude", VendorAnthropic, false, ""))
			keys := newFakeVendorKeys()
			keys.usable = []VendorAccount{keyAccount(VendorAnthropic, "k1", "ana")}
			reg.SetVendorKeys(keys)

			door := reg.VendorDoorFor(tc.ctx, tc.userId)
			if door.Chosen || door.Auto || door.Key {
				t.Fatalf("door = %+v, want shut: only a key could pay, and this call has none", door)
			}
			if len(keys.queries) != 0 {
				t.Fatalf("the keys were read %d times for a call that can never spend one", len(keys.queries))
			}
		})
	}
}

// A key opens the door only at records it can pay through: its own vendor's,
// on a cluster that sends that vendor's calls to the vendor's own address.
func TestAKeyOpensOnlyTheRecordsItCanPayThrough(t *testing.T) {
	t.Run("another vendor's records", func(t *testing.T) {
		reg := newProviderRegistry()
		reg.setEntry(vendorRecord("gpt", VendorOpenAI, false, ""))
		reg.SetVendorKeys(keysHeldBy("ana", keyAccount(VendorAnthropic, "k1", "ana")))
		if door := reg.VendorDoorFor(personContext("ana"), "ana"); door.Chosen || door.Key {
			t.Fatalf("door = %+v, want an Anthropic key to open nothing at OpenAI", door)
		}
	})
	t.Run("a record that dials another address", func(t *testing.T) {
		reg := newProviderRegistry()
		reg.setEntry(vendorRecord("claude", VendorAnthropic, false, "https://proxy.internal"))
		reg.SetVendorKeys(keysHeldBy("ana", keyAccount(VendorAnthropic, "k1", "ana")))
		if door := reg.VendorDoorFor(personContext("ana"), "ana"); door.Chosen || door.Key {
			t.Fatalf("door = %+v, want a key that is never sent there to open nothing", door)
		}
	})
	t.Run("no key seam on this node", func(t *testing.T) {
		reg := newProviderRegistry()
		reg.setEntry(vendorRecord("claude", VendorAnthropic, false, ""))
		if door := reg.VendorDoorFor(personContext("ana"), "ana"); door.Chosen || door.Auto || door.Key {
			t.Fatalf("door = %+v, want shut on a node that cannot pay with a key", door)
		}
	})
}

// Auto is asked about only when a chosen route found a key: the keys that
// count under Auto are among the ones that count for a chosen route.
func TestTheDoorReadsAPersonsKeysOncePerVendorWhenTheyHaveNone(t *testing.T) {
	reg := newProviderRegistry()
	reg.setEntry(vendorRecord("claude", VendorAnthropic, false, ""))
	reg.setEntry(vendorRecord("claude-fast", VendorAnthropic, false, ""))
	reg.setEntry(vendorRecord("gpt", VendorOpenAI, false, ""))
	keys := noKeys()
	reg.SetVendorKeys(keys)

	reg.VendorDoorFor(personContext("ana"), "ana")
	asked := map[string]int{}
	for _, q := range keys.queries {
		if !q.Chosen {
			t.Errorf("Auto was asked about with no key to switch on: %+v", q)
		}
		if q.ActingUserId != "ana" {
			t.Errorf("the keys were read for %q, want the person asking", q.ActingUserId)
		}
		asked[q.Vendor]++
	}
	if asked[VendorAnthropic] != 1 || asked[VendorOpenAI] != 1 || len(asked) != 2 {
		t.Fatalf("the keys were read %v times per vendor, want once each", asked)
	}
}

// What Ask needs is a plain stream. A key opens it only through a record that
// can stream.
func TestAKeyOpensAskOnlyThroughARecordThatStreams(t *testing.T) {
	plain := vendorRecord("claude", VendorAnthropic, false, "")
	streaming := vendorRecord("claude-stream", VendorAnthropic, false, "")
	streaming.Client = &streamingVendorClient{}

	reg := newProviderRegistry()
	reg.setEntry(plain)
	reg.SetVendorKeys(keysHeldBy("ana", keyAccount(VendorAnthropic, "k1", "ana")))
	if door := reg.VendorDoorFor(personContext("ana"), "ana"); !door.Chosen || door.ChosenStreams {
		t.Fatalf("door = %+v, want served and not streaming through a record that cannot stream", door)
	}
	if streamingReady(t, reg, personContext("ana")) {
		t.Fatal("Ask is reported ready with no record that can stream")
	}

	reg.setEntry(streaming)
	if door := reg.VendorDoorFor(personContext("ana"), "ana"); !door.ChosenStreams {
		t.Fatalf("door = %+v, want a streaming record paid by the key to open Ask", door)
	}
	if !streamingReady(t, reg, personContext("ana")) {
		t.Fatal("a person with a key and a streaming record cannot use Ask")
	}
	if streamingReady(t, reg, personContext("ben")) {
		t.Fatal("somebody with no key was told Ask is ready")
	}
}

// The row a page reads keeps its two facts apart: whether the cluster's own
// account is set up, and whether this person has a key that can pay.
func TestInferenceStatusTellsAKeyFromTheClustersAccount(t *testing.T) {
	status := func(reg *ProviderRegistry, ctx context.Context) map[string]any {
		t.Helper()
		rows, err := (&MemQLEngine{providers: reg}).evaluateInferenceStatusExpression(ctx)
		if err != nil || len(rows) != 1 {
			t.Fatalf("inferenceStatus: %v, %v", rows, err)
		}
		return decodePayload(t, rows[0].Payload)
	}

	keyOnly := newProviderRegistry()
	keyOnly.setEntry(vendorRecord("claude", VendorAnthropic, false, ""))
	keyOnly.SetVendorKeys(keysHeldBy("ana", keyAccount(VendorAnthropic, "k1", "ana")))

	ana := status(keyOnly, personContext("ana"))
	if ana["keyAccount"] != true || ana["federationConfigured"] != false || ana["cloudConfigured"] != false {
		t.Fatalf("status = %v, want a key account and no cluster account", ana)
	}
	if ana["eligible"] != true {
		t.Fatalf("status = %v, want a person with a key to be able to get inference", ana)
	}
	if doors, _ := ana["doorsOpen"].([]any); len(doors) != 1 || doors[0] != InferenceDoorFederation {
		t.Fatalf("doorsOpen = %v, want the vendor door", ana["doorsOpen"])
	}

	ben := status(keyOnly, personContext("ben"))
	if ben["keyAccount"] != false || ben["eligible"] != false {
		t.Fatalf("status = %v for a person with no key, want nothing open", ben)
	}
	if doors, _ := ben["doorsOpen"].([]any); len(doors) != 0 {
		t.Fatalf("doorsOpen = %v for a person with no key", ben["doorsOpen"])
	}

	federated := newProviderRegistry()
	federated.setEntry(federatedOpenAI(vendorRecord("gpt", VendorOpenAI, true, "")))
	federated.SetVendorKeys(noKeys())
	federated.SetClusterAccountGate(openTo("ben").gate)
	allowed := status(federated, personContext("ben"))
	if allowed["federationConfigured"] != true || allowed["clusterAccountOpen"] != true || allowed["keyAccount"] != false || allowed["eligible"] != true {
		t.Fatalf("status = %v, want the cluster's account, open to him, and no key", allowed)
	}
	// The same cluster, asked by a person its account is closed to: it is set
	// up, it is not theirs to spend, and with no key they have no vendor.
	closed := status(federated, personContext("ana"))
	if closed["federationConfigured"] != true || closed["clusterAccountOpen"] != false || closed["eligible"] != false {
		t.Fatalf("status = %v, want a cluster account that is set up and closed to her, and nothing open", closed)
	}
	if doors, _ := closed["doorsOpen"].([]any); len(doors) != 0 {
		t.Fatalf("doorsOpen = %v for a person the cluster's account is closed to", closed["doorsOpen"])
	}
}

// Auto is ready only with something Auto may spend. A route the person
// chooses is ready with their key.
func TestRouteReadinessCountsAKeyOnlyWhereItMayBeSpent(t *testing.T) {
	auto, _ := BuiltinRouteSpec(RouteIdAuto, nil)
	vendors := RouteSpec{Id: "mine", Name: "Mine", Kind: RouteKindCustom, Jobs: RouteJobs{
		Fast:      []string{"federation:cheapest"},
		Strong:    []string{"federation:strongest"},
		Reasoning: []string{"federation:strongest"},
	}}

	// A catalog whose records declare a job, and a person whose key at
	// Anthropic is not switched on for Auto.
	reg := apiCatalog(nil)
	autoKeys := keysHeldBy("ana", keyAccount(VendorAnthropic, "k1", "ana"))
	reg.SetVendorKeys(autoKeys)
	readinessOf := func() routeReadiness {
		r := newRouteReadiness((&MemQLEngine{providers: reg}).inferenceDoors(personContext("ana")), reg)
		r.embeddingReachable = true
		return r
	}
	keyNotOnForAuto := readinessOf()
	if d := keyNotOnForAuto.doors; !d.Federation || d.FederationAuto || !d.KeyAccount {
		t.Fatalf("doors = %+v, want a key that serves a chosen route and not Auto", d)
	}
	// The reason points at the switch: this person has added a source, and
	// "needs a source" would send them looking for another one.
	if s := keyNotOnForAuto.summarize(auto); s.Ready || s.Reason != "An API key you can use is off for Auto" {
		t.Fatalf("Auto = ready %v, reason %q; want not ready while the key is off for Auto, saying so", s.Ready, s.Reason)
	}
	if s := (routeReadiness{embeddingReachable: true}).summarize(auto); s.Ready || s.Reason != "Needs a source that can answer" {
		t.Fatalf("Auto = ready %v, reason %q with nothing at all; want the plain sentence", s.Ready, s.Reason)
	}
	if s := keyNotOnForAuto.summarize(vendors); !s.Ready {
		t.Fatalf("a chosen route = not ready (%q); want the person's key to serve it", s.Reason)
	}
	for _, job := range keyNotOnForAuto.summarize(vendors).Jobs {
		if job.Job != string(airoute.LevelEmbeddings) && job.ServesNow == "" {
			t.Errorf("job %s names nothing that serves it now", job.Job)
		}
	}

	autoKeys.autoUsable = &[]VendorAccount{keyAccount(VendorAnthropic, "k1", "ana")}
	keyOnForAuto := readinessOf()
	if s := keyOnForAuto.summarize(auto); !s.Ready {
		t.Fatalf("Auto = not ready (%q); want ready once the key is on for Auto", s.Reason)
	}
}
