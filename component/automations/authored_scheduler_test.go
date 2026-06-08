package automations_test

// authored_scheduler_test.go -- integration coverage for the per-owner
// authored automation scheduler (epic memql#954, issue #959, increment 2).
//
// External test package so it can load the full DSL concept tree (so authored
// automations whose triggers name real concepts compile) and exercise the
// scheduler against a real event bus + the real automation loader -- the same
// engine-backed style as the #956 sandbox tests.
//
// The north-star slice of the acceptance test lives here: register an authored
// automation AT RUNTIME, publish its trigger, and observe it FIRE -- under the
// AUTHOR's authz envelope, never the system actor -- then deactivate it live
// and observe it stop firing.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

func loadConceptsForAuthored(t *testing.T) {
	t.Helper()
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
}

// runRecorder captures each authored automation firing: the actor the run
// executed as (pulled out of the author-scoped ctx) and the triggering event.
type runRecorder struct {
	mu    sync.Mutex
	fired []recordedRun
}

type recordedRun struct {
	automation string
	actorUser  string
	actorRole  auth.Role
	eventTopic string
}

func (r *runRecorder) run(ctx context.Context, a *automations.Automation, ev *events.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := recordedRun{automation: a.Name}
	if ac, ok := auth.AccessFromContext(ctx); ok {
		rec.actorUser = ac.UserId
		rec.actorRole = ac.Role
	}
	if ev != nil {
		rec.eventTopic = ev.Topic
	}
	r.fired = append(r.fired, rec)
	return nil
}

func (r *runRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.fired)
}

func (r *runRecorder) last() recordedRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.fired) == 0 {
		return recordedRun{}
	}
	return r.fired[len(r.fired)-1]
}

func newAuthoredSchedulerForTest(t *testing.T, bus *events.Bus, run automations.RunAutomationFunc) *automations.AuthoredScheduler {
	t.Helper()
	loader := automations.NewLoader(automations.LoaderOptions{Registry: concept.DefaultRegistry()})
	s, err := automations.NewAuthoredScheduler(automations.AuthoredSchedulerOptions{
		Loader:   loader,
		EventBus: bus,
		Run:      run,
	})
	if err != nil {
		t.Fatalf("NewAuthoredScheduler: %v", err)
	}
	return s
}

func authoredAutomationConstruct(owner, name, source string) *memql.AuthoredConstruct {
	return &memql.AuthoredConstruct{
		OwnerUserId: owner,
		Kind:        "automation",
		Name:        name,
		BundleId:    "v1:authoring:bundle:test",
		Version:     1,
		Source:      source,
	}
}

// TestAuthoredScheduler_EventTrigger_FiresUnderAuthorEnvelope is the
// increment-2 north star: an authored automation registered at runtime fires
// on its event trigger, running under the AUTHOR's authz envelope (their
// userId + a non-privileged writer role -- NOT the system actor).
func TestAuthoredScheduler_EventTrigger_FiresUnderAuthorEnvelope(t *testing.T) {
	loadConceptsForAuthored(t)
	bus := events.NewBus()
	rec := &runRecorder{}
	s := newAuthoredSchedulerForTest(t, bus, rec.run)
	defer s.Stop()

	const owner = "v1:identity:user:alice"
	src := `@enabled
@trigger(event="node.created", concept="v1:identity:user")
@description("Authored: react to user creation")
automation aliceOnUserCreate {
  step run {
    logic sandboxNoopLogic { event: event }
  }
}`

	// Register AT RUNTIME -- no restart.
	if err := s.Activate(authoredAutomationConstruct(owner, "aliceOnUserCreate", src)); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !s.IsActive(owner, "aliceOnUserCreate") {
		t.Fatal("expected the authored automation to be active after Activate")
	}

	// Fire its trigger. The compiled trigger keys off the canonical CDC topic
	// for v1:identity:user creation; publish synchronously so the assertion is
	// deterministic.
	bus.PublishSync(events.NewEvent("graph.node.created.v1:identity:user", events.KindNodeCreated, map[string]any{"id": "v1:identity:user:bob"}))

	if rec.count() != 1 {
		t.Fatalf("expected the authored automation to fire exactly once, got %d", rec.count())
	}
	got := rec.last()
	if got.automation != "aliceOnUserCreate" {
		t.Errorf("wrong automation fired: %q", got.automation)
	}
	// No-escalation: the run executed as the AUTHOR (alice), writer role.
	if got.actorUser != owner {
		t.Errorf("authored run must execute as the author %q, got %q", owner, got.actorUser)
	}
	if got.actorRole != auth.RoleWriter {
		t.Errorf("authored run must NOT escalate -- expected writer role, got %q", got.actorRole)
	}

	// Deactivate LIVE -- it stops firing.
	s.Deactivate(owner, "aliceOnUserCreate")
	if s.IsActive(owner, "aliceOnUserCreate") {
		t.Fatal("expected the automation to be inactive after Deactivate")
	}
	bus.PublishSync(events.NewEvent("graph.node.created.v1:identity:user", events.KindNodeCreated, map[string]any{"id": "v1:identity:user:carol"}))
	if rec.count() != 1 {
		t.Errorf("deactivated automation must not fire, got %d total firings", rec.count())
	}
}

// TestAuthoredScheduler_OwnerScopedIsolation: two owners can each activate an
// identically-named authored automation; they fire independently and each runs
// under its own author envelope. Deactivating one leaves the other live.
func TestAuthoredScheduler_OwnerScopedIsolation(t *testing.T) {
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
		t.Fatalf("activate a: %v", err)
	}
	if err := s.Activate(authoredAutomationConstruct("user-b", "onUser", src)); err != nil {
		t.Fatalf("activate b: %v", err)
	}
	if s.ActiveCount() != 2 {
		t.Fatalf("expected 2 active authored automations, got %d", s.ActiveCount())
	}

	bus.PublishSync(events.NewEvent("graph.node.created.v1:identity:user", events.KindNodeCreated, map[string]any{"id": "x"}))
	// Both owners' automations fire on the same event.
	if rec.count() != 2 {
		t.Fatalf("expected both owners' automations to fire, got %d", rec.count())
	}
	seen := map[string]bool{}
	rec.mu.Lock()
	for _, f := range rec.fired {
		seen[f.actorUser] = true
	}
	rec.mu.Unlock()
	if !seen["user-a"] || !seen["user-b"] {
		t.Errorf("each automation must run under its own author envelope, saw: %v", seen)
	}

	// Deactivating one owner's automation leaves the other live.
	s.Deactivate("user-a", "onUser")
	if !s.IsActive("user-b", "onUser") || s.IsActive("user-a", "onUser") {
		t.Fatal("deactivation must be owner-scoped")
	}
}

// TestAuthoredScheduler_ScheduledTrigger fires on a sub-second cron so the test
// observes a real scheduled authored firing without a long wait.
func TestAuthoredScheduler_ScheduledTrigger(t *testing.T) {
	loadConceptsForAuthored(t)
	bus := events.NewBus()
	rec := &runRecorder{}
	s := newAuthoredSchedulerForTest(t, bus, rec.run)
	defer s.Stop()

	// Every second.
	src := `@enabled
@trigger(schedule="* * * * * *")
automation everySecondSweep {
  step run {
    logic sandboxNoopLogic { event: event }
  }
}`
	if err := s.Activate(authoredAutomationConstruct("user-a", "everySecondSweep", src)); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rec.count() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if rec.count() < 1 {
		t.Fatal("expected the scheduled authored automation to fire at least once")
	}
	if got := rec.last(); got.actorUser != "user-a" {
		t.Errorf("scheduled run must execute under the author envelope, got %q", got.actorUser)
	}
}

// TestAuthoredScheduler_RejectsNonAutomation: only automation-kind constructs
// are schedulable.
func TestAuthoredScheduler_RejectsNonAutomation(t *testing.T) {
	loadConceptsForAuthored(t)
	bus := events.NewBus()
	s := newAuthoredSchedulerForTest(t, bus, (&runRecorder{}).run)
	defer s.Stop()

	err := s.Activate(&memql.AuthoredConstruct{
		OwnerUserId: "user-a", Kind: "query", Name: "q", Source: "// q", Version: 1,
	})
	if err == nil {
		t.Fatal("expected a non-automation construct to be rejected")
	}
}

// TestAuthoredScheduler_CompileErrorSurfaces: a malformed automation source
// fails Activate rather than silently registering a dead trigger.
func TestAuthoredScheduler_CompileErrorSurfaces(t *testing.T) {
	loadConceptsForAuthored(t)
	bus := events.NewBus()
	s := newAuthoredSchedulerForTest(t, bus, (&runRecorder{}).run)
	defer s.Stop()

	// Missing closing brace.
	src := `@trigger(schedule="* * * * * *")
automation broken {
  step run {
    logic sandboxNoopLogic { event: event }
}`
	if err := s.Activate(authoredAutomationConstruct("user-a", "broken", src)); err == nil {
		t.Fatal("expected Activate to surface a compile error")
	}
	if s.ActiveCount() != 0 {
		t.Errorf("a failed Activate must not leave a live entry, got %d", s.ActiveCount())
	}
}
