package router

// auto_accounts_test.go -- Auto is unchanged except for who pays (design
// record docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// R7), checked over the policies and the vendor catalog THIS TREE SHIPS, not
// over a fixture's stand-ins: a chain whose vendor hop expands to nothing at
// some level is a defect of the shipped files, and only they can show it.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
)

// shippedRouting is the provider catalog and the policies this tree ships,
// loaded as the engine loads them. Nothing here reaches a vendor: a record is
// registered with its client, and no call is made.
func shippedRouting(t *testing.T) (*memql.ProviderRegistry, *memql.PolicyRegistry) {
	t.Helper()
	// No ambient credential of an SDK's own may answer for the engine.
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "OPENAI_ORG_ID", "OPENAI_PROJECT_ID"} {
		t.Setenv(name, "")
	}
	providers := memql.NewProviderRegistryForTest()
	if _, err := memql.LoadUnifiedProviders(nil, providers); err != nil {
		t.Fatalf("load the shipped providers: %v", err)
	}
	policies := memql.NewPolicyRegistryForTest(nil)
	if _, err := memql.LoadUnifiedPolicies(nil, policies); err != nil {
		t.Fatalf("load the shipped policies: %v", err)
	}
	return providers, policies
}

var routedLevels = []airoute.Level{airoute.LevelFast, airoute.LevelStrong, airoute.LevelReasoning}

// Auto orders the same three general chains it always did. With nothing
// measured it keeps a quick task's chain in its own order and, at every level,
// leaves the vendor last: a vendor costs, and nothing measured says it is
// worth it. (Above quick tasks an app is ranked ahead of a local model on
// quality alone, which is Auto's own rule and older than accounts.)
func TestAutoStillRanksItsThreeShippedChains(t *testing.T) {
	_, policies := shippedRouting(t)
	want := map[string]bool{"localFirst": true, "fastLocalFirst": true, "federationStrongest": true}
	if !reflect.DeepEqual(autoRankedPolicies, want) {
		t.Fatalf("Auto ranks %v, want exactly %v", autoRankedPolicies, want)
	}
	for name := range want {
		cfg, ok := policies.Lookup(name)
		if !ok {
			t.Fatalf("the shipped policies have no %s for Auto to rank", name)
		}
		chain := cfg.ProviderChain()
		if len(chain) != 3 || doorFor(chain[0]) != DoorLocal || doorFor(chain[1]) != DoorApp || doorFor(chain[2]) != DoorFederation {
			t.Fatalf("%s is %v, want a local model, then an app, then a vendor", name, chain)
		}
		for _, level := range routedLevels {
			ranked, _ := rankAutoEntries(chain, level, nil, memql.RecommendedAutoBalance, "none")
			if len(ranked) != len(chain) || ranked[len(ranked)-1] != chain[2] {
				t.Errorf("%s at %s, with nothing measured, ranks %v; want every source of %v, the vendor last", name, level, ranked, chain)
			}
			if level == airoute.LevelFast && !reflect.DeepEqual(ranked, chain) {
				t.Errorf("%s at %s, with nothing measured, ranks %v; want its own order %v", name, level, ranked, chain)
			}
		}
	}
}

// Every vendor hop of every shipped chain has a model to offer at every level
// a call can ask for. A selector that expanded to nothing would make the hop
// a place no call can be served from, whatever accounts a person has.
func TestEveryShippedChainsVendorHopFindsAModelAtEveryLevel(t *testing.T) {
	providers, policies := shippedRouting(t)
	hops := 0
	for _, cfg := range policies.All() {
		for _, entry := range cfg.ProviderChain() {
			selector, isVendor := strings.CutPrefix(entry, "federation:")
			if !isVendor {
				continue
			}
			hops++
			switch selector {
			case memql.VendorSelectorStrongest, memql.VendorSelectorCheapest:
				for _, level := range routedLevels {
					ordered, _ := providers.VendorRecordsFor(level, selector)
					if len(ordered) == 0 {
						t.Errorf("%s: %s offers no model at %s", cfg.Name, entry, level)
						continue
					}
					vendors := map[string]bool{}
					for _, name := range ordered {
						e, _ := providers.Entry(name)
						vendors[memql.VendorOfEntry(e)] = true
					}
					if !vendors[memql.VendorAnthropic] || !vendors[memql.VendorOpenAI] {
						t.Errorf("%s: %s at %s offers %v, which leaves a person with one vendor's key nothing to pay for", cfg.Name, entry, level, ordered)
					}
				}
			default:
				if _, ok := providers.Entry(selector); !ok {
					t.Errorf("%s names %s, which is not a record this tree ships", cfg.Name, entry)
				}
			}
		}
	}
	if hops < 4 {
		t.Fatalf("read %d vendor hops from the shipped policies; the files moved or this is measuring nothing", hops)
	}
}

// shippedFixture is a router over the shipped catalog and policies, with a
// stand-in key seam, and the cluster's own account set up nowhere: the state
// of every cluster nobody connected a vendor on.
func shippedFixture(t *testing.T, keys ...lentKey) (*Router, *accountKeys) {
	t.Helper()
	providers, policies := shippedRouting(t)
	seam := &accountKeys{keys: keys}
	providers.SetVendorKeys(seam)
	providers.SetFleetInference(&stubFleetInference{})
	providers.SetAppInference(&stubAppInference{})
	return New(providers, policies, testRules(t, defaultRule("localFirst")), nil, nil), seam
}

// End to end over the shipped chain a default call walks: with no local model
// and no app, a person's key pays under Auto only once its owner switched
// that on, and then for the model the chain's selector picks at the call's
// level.
func TestAutoSpendsAKeyOnlyWhenItsOwnerSwitchedThatOn(t *testing.T) {
	// Off for Auto: the key is not spent, and the call is refused.
	r, seam := shippedFixture(t, aliceOwnKey)
	_, _, err := r.resolveChat(personCtx(alice), autoRequest(airoute.ModalityChat))
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) || refusal.Code != work.RefusalEveryDoorShut {
		t.Fatalf("err = %v, want every door shut while the key is off for Auto", err)
	}
	for _, q := range seam.asked() {
		if q.Chosen {
			t.Fatalf("Auto asked for the keys of a route somebody chose: %+v", q)
		}
	}

	// On for Auto: the cheapest strong Claude model, paid by the key.
	r, _ = shippedFixture(t, switchedOnForAuto(aliceOwnKey))
	_, resolved, err := r.resolveChat(personCtx(alice), autoRequest(airoute.ModalityChat))
	if err != nil {
		t.Fatalf("Auto with a key switched on: %v", err)
	}
	if len(resolved.Chain) == 0 || !strings.HasSuffix(resolved.Chain[0], "@key:k-own") {
		t.Fatalf("chain = %v, want a record paid by the person's key first", resolved.Chain)
	}
	record, _ := memql.SplitAccountReference(resolved.Chain[0])
	if resolved.ProviderName != record || resolved.Vendor != "anthropic" {
		t.Fatalf("resolved %q at %q, want the Claude record %s", resolved.ProviderName, resolved.Vendor, record)
	}
	if got := accountOf(resolved.Decision); got[0] != memql.VendorAccountKey || got[1] != "k-own" {
		t.Fatalf("the decision says %v paid, want the person's key", got)
	}
	// And it is the model the selector means: among the records that serve
	// strong, at the vendor she has a key for, the cheapest.
	cheapest, _ := r.providers.VendorRecordsFor(airoute.LevelStrong, memql.VendorSelectorCheapest)
	for _, name := range cheapest {
		e, _ := r.providers.Entry(name)
		if memql.VendorOfEntry(e) == memql.VendorAnthropic {
			if record != name {
				t.Fatalf("the key paid for %s; the cheapest strong Claude record is %s", record, name)
			}
			break
		}
	}
}

// Every gate that stood in front of a vendor still stands, and is asked before
// any account is considered: a key is a paid provider (R7).
func TestEveryGateIsAskedBeforeAnAccount(t *testing.T) {
	t.Run("a run that forbids paid providers", func(t *testing.T) {
		r, seam := shippedFixture(t, switchedOnForAuto(aliceOwnKey))
		req := autoRequest(airoute.ModalityChat)
		req.NoPaidProviders = true
		_, _, err := r.resolveChat(personCtx(alice), req)
		var refusal *InferenceUnavailable
		if !errors.As(err, &refusal) {
			t.Fatalf("err = %v, want a refusal", err)
		}
		if asked := seam.asked(); len(asked) != 0 {
			t.Errorf("accounts were considered (%+v) for a source the run forbids", asked)
		}
	})

	t.Run("the cost ceiling", func(t *testing.T) {
		r, seam := shippedFixture(t, switchedOnForAuto(aliceOwnKey))
		r.ceilingCheck = func(ctx context.Context) (string, bool) { return "the ceiling of this test", true }
		_, _, err := r.resolveChat(personCtx(alice), autoRequest(airoute.ModalityChat))
		var refusal *InferenceUnavailable
		if !errors.As(err, &refusal) || refusal.Code != work.RefusalCeilingReached {
			t.Fatalf("err = %v, want the ceiling's refusal", err)
		}
		if asked := seam.asked(); len(asked) != 0 {
			t.Errorf("accounts were considered (%+v) past a reached ceiling", asked)
		}
	})

	t.Run("a chain that names no vendor", func(t *testing.T) {
		// localOnly is a shipped chain of local models and nothing else. A key
		// does not make it reach a vendor: only the person's consent for the
		// call does, and this call carries none.
		providers, policies := shippedRouting(t)
		seam := &accountKeys{keys: []lentKey{switchedOnForAuto(aliceOwnKey)}}
		providers.SetVendorKeys(seam)
		providers.SetFleetInference(&stubFleetInference{})
		r := New(providers, policies, testRules(t, defaultRule("localOnly")), nil, nil)
		_, _, err := r.resolveChat(personCtx(alice), autoRequest(airoute.ModalityChat))
		var refusal *InferenceUnavailable
		if !errors.As(err, &refusal) || refusal.Code != work.RefusalEveryDoorShut {
			t.Fatalf("err = %v, want every door shut", err)
		}
		if asked := seam.asked(); len(asked) != 0 {
			t.Errorf("accounts were considered (%+v) on a chain that names no vendor", asked)
		}
	})
}
