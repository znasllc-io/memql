package automations_test

// authored_global_killswitch_test.go -- coverage for the cluster-wide GLOBAL
// KILL SWITCH for authored automations (epic memql#954, issue #961, increment
// 3). When the global gate reports the authored runtime disabled, NO authored
// automation fires for ANY owner; flipping it back resumes every owner's
// automations without a re-activation. Reuses the authored-scheduler harness.

import (
	"sync/atomic"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
)

// TestAuthoredScheduler_GlobalKillSwitchHaltsAll: with the global gate off,
// every owner's authored automation is suppressed on its trigger; flipping the
// gate on resumes them -- a single switch halts (and resumes) the whole
// authored runtime cluster-wide.
func TestAuthoredScheduler_GlobalKillSwitchHaltsAll(t *testing.T) {
	loadConceptsForAuthored(t)
	bus := events.NewBus()
	rec := &runRecorder{}

	// globalEnabled is the in-memory stand-in for
	// v1:identity:clusterSettings.authoredAutomationsEnabled; the gate reads it
	// live so a flip takes effect immediately, exactly like the engine-backed
	// gate the app wires.
	var globalEnabled atomic.Bool
	globalEnabled.Store(true)

	loader := automations.NewLoader(automations.LoaderOptions{Registry: concept.DefaultRegistry()})
	s, err := automations.NewAuthoredScheduler(automations.AuthoredSchedulerOptions{
		Loader:     loader,
		EventBus:   bus,
		Run:        rec.run,
		GlobalGate: func() bool { return globalEnabled.Load() },
	})
	if err != nil {
		t.Fatalf("NewAuthoredScheduler: %v", err)
	}
	defer s.Stop()

	src := `@enabled
@trigger(event="node.created", concept="v1:identity:user")
automation onUser {
  step run {
    logic sandboxNoopLogic { event: event }
  }
}`
	// Two different owners' automations, both active.
	if err := s.Activate(authoredAutomationConstruct("user-a", "onUser", src)); err != nil {
		t.Fatalf("activate a: %v", err)
	}
	if err := s.Activate(authoredAutomationConstruct("user-b", "onUser", src)); err != nil {
		t.Fatalf("activate b: %v", err)
	}

	fire := func() {
		bus.PublishSync(events.NewEvent("graph.node.created.v1:identity:user", events.KindNodeCreated, map[string]any{"id": "x"}))
	}

	// Switch ON: both owners fire.
	fire()
	if rec.count() != 2 {
		t.Fatalf("with the global switch on, both owners should fire, got %d", rec.count())
	}

	// Flip the GLOBAL KILL SWITCH OFF: a subsequent trigger fires NOTHING -- not
	// owner-a, not owner-b. The automations stay registered (active) but the
	// runtime is halted cluster-wide.
	globalEnabled.Store(false)
	fire()
	if rec.count() != 2 {
		t.Fatalf("the global kill switch must halt ALL authored automations; expected no new firings, got total %d", rec.count())
	}
	if !s.IsActive("user-a", "onUser") || !s.IsActive("user-b", "onUser") {
		t.Error("a global halt must not unregister automations -- they stay active, just suppressed")
	}

	// Flip it back ON: every owner resumes without a re-activation.
	globalEnabled.Store(true)
	fire()
	if rec.count() != 4 {
		t.Fatalf("resuming the global switch must resume ALL authored automations, got total %d", rec.count())
	}
}

// TestAuthoredScheduler_SetGlobalGateLive: the gate can be installed after
// construction (the app wires it once the engine is up) and takes effect on the
// next firing.
func TestAuthoredScheduler_SetGlobalGateLive(t *testing.T) {
	loadConceptsForAuthored(t)
	bus := events.NewBus()
	rec := &runRecorder{}
	s := newAuthoredSchedulerForTest(t, bus, rec.run)
	defer s.Stop()

	src := `@enabled
@trigger(event="node.created", concept="v1:identity:user")
automation onUser {
  step run {
    logic sandboxNoopLogic { event: event }
  }
}`
	if err := s.Activate(authoredAutomationConstruct("user-a", "onUser", src)); err != nil {
		t.Fatalf("activate: %v", err)
	}

	// No gate yet -> fires.
	bus.PublishSync(events.NewEvent("graph.node.created.v1:identity:user", events.KindNodeCreated, map[string]any{"id": "x"}))
	if rec.count() != 1 {
		t.Fatalf("expected 1 firing before any gate, got %d", rec.count())
	}

	// Install a disabling gate live -> next firing is suppressed.
	s.SetGlobalGate(func() bool { return false })
	bus.PublishSync(events.NewEvent("graph.node.created.v1:identity:user", events.KindNodeCreated, map[string]any{"id": "y"}))
	if rec.count() != 1 {
		t.Errorf("a live-installed disabling gate must suppress firings, got %d", rec.count())
	}
}
