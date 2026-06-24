package memql

// authoring_demote_propagate_test.go -- coverage for the LIVE cross-node
// propagation of durable demotions (memql#2163), the inverse of the promote
// propagation test.
//
// Asserts the CROSS-NODE path (the load-bearing test): a demote on engine A
// makes the construct UNCALLABLE on a DIFFERENT engine B WITHOUT a restart,
// purely via the authoring.demote.* broadcast. This test FAILS against
// single-node code (no demote subscriber on B) and PASSES with the propagation
// wired.

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// TestAuthoringDemotePropagation_CrossNode: two SEPARATE engines bridged by
// forwarding the authoring.demote.* broadcast from engine A's bus to engine B's
// bus. Both start with mcpSessSpec promoted in their shared registries; a durable
// demote on engine A must make the construct uncallable on engine B WITHOUT a
// restart, purely via the broadcast.
func TestAuthoringDemotePropagation_CrossNode(t *testing.T) {
	busA := events.NewBus()
	busB := events.NewBus()
	t.Cleanup(func() { busA.Close(); busB.Close() })

	// Bridge A -> B: forward authoring.demote.* from A's bus onto B's bus,
	// stamping an OriginNodeId so it reads as a remote (peer-bridged) event.
	busA.Subscribe("authoring.demote.#", func(ev events.Event) {
		ev.OriginNodeId = "node-A"
		busB.Publish(ev)
	})

	// Engine A handles the demote, removes locally, and broadcasts.
	engA := &MemQLEngine{specs: newSpecRegistry()}
	engA.SetEventBus(busA)
	storeA := seedPromotedSpec(t, engA, "owner-1")

	// Engine B: a DIFFERENT engine. Seed it with the SAME construct promoted (as
	// if it had received the original promote broadcast), so the demote has
	// something to remove there.
	engB := &MemQLEngine{specs: newSpecRegistry()}
	engB.SetEventBus(busB)
	_ = seedPromotedSpec(t, engB, "owner-1")
	if _, ok := engB.specs.Lookup("mcpSessSpec"); !ok {
		t.Fatal("engine B should have the promoted spec before the demote")
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// B subscribes to the authoring-demote broadcast; on receipt it loads the
	// bundle's (now retired) rows from the shared DB and unregisters each.
	bStore := &crossNodeRehydrateStore{row: AuthoringConstructRow{
		OwnerUserId: "owner-1", Kind: "spec", Name: "mcpSessSpec", Source: sessionSpecSrc, Status: string(BundleRetired),
	}}
	engB.startAuthoringDemoteSubscriberWithStore(ctx, bStore)

	// Demote on engine A (fake demote store, real bus broadcast).
	res, err := engA.demoteBundleDurableWithStore(ctx, storeA, "owner-1", sessionSpecSrc)
	if err != nil {
		t.Fatalf("demote on engine A: %v", err)
	}
	if !res.OK || len(res.Demoted) != 1 {
		t.Fatalf("demote on engine A did not succeed: %+v", res)
	}

	// Engine A: uncallable immediately (local removal).
	if _, ok := engA.specs.Lookup("mcpSessSpec"); ok {
		t.Fatal("demoted spec still callable on engine A (local removal failed)")
	}

	// Engine B: uncallable within seconds via the broadcast -- NO restart.
	if !eventually(2*time.Second, func() bool {
		_, ok := engB.specs.Lookup("mcpSessSpec")
		return !ok
	}) {
		t.Fatal("CROSS-NODE FAILURE: demoted spec never became uncallable on engine B " +
			"-- the authoring.demote broadcast did not propagate the demote (this is the bug the subscriber fixes)")
	}
}
