package automations

// G4 (event-payload-binding ADR Decision 4, memql#2366) tests: the automation
// envelope exposes event.actor (from the event Metadata's acting identity)
// and event.timestamp (RFC3339 occurrence time), consistently across the
// executor envelope, the scheduler @filter binding, and checkpoint resume.

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

func actorEvent(actorId string) events.Event {
	ev := events.NewEvent("graph.node.created.v1:cluster:deployment", events.KindNodeCreated,
		map[string]any{"id": "v1:cluster:deployment:d1", "environment": "development"})
	if actorId != "" {
		ev = ev.WithMetadata("actor", actorId)
	}
	return ev
}

func TestBuildEventEnvelope_ActorPresent(t *testing.T) {
	ev := actorEvent("user:alice")
	env := buildEventEnvelope(&ev, "", "")
	actor, ok := env["actor"].(map[string]any)
	if !ok {
		t.Fatalf("event.actor missing or wrong type: %v", env["actor"])
	}
	if actor["id"] != "user:alice" {
		t.Fatalf("event.actor.id = %v, want user:alice", actor["id"])
	}
}

// An event with NO stamped actor must not grow an empty actor map -- that
// would defeat exists(event.actor) guards.
func TestBuildEventEnvelope_ActorAbsentStaysAbsent(t *testing.T) {
	ev := actorEvent("")
	env := buildEventEnvelope(&ev, "", "")
	if _, present := env["actor"]; present {
		t.Fatalf("event.actor must be ABSENT when the emitter stamped none, got %v", env["actor"])
	}
}

func TestBuildEventEnvelope_Timestamp(t *testing.T) {
	ev := actorEvent("user:alice")
	env := buildEventEnvelope(&ev, "", "")
	ts, ok := env["timestamp"].(string)
	if !ok || ts == "" {
		t.Fatalf("event.timestamp missing: %v", env["timestamp"])
	}
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Fatalf("event.timestamp %q is not RFC3339: %v", ts, err)
	}
	// Zero timestamp -> omitted.
	zero := events.Event{Topic: "t", Payload: map[string]any{}}
	env = buildEventEnvelope(&zero, "", "")
	if _, present := env["timestamp"]; present {
		t.Fatal("zero occurrence time must omit event.timestamp")
	}
}

// The synthetic (nil-event) envelope keeps its prior shape -- no actor, no
// timestamp.
func TestBuildEventEnvelope_SyntheticUnchanged(t *testing.T) {
	env := buildEventEnvelope(nil, "schedule", "cron")
	if _, present := env["actor"]; present {
		t.Fatal("synthetic envelope must not carry an actor")
	}
	if _, present := env["timestamp"]; present {
		t.Fatal("synthetic envelope must not carry a timestamp")
	}
}

// A condition/filter expression reads event.actor.id -- the shape an
// automation (or @trigger @filter) uses.
func TestEvaluator_EventActorInCondition(t *testing.T) {
	ev := actorEvent("user:alice")
	e := NewEvaluator()
	e.SetCustom("event", buildEventEnvelope(&ev, "", ""))
	ok, err := e.EvaluateCondition(`event.actor.id == "user:alice"`)
	if err != nil {
		t.Fatalf("EvaluateCondition: %v", err)
	}
	if !ok {
		t.Fatal("event.actor.id condition should match the stamped actor")
	}
	ok, err = e.EvaluateCondition(`exists(event.actor)`)
	if err != nil {
		t.Fatalf("EvaluateCondition exists: %v", err)
	}
	if !ok {
		t.Fatal("exists(event.actor) should be true when stamped")
	}
	// Unstamped event: exists() must be false.
	noActor := actorEvent("")
	e2 := NewEvaluator()
	e2.SetCustom("event", buildEventEnvelope(&noActor, "", ""))
	ok, err = e2.EvaluateCondition(`exists(event.actor)`)
	if err != nil {
		t.Fatalf("EvaluateCondition exists (absent): %v", err)
	}
	if ok {
		t.Fatal("exists(event.actor) must be false when the emitter stamped none")
	}
}
