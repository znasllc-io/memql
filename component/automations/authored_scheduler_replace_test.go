package automations_test

// authored_scheduler_replace_test.go -- an Activate that fails leaves whatever
// was wired under the name wired (memql#6022).
//
// The arming code registers a newer version of an authored automation OVER the
// one a node holds, and keeps the held one when the newer one is refused on
// this node -- so a version that cannot register here does not take the live
// one down with it, pass after pass of the reconcile loop. That only holds if
// a refused Activate changes nothing; it used to tear the old subscription down
// first and fail afterwards for anything it checked late.

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

func TestAnActivateThatFailsLeavesTheWiredAutomationRunning(t *testing.T) {
	concept, err := memorynodes.ParseConceptMemQL([]byte(`concept request { title string }`), "v1/forge/request")
	if err != nil {
		t.Fatal(err)
	}
	registry := memorynodes.NewRegistry(map[string]*memorynodes.Concept{"v1:forge:request": concept})
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	var runs atomic.Int64
	// No before-write engine: a before-write automation is refused here, after
	// it compiles -- the late refusal this test is about.
	s, err := automations.NewAuthoredScheduler(automations.AuthoredSchedulerOptions{
		Loader:   automations.NewLoader(automations.LoaderOptions{Registry: registry}),
		EventBus: bus,
		Run: func(context.Context, *automations.Automation, *events.Event) error {
			runs.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)

	wired := &memql.AuthoredConstruct{OwnerUserId: "owner-a", Kind: "automation", Name: "adjust", Version: 1,
		Source: "@trigger(event=\"node.created\", concept=\"v1:forge:request\")\nautomation adjust { return true }"}
	if err := s.Activate(wired); err != nil {
		t.Fatalf("setup: activate the wired version: %v", err)
	}

	refused := &memql.AuthoredConstruct{OwnerUserId: "owner-a", Kind: "automation", Name: "adjust", Version: 2,
		Source: "@trigger(before=\"write\", concept=\"v1:forge:request\")\nautomation adjust { row.title = \"adjusted\" }"}
	if err := s.Activate(refused); err == nil {
		t.Fatal("setup: a before-write automation must be refused by a scheduler with no before-write engine")
	}

	if !s.IsActive("owner-a", "adjust") {
		t.Fatal("a refused Activate tore down the version that was already wired")
	}
	bus.PublishSync(events.NewEvent("graph.node.created.v1:forge:request", events.KindNodeCreated, map[string]any{"id": "r1"}))
	if runs.Load() != 1 {
		t.Fatalf("the wired version fired %d times after a refused replacement, want 1", runs.Load())
	}
}
