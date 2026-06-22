package planner

import (
	"context"
	"testing"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/events"
)

// TestAdmissionUsesDirectGetter_FairnessUsesPooled asserts the pooling
// endpoint split (memql#1931 / epic #1925): the admission controller -- which
// holds SESSION-SCOPED advisory locks across statements -- resolves its
// *bun.DB through the DIRECT (non-pooled) getter, while the across-account
// fairness cron, which only does bulk pooled reads, stays on the pooled
// getter. Distinct sentinel *bun.DB values let us assert which getter each
// component received by pointer identity.
func TestAdmissionUsesDirectGetter_FairnessUsesPooled(t *testing.T) {
	pooled := &bun.DB{}
	direct := &bun.DB{}
	pooledGetter := func() *bun.DB { return pooled }
	directGetter := func() *bun.DB { return direct }

	p, err := NewPlannerIntegration(
		context.Background(),
		WithEngine(&fakeEngine{}),
		WithEventBus(events.NewBus()),
		WithLogger(testLogger()),
		WithDBGetter(pooledGetter),
		WithDirectDBGetter(directGetter),
	)
	if err != nil {
		t.Fatalf("NewPlannerIntegration: %v", err)
	}

	if p.admission == nil || p.admission.dbGetter == nil {
		t.Fatal("admission controller missing a db getter")
	}
	if got := p.admission.dbGetter(); got != direct {
		t.Errorf("admission controller must resolve via the DIRECT (non-pooled) getter; got %p want %p", got, direct)
	}

	if p.fairnessCron == nil || p.fairnessCron.dbGetter == nil {
		t.Fatal("fairness cron missing a db getter")
	}
	if got := p.fairnessCron.dbGetter(); got != pooled {
		t.Errorf("fairness cron must stay on the POOLED getter (bulk reads, no session lock); got %p want %p", got, pooled)
	}
}

// TestAdmissionFallsBackToPooledGetter asserts that when no direct getter is
// wired (older callers / tests), the admission controller falls back to the
// pooled getter. DirectBunDB itself falls back to the main pool when
// DIRECT_DSN is unset, so the end-to-end effect is unchanged behavior.
func TestAdmissionFallsBackToPooledGetter(t *testing.T) {
	pooled := &bun.DB{}
	pooledGetter := func() *bun.DB { return pooled }

	p, err := NewPlannerIntegration(
		context.Background(),
		WithEngine(&fakeEngine{}),
		WithEventBus(events.NewBus()),
		WithLogger(testLogger()),
		WithDBGetter(pooledGetter),
		// no WithDirectDBGetter
	)
	if err != nil {
		t.Fatalf("NewPlannerIntegration: %v", err)
	}

	if p.admission == nil || p.admission.dbGetter == nil {
		t.Fatal("admission controller missing a db getter")
	}
	if got := p.admission.dbGetter(); got != pooled {
		t.Errorf("admission controller must fall back to the pooled getter when no direct getter is wired; got %p want %p", got, pooled)
	}
}
