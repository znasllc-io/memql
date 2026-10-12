package work

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// gate_pool_wiring_test.go -- a work integration with no gate pool says which
// wiring it lacks (memql#6022, F9 fix round 4).
//
// The decision gate stopped falling back to the request pool in round 3, and the
// proving harness, which built the work spine itself without SetGateDB, failed in
// CI's proving lane at its first decision with nothing but "shared write
// coordination is unavailable". A builder that forgets the pool is now told so --
// at construction for the plug-in, at first use for New -- in words that name
// what to wire.

// TestAWorkIntegrationWithNoGatePoolNamesTheWiringItLacks: New without SetGateDB,
// and SetGateDB with a resolver that has no pool, each answer the decision gate
// with a sentence naming the missing wiring.
func TestAWorkIntegrationWithNoGatePoolNamesTheWiringItLacks(t *testing.T) {
	i := New(newRecordingEngine(), testLogger())

	release, err := i.decisionGate(context.Background(), "some-approval")
	if release != nil || !errors.Is(err, memql.ErrWriteGatePoolMissing) {
		t.Fatalf("an unwired work integration's decision gate answered %v, want ErrWriteGatePoolMissing", err)
	}
	if !strings.Contains(err.Error(), "SetGateDB") || !strings.Contains(err.Error(), "PluginContext.GateDB") {
		t.Errorf("the answer does not say which wiring is missing: %v", err)
	}

	i.SetGateDB(func() *sql.DB { return nil })
	if _, err := i.decisionGate(context.Background(), "some-approval"); !errors.Is(err, memql.ErrWriteGatePoolMissing) || !strings.Contains(err.Error(), "failed to open the write-gate pool") {
		t.Errorf("a resolver with no pool answered %v", err)
	}
	if _, err := i.OpenReviewGoal(callerContext("u-alice"), "key", DirectGoal{OwnerUserId: "u-alice", Statement: "Revise", AutomationName: "materializeFile"}, map[string]any{"revision": "r1"}, "Approve?"); !errors.Is(err, memql.ErrWriteGatePoolMissing) {
		t.Errorf("an unwired review goal answered %v, want ErrWriteGatePoolMissing", err)
	}
}

// TestTheWorkPluginRefusesAContextWithNoGatePool: the plug-in refuses a plug-in
// context without GateDB at construction, the way it refuses one without
// AdmitSourceRow, instead of materializing a spine that refuses every decision.
func TestTheWorkPluginRefusesAContextWithNoGatePool(t *testing.T) {
	for _, p := range memql.RegisteredPlugins() {
		if p.Name != "work" {
			continue
		}
		provider, err := p.Factory(memql.PluginContext{AdmitSourceRow: func(context.Context, memorynodes.MemoryNode) bool { return true }})
		if err == nil || provider != nil {
			t.Fatalf("the work plug-in materialized without a gate pool: %v", provider)
		}
		if !strings.Contains(err.Error(), "GateDB") {
			t.Errorf("the refusal does not name PluginContext.GateDB: %v", err)
		}
		return
	}
	t.Fatal("the work plug-in is not registered")
}
