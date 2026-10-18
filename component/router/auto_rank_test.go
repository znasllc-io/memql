package router

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
)

var shippedChain = []string{memql.FleetStrongest, memql.AppWildcard, "federation:cheapest"}

// Nothing measured: the chain keeps its own local-first order at quick tasks
// and main work, so a cluster nobody measured routes exactly as before Auto.
func TestAutoWithNoSignalsKeepsTheChainsOwnOrder(t *testing.T) {
	for _, level := range []airoute.Level{airoute.LevelFast, airoute.LevelStrong} {
		got, _ := rankAutoEntries(shippedChain, level, nil, memql.RecommendedAutoBalance, "none")
		if strings.Join(got, ",") != strings.Join(shippedChain, ",") {
			t.Errorf("%s: ranked %v, want the chain's own order", level, got)
		}
	}
}

// Measured speed and reliability move a source up: a local model that has
// been failing goes behind an app that has been answering.
func TestAutoRanksByMeasuredSpeedAndReliability(t *testing.T) {
	signals := map[autoSignalKey]autoSignal{
		{Door: DoorLocal, Level: airoute.LevelStrong}: {MedianMs: 9000, Success: 0.4, Samples: 30},
		{Door: DoorApp, Level: airoute.LevelStrong}:   {MedianMs: 3000, Success: 1, Samples: 30},
	}
	got, scored := rankAutoEntries(shippedChain, airoute.LevelStrong, signals, memql.RecommendedAutoBalance, "none")
	if got[0] != memql.AppWildcard {
		t.Fatalf("ranked %v, want the fast, reliable app first", got)
	}
	if got[len(got)-1] != "federation:cheapest" {
		t.Fatalf("ranked %v, want the metered vendor last at the recommended balance", got)
	}
	if len(scored) != 3 || scored[0].Speed != 1 {
		t.Fatalf("scores %+v, want the fastest source scaled to 1", scored)
	}
}

// The balance is the owner's lever: with cost set to nothing and quality to
// everything, a strong vendor model leads a reasoning call.
func TestAutoFollowsTheBalance(t *testing.T) {
	chain := []string{memql.FleetStrongest, memql.AppWildcard, "federation:strongest"}
	got, _ := rankAutoEntries(chain, airoute.LevelReasoning, nil, memql.AutoBalance{Speed: 0, Cost: 0, Quality: 100}, "none")
	if got[0] != "federation:strongest" {
		t.Fatalf("ranked %v, want the strongest vendor first when only quality counts", got)
	}
}

// A person's monthly budget bends the order: from 80% vendors go after every
// free source whatever they score, and at 100% they are left out.
func TestAutoRespectsAPersonsMonthlyBudget(t *testing.T) {
	chain := []string{memql.FleetStrongest, memql.AppWildcard, "federation:strongest"}
	balance := memql.AutoBalance{Speed: 0, Cost: 0, Quality: 100}

	near, _ := rankAutoEntries(chain, airoute.LevelReasoning, nil, balance, "near")
	if near[len(near)-1] != "federation:strongest" {
		t.Fatalf("near the budget ranked %v, want the vendor after every free source", near)
	}
	over, _ := rankAutoEntries(chain, airoute.LevelReasoning, nil, balance, "over")
	for _, e := range over {
		if doorFor(e) == DoorFederation {
			t.Fatalf("over the budget ranked %v, want no vendor at all", over)
		}
	}
}

func TestAutoSignalsFoldTheLedger(t *testing.T) {
	rows := []map[string]any{
		{"door": "local", "servedLevel": "strong", "outcome": "ok", "totalDurationMs": 1000.0},
		{"door": "local", "servedLevel": "strong", "outcome": "ok", "totalDurationMs": 3000.0},
		{"door": "local", "servedLevel": "strong", "outcome": "error", "totalDurationMs": 50.0},
		{"door": "local", "servedLevel": "strong", "outcome": "cancelled"},
		{"door": "session", "level": "reasoning", "outcome": "ok", "totalDurationMs": 20000.0},
	}
	got := foldAutoSignals(rows)
	local := got[autoSignalKey{Door: DoorLocal, Level: airoute.LevelStrong}]
	if local.Samples != 3 || local.MedianMs != 3000 || local.Success < 0.66 || local.Success > 0.67 {
		t.Fatalf("local strong = %+v, want 3 samples (cancelled ignored), median 3000, success 2/3", local)
	}
	if app := got[autoSignalKey{Door: DoorApp, Level: airoute.LevelReasoning}]; app.Samples != 1 {
		t.Fatalf("a session call did not count for the app door: %+v", got)
	}
}

// Through the router: a general shipped chain is ranked and the decision
// record says how; a locked prompt-specific policy is walked as written.
func TestAutoRankingIsOnTheRecordAndOnlyForTheGeneralChains(t *testing.T) {
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(&stubFleetInference{models: []memql.FleetModel{fleetModel("qwen3.5:9b", true)}})
	providers.SetAppInference(&stubAppInference{doors: []memql.AppDoor{openApp("claude-code")}})
	policies := memql.NewPolicyRegistryForTest(map[string][]string{
		"localFirst": {memql.FleetStrongest, memql.AppWildcard},
		"localOnly":  {memql.FleetStrongest},
	})
	r := New(providers, policies, testRules(t, defaultRule("localFirst")), nil, nil)
	got, err := r.ResolveFor(context.Background(), ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityChat, UserId: "alice"})
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	var note string
	for _, c := range got.Resolution.Decision.Considered {
		if c.Entry == "(auto)" {
			note = c.Reason
		}
	}
	if !strings.Contains(note, "auto ranked fleet:strongest") || !strings.Contains(note, "balance speed 50, cost 50, quality 50") {
		t.Fatalf("the decision does not say how Auto ranked: %q", note)
	}

	r = New(providers, policies, testRules(t, defaultRule("localOnly")), nil, nil)
	got, err = r.ResolveFor(context.Background(), ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityChat, UserId: "alice"})
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	for _, c := range got.Resolution.Decision.Considered {
		if c.Entry == "(auto)" {
			t.Fatalf("Auto ranked a policy it does not order: %q", c.Reason)
		}
	}
}

// A vendor Auto moved to the front still asks the cost ceiling: the chain it
// walks is Auto's reordering, not a paid policy somebody chose.
func TestAVendorAutoRankedFirstStillAsksTheCeiling(t *testing.T) {
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(&stubFleetInference{})
	cloud := &countingCloud{}
	providers.RegisterWithParamsForTest("vendor", "OpenAI", "gpt", map[string]any{"contextWindow": 200000}, cloud)
	// fleet:fastest ranks below a vendor at reasoning when only quality counts,
	// so the vendor is walked FIRST -- with no local door before it, the one
	// case the ceiling would otherwise skip.
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"localFirst": {memql.FleetFastest, "vendor"}})
	policies.SetAutoBalanceForTest(memql.AutoBalance{Speed: 0, Cost: 0, Quality: 100})
	r := New(providers, policies, testRules(t, defaultRule("localFirst")), nil, nil)
	asked := 0
	r.ceilingCheck = func(context.Context) (string, bool) { asked++; return "ceiling for the test", true }

	_, err := r.ResolveFor(context.Background(), ResolveRequest{Level: airoute.LevelReasoning, Modality: airoute.ModalityChat, UserId: "alice"})
	if err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("err = %v, want the ceiling refusal", err)
	}
	if asked == 0 || cloud.calls != 0 {
		t.Fatalf("ceiling asked %d time(s), vendor called %d time(s)", asked, cloud.calls)
	}
	ranked, _ := rankAutoEntries([]string{memql.FleetFastest, "vendor"}, airoute.LevelReasoning, nil, memql.AutoBalance{Speed: 0, Cost: 0, Quality: 100}, "none")
	if ranked[0] != "vendor" {
		t.Fatalf("the fixture does not put the vendor first: %v", ranked)
	}
}
