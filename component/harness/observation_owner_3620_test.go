package harness

import (
	"context"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// memql#3620. Every observation the reconciler writes goes through
// recordHarnessObservation, whose insert stamps `ownerUserId: actor.userId`.
// The reconcile tick runs under contextWithSystemActor, which carries claims +
// TokenInfo and NO AccessContext -- so that stamp resolved to the EMPTY STRING
// and the controller minted `v1:harness:observation` rows owned by nobody, on a
// concept whose own field doc says the owner is "inherited from the parent
// step's owner".
//
// The resolver refuses that now, so the owner has to be supplied. It was
// already in hand: StepView.OwnerUserId and PlanView.OwnerUserId are read off
// the very rows being observed (adapters.go's BunStepReader projects both).

// ownerCapturingGraph records the CONTEXT each observation was written under,
// which is the thing under test -- the Observation payload never carried the
// owner, so asserting on it would measure nothing.
type ownerCapturingGraph struct {
	*fakeGraph
	mu       sync.Mutex
	obsOwner []string
}

func (g *ownerCapturingGraph) RecordObservation(ctx context.Context, obs Observation) error {
	ac, _ := auth.AccessFromContext(ctx)
	owner, _ := auth.ActorEnvelopeValue(ac, "userId")
	s, _ := owner.(string)
	g.mu.Lock()
	g.obsOwner = append(g.obsOwner, s)
	g.mu.Unlock()
	return g.fakeGraph.RecordObservation(ctx, obs)
}

func (g *ownerCapturingGraph) owners() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.obsOwner))
	copy(out, g.obsOwner)
	return out
}

func TestReconcilerWritesObservationsUnderTheStepOwner(t *testing.T) {
	base := newFakeGraph()
	graph := &ownerCapturingGraph{fakeGraph: base}
	base.addStep(StepView{
		ID: "step-1", PlanID: "plan-1", Status: StepStatusReady,
		OwnerUserId: "user-owner-3620", IdempotencyKey: "k1",
	})

	r, err := New(graph, graph, newCountDispatcher(), graph, nil, nil, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Reconcile(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := graph.owners()
	if len(got) == 0 {
		t.Fatal("no observation was written -- the test measures nothing")
	}
	for i, owner := range got {
		if owner != "user-owner-3620" {
			t.Errorf("observation %d was written under actor.userId=%q, want %q.\n"+
				"An empty owner here is the memql#3620 orphan row: recordHarnessObservation "+
				"stamps ownerUserId from actor.userId, so the write either names the step's "+
				"owner or names nobody.", i, owner, "user-owner-3620")
		}
	}
}

// ContextForOwner layers ONLY the AccessContext. createdBy must keep naming the
// reconciler: "which component wrote this row" and "whose row is this" are
// different questions, and auth.ContextWithUserActor would answer both with the
// user and erase the first.
func TestContextForOwnerKeepsTheSystemActorForCreatedBy(t *testing.T) {
	ctx := ContextForOwner(contextWithSystemActor(context.Background()), "user-owner-3620")

	if actor := auth.ActorFromContext(ctx); actor != systemActorId {
		t.Errorf("createdBy actor = %q, want %q -- the row is still the controller's write",
			actor, systemActorId)
	}
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac.UserId != "user-owner-3620" {
		t.Errorf("AccessContext = %+v, want UserId=%q -- this is what actor.userId stamps from",
			ac, "user-owner-3620")
	}
}

// A blank owner returns ctx UNCHANGED rather than inventing one. The write then
// fails loudly against a step that already has no owner, which is the correct
// end state: an observation cannot be more attributable than the step it
// observes. Silently substituting the system principal would put the
// reconciler's own name on a user's row.
func TestContextForOwnerRefusesToInventAnOwner(t *testing.T) {
	base := contextWithSystemActor(context.Background())
	for _, blank := range []string{"", "   "} {
		ctx := ContextForOwner(base, blank)
		if ac, ok := auth.AccessFromContext(ctx); ok {
			t.Errorf("ContextForOwner(%q) stamped %+v; a blank owner must leave the context "+
				"alone so the write refuses rather than inventing an owner", blank, ac)
		}
	}
}

// The system actor itself must stay AccessContext-free. If it acquires one, the
// test above stops measuring anything and the reconciler silently goes back to
// stamping a non-person as the owner of a user's rows.
func TestHarnessSystemActorCarriesNoAccessContext(t *testing.T) {
	if _, ok := auth.AccessFromContext(contextWithSystemActor(context.Background())); ok {
		t.Fatal("contextWithSystemActor must not stamp an AccessContext -- the owner comes " +
			"from the row (ContextForOwner), not from the controller")
	}
}
