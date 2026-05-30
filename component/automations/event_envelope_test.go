package automations

import (
	"testing"

	"github.com/znasllc-io/memql/component/events"
)

// TestBuildEventEnvelope_ScheduleTriggerIsObject pins the fix for
// issue #418: a schedule- (or manual-/startup-) triggered automation
// run -- which has no triggering event -- must still seed `event`
// with an OBJECT envelope, never leave it unset/string. The
// downstream function-arg renderer otherwise emits the unresolved
// `event` reference as a bare token that the engine coerces to a
// string, failing the receiving logic function's `event: object`
// validation on every cron tick.
func TestBuildEventEnvelope_ScheduleTriggerIsObject(t *testing.T) {
	env := buildEventEnvelope(nil, "schedule", "schedule")

	if env == nil {
		t.Fatal("expected a non-nil event envelope for a schedule trigger")
	}
	payload, ok := env["payload"].(map[string]any)
	if !ok {
		t.Fatalf("expected payload to be an object, got %T", env["payload"])
	}
	if payload["triggeredBy"] != "schedule" {
		t.Fatalf("expected payload.triggeredBy=schedule, got %v", payload["triggeredBy"])
	}
	if env["kind"] != "schedule" {
		t.Fatalf("expected kind=schedule, got %v", env["kind"])
	}
	if env["topic"] != "schedule" {
		t.Fatalf("expected topic=schedule, got %v", env["topic"])
	}
}

// TestBuildEventEnvelope_EventTriggerCarriesEventData verifies the
// event-triggered path is unchanged: it surfaces the real triggering
// event's topic / kind / payload.
func TestBuildEventEnvelope_EventTriggerCarriesEventData(t *testing.T) {
	evt := &events.Event{
		Topic:   "graph.node.created.v1:identity:guestInvitation",
		Kind:    events.KindNodeCreated,
		Payload: map[string]any{"id": "v1:identity:guestInvitation:abc"},
	}

	env := buildEventEnvelope(evt, "schedule", "ignored-trigger")

	if env["topic"] != evt.Topic {
		t.Fatalf("expected topic=%q, got %v", evt.Topic, env["topic"])
	}
	if env["kind"] != evt.Kind.String() {
		t.Fatalf("expected kind=%q, got %v", evt.Kind.String(), env["kind"])
	}
	payload, ok := env["payload"].(map[string]any)
	if !ok {
		t.Fatalf("expected payload to be the event payload object, got %T", env["payload"])
	}
	if payload["id"] != "v1:identity:guestInvitation:abc" {
		t.Fatalf("expected event payload to pass through, got %v", payload)
	}
}
