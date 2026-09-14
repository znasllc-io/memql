package automations

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// TestConditionWithStepMetadata verifies conditions can read a step's metadata.
func TestConditionWithStepMetadata(t *testing.T) {
	eval := NewEvaluator()

	// Simulate a step result with metadata
	eval.SetStepResult("checkExistingUser", &StepResult{
		StepId: "checkExistingUser",
		Status: "success",
		Result: []any{}, // Empty result means user doesn't exist
		Metadata: map[string]any{
			"itemCount": 0,
		},
	})

	tests := []struct {
		name      string
		condition string
		expected  bool
	}{
		{
			name:      "itemCount equals zero",
			condition: "steps.checkExistingUser.metadata.itemCount == 0",
			expected:  true,
		},
		{
			name:      "itemCount not equals zero",
			condition: "steps.checkExistingUser.metadata.itemCount != 0",
			expected:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := evalV1Cond(t, eval, tt.condition)
			if err != nil {
				t.Fatalf("failed to evaluate condition %q: %v", tt.condition, err)
			}
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

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
