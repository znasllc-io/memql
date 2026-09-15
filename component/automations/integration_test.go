package automations

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// TestEventBusTrigger verifies events can trigger automations.
func TestEventBusTrigger(t *testing.T) {
	bus := events.NewBus(nil)
	defer bus.Close()

	received := make(chan events.Event, 1)

	// Subscribe to session.opened
	unsub := bus.Subscribe("session.opened", func(event events.Event) {
		received <- event
	})
	defer unsub()

	// Publish a session.opened event
	event := events.NewEvent("session.opened", events.KindSessionOpened, map[string]any{
		"subject":     "auth0|testuser",
		"email":       "test@example.com",
		"firstName":   "Test",
		"lastName":    "User",
		"role":        "member",
		"phoneNumber": "",
	})
	bus.PublishSync(event)

	// Wait for event
	select {
	case e := <-received:
		if e.Topic != "session.opened" {
			t.Errorf("expected topic 'session.opened', got %q", e.Topic)
		}
		if e.Payload["subject"] != "auth0|testuser" {
			t.Errorf("expected subject 'auth0|testuser', got %v", e.Payload["subject"])
		}
		if e.Payload["email"] != "test@example.com" {
			t.Errorf("expected email 'test@example.com', got %v", e.Payload["email"])
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}
