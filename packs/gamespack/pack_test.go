package gamespack

import (
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// registerOnce registers the pack the way packs/anchor does, once per process:
// dsl.RegisterTree panics on a second registration of one namespace.
var registerOnce sync.Once

// The pack holds memories about the people who play; holding them is an
// owner's decision, so it ships disabled.
func TestPackShipsDisabled(t *testing.T) {
	registerOnce.Do(func() { Register(Domain) })
	if enabled, declared := memqldsl.PackDefaults()[Domain]; !declared || enabled {
		t.Fatalf("the games pack must declare itself disabled, got declared=%v enabled=%v", declared, enabled)
	}
	if memqldsl.IsStorefrontPack(Domain) {
		t.Fatal("the games pack is not a storefront pack: only an owner may switch it on")
	}
}

// Every concept loads, with the retention the design record states: raw
// evidence expires whole after 30 days, everything else keeps its newest
// version, and nothing is archived.
func TestConceptsLoadWithTheirRetention(t *testing.T) {
	registerOnce.Do(func() { Register(Domain) })
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	want := map[string]bool{ // concept -> keepLatest
		conceptPlayer: true, conceptEpisode: false, conceptPreference: true, conceptPromise: true,
		conceptPlace: true, conceptSkill: true, conceptPlanTrace: false,
	}
	for id, keepLatest := range want {
		c, err := memorynodes.DefaultRegistry().Get(id)
		if err != nil {
			t.Fatalf("%s is not registered: %v", id, err)
		}
		r := c.Retention
		if r == nil || r.TTL != 30*24*time.Hour || r.KeepLatest != keepLatest || r.Archive != "none" {
			t.Errorf("%s retention = %+v, want ttl 720h, keepLatest %v, archive none", id, r, keepLatest)
		}
	}
}

// The two concepts the learning ladder lives on are written by the pack's Go
// half and by nothing else, and that has to be a fact about the CONCEPT:
// @serverOnly on a mutation closes that one mutation, while a raw insert() or
// update() is a public expression the owned tier admits for the row's owner.
// @serverWritten is checked at the engine's single write seam, whichever
// construct arrives there. The other five concepts are written by the runtime
// and the owner's browser as the owner, by design, so they must not carry it.
func TestOnlyTheLearningConceptsAreServerWritten(t *testing.T) {
	registerOnce.Do(func() { Register(Domain) })
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	want := map[string]bool{ // concept -> serverWritten
		conceptPlayer: false, conceptEpisode: false, conceptPreference: false, conceptPromise: false,
		conceptPlace: false, conceptSkill: true, conceptPlanTrace: true,
	}
	for id, serverWritten := range want {
		c, err := memorynodes.DefaultRegistry().Get(id)
		if err != nil {
			t.Fatalf("%s is not registered: %v", id, err)
		}
		switch {
		case c.ServerWritten == serverWritten:
		case serverWritten:
			t.Errorf("%s ServerWritten = false, want true: @serverOnly closes only its named writer, so the owner's raw insert() or update() still reaches this concept", id)
		default:
			t.Errorf("%s ServerWritten = true, want false: the runtime and the owner's browser write this concept as the owner", id)
		}
	}
}

// The pack takes write gates -- learning, and every ladder move -- and a gate
// resolves its pool from PluginContext.GateDB. The contract on that field is
// that a pack which takes gates REFUSES a context without it, at construction:
// the alternative is a pack that records traces and answers lookups until its
// first learning decision, and only then says that shared write coordination is
// unavailable.
func TestNewProviderRefusesAContextWithoutAGatePool(t *testing.T) {
	provider, err := NewProvider(memql.PluginContext{})
	if err == nil || provider != nil {
		t.Fatalf("NewProvider built a pack with no GateDB: %v", provider)
	}
	if !strings.Contains(err.Error(), "GateDB") {
		t.Errorf("the refusal does not name PluginContext.GateDB: %v", err)
	}

	// The control: the same context with a gate-pool resolver builds the pack, so
	// the refusal above is the missing field's doing. The resolver may answer nil
	// (the database has not started yet); that is the first gate's refusal to
	// make, with its own words, and not the constructor's.
	provider, err = NewProvider(memql.PluginContext{GateDB: func() *sql.DB { return nil }})
	if err != nil || provider == nil {
		t.Fatalf("NewProvider with a GateDB = %v, %v, want a pack", provider, err)
	}
	if p, ok := provider.(*Provider); !ok || p.gates == nil {
		t.Fatalf("the pack did not keep the gate-pool resolver it was given: %#v", provider)
	}
}
