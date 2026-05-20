package automations

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// TestEvaluatorWithEventData verifies the evaluator can access event data.
func TestEvaluatorWithEventData(t *testing.T) {
	eval := NewEvaluator()

	// Set event data as custom variable
	eval.SetCustom("event", map[string]any{
		"topic": "session.opened",
		"kind":  "session_opened",
		"payload": map[string]any{
			"subject":     "auth0|123456",
			"email":       "test@example.com",
			"firstName":   "Test",
			"lastName":    "User",
			"role":        "developer",
			"phoneNumber": "+1234567890",
		},
	})
	eval.SetCustom("timestamp", time.Now().UTC().Format(time.RFC3339))

	tests := []struct {
		name     string
		expr     string
		expected any
	}{
		{
			name:     "event topic",
			expr:     "$event.topic",
			expected: "session.opened",
		},
		{
			name:     "event payload subject",
			expr:     "$event.payload.subject",
			expected: "auth0|123456",
		},
		{
			name:     "event payload email",
			expr:     "$event.payload.email",
			expected: "test@example.com",
		},
		{
			name:     "event payload firstName",
			expr:     "$event.payload.firstName",
			expected: "Test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := eval.EvaluateValue(tt.expr)
			if err != nil {
				t.Fatalf("failed to evaluate %q: %v", tt.expr, err)
			}
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

// TestEvaluatorConditionWithStepMetadata verifies conditions can access step metadata.
func TestEvaluatorConditionWithStepMetadata(t *testing.T) {
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
			condition: "$steps.checkExistingUser.metadata.itemCount == 0",
			expected:  true,
		},
		{
			name:      "itemCount not equals zero",
			condition: "$steps.checkExistingUser.metadata.itemCount != 0",
			expected:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := eval.EvaluateCondition(tt.condition)
			if err != nil {
				t.Fatalf("failed to evaluate condition %q: %v", tt.condition, err)
			}
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

// TestEvaluatorStringSubstitution verifies $ expressions in strings are substituted.
func TestEvaluatorStringSubstitution(t *testing.T) {
	eval := NewEvaluator()

	eval.SetCustom("event", map[string]any{
		"payload": map[string]any{
			"subject":   "auth0|user123",
			"email":     "user@example.com",
			"firstName": "John",
			"lastName":  "Doe",
		},
	})

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple substitution",
			input:    "user-$event.payload.subject",
			expected: "user-auth0|user123",
		},
		{
			name:     "multiple substitutions",
			input:    "$event.payload.firstName $event.payload.lastName",
			expected: "John Doe",
		},
		{
			name:     "substitution in JSON template",
			input:    `{"email": "$event.payload.email"}`,
			expected: `{"email": "user@example.com"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := eval.EvaluateString(tt.input)
			if err != nil {
				t.Fatalf("failed to evaluate %q: %v", tt.input, err)
			}
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestEvaluatorStringSubstitution_ArrayIndex(t *testing.T) {
	eval := NewEvaluator()

	eval.SetStepResult("extractLead", &StepResult{
		StepId:   "extractLead",
		Status:   "success",
		Result:   []any{map[string]any{"extraction": map[string]any{"email": "john.smith@example.com"}}},
		Metadata: map[string]any{"itemCount": 1},
	})

	got, err := eval.EvaluateString("lead-$steps.extractLead.result[0].extraction.email")
	if err != nil {
		t.Fatalf("failed to evaluate string: %v", err)
	}
	if got != "lead-john.smith@example.com" {
		t.Fatalf("expected %q, got %q", "lead-john.smith@example.com", got)
	}
}

func TestEvaluatorCondition_AndOrAndNullLiteral(t *testing.T) {
	eval := NewEvaluator()
	eval.SetCustom("event", map[string]any{
		"payload": map[string]any{
			"email": "john.smith@example.com",
			"phone": nil,
		},
	})

	tests := []struct {
		name      string
		condition string
		expected  bool
	}{
		{
			name:      "supports && and null literal",
			condition: `$event.payload.email != null && $event.payload.phone == null`,
			expected:  true,
		},
		{
			name:      "supports ||",
			condition: `$event.payload.email == null || $event.payload.email != null`,
			expected:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := eval.EvaluateCondition(tt.condition)
			if err != nil {
				t.Fatalf("failed to evaluate condition %q: %v", tt.condition, err)
			}
			if got != tt.expected {
				t.Fatalf("expected %v, got %v", tt.expected, got)
			}
		})
	}
}

// TestEvaluatorQueryWithUUID verifies UUIDs with hyphens are properly quoted in queries.
func TestEvaluatorQueryWithUUID(t *testing.T) {
	eval := NewEvaluator()

	// Set event data with a UUID containing hyphens
	eval.SetCustom("event", map[string]any{
		"payload": map[string]any{
			"subject": "228c5a9e-4108-4647-9f07-ad8d07d95257",
		},
	})

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "UUID in equality check should be quoted",
			input:    "payload.authorizerId==$event.payload.subject",
			expected: `payload.authorizerId=="228c5a9e-4108-4647-9f07-ad8d07d95257"`,
		},
		{
			name:     "plain string without operators should not be quoted",
			input:    "payload.name==$event.payload.simpleName",
			expected: "payload.name=$event.payload.simpleName", // Not found, original kept
		},
	}

	// Add simple name for the second test
	eval.SetCustom("event", map[string]any{
		"payload": map[string]any{
			"subject":    "228c5a9e-4108-4647-9f07-ad8d07d95257",
			"simpleName": "alice",
		},
	})

	// Update expected for second test after custom is set
	tests[1].expected = "payload.name==alice"

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := eval.EvaluateStringForQuery(tt.input)
			if err != nil {
				t.Fatalf("failed to evaluate %q: %v", tt.input, err)
			}
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// TestFormatValueForQuery verifies operator-containing strings are quoted.
func TestFormatValueForQuery(t *testing.T) {
	tests := []struct {
		name     string
		input    any
		expected string
	}{
		{
			name:     "colon-delimited ID should be quoted",
			input:    "v1:cognition:space:009c77bdd719eb267f79201676a12c179a59539f692b07b37f6c169531471b17",
			expected: `"v1:cognition:space:009c77bdd719eb267f79201676a12c179a59539f692b07b37f6c169531471b17"`,
		},
		{
			name:     "UUID with hyphens should be quoted",
			input:    "228c5a9e-4108-4647-9f07-ad8d07d95257",
			expected: `"228c5a9e-4108-4647-9f07-ad8d07d95257"`,
		},
		{
			name:     "simple string should not be quoted",
			input:    "alice",
			expected: "alice",
		},
		{
			name:     "string with plus should be quoted",
			input:    "user+tag",
			expected: `"user+tag"`,
		},
		{
			name:     "email with at should not be quoted",
			input:    "user@example.com",
			expected: "user@example.com", // @ is not an operator
		},
		{
			name:     "number should not be quoted",
			input:    42,
			expected: "42",
		},
		{
			name:     "boolean should not be quoted",
			input:    true,
			expected: "true",
		},
		{
			name:     "string with embedded quotes and no operators should not be quoted",
			input:    `say "hello"`,
			expected: `say "hello"`, // No operators, so not quoted
		},
		{
			name:     "string with operators and embedded quotes should escape them",
			input:    `x+y="hello"`,
			expected: `"x+y=\"hello\""`,
		},
		// MemQL fragment tests - fragments should pass through unquoted
		{
			name:     "MemQL fragment with semicolon should NOT be quoted",
			input:    "concept==v1:user;payload.active==true",
			expected: "concept==v1:user;payload.active==true",
		},
		{
			name:     "MemQL fragment with == operator should NOT be quoted",
			input:    "payload.status==completed",
			expected: "payload.status==completed",
		},
		{
			name:     "MemQL fragment with in operator should NOT be quoted",
			input:    `payload.role in ("admin","user")`,
			expected: `payload.role in ("admin","user")`,
		},
		{
			name:     "MemQL fragment with != operator should NOT be quoted",
			input:    "payload.deleted!=true",
			expected: "payload.deleted!=true",
		},
		// Edge cases: literal values that contain operators should BE quoted
		{
			name:     "literal value with == should be quoted (not a fragment)",
			input:    "abc==def123",
			expected: `"abc==def123"`,
		},
		{
			name:     "checksum-like value with == should be quoted",
			input:    "sha256==abcdef123456",
			expected: `"sha256==abcdef123456"`,
		},
		{
			name:     "base64 padding should be quoted",
			input:    "dGVzdA==",
			expected: `"dGVzdA=="`,
		},
		// Fragments with known field prefixes (no dots)
		{
			name:     "concept== fragment should NOT be quoted",
			input:    "concept==v1:crm:lead",
			expected: "concept==v1:crm:lead",
		},
		{
			name:     "id== fragment should NOT be quoted",
			input:    `id=="user-123"`,
			expected: `id=="user-123"`,
		},
		// Numeric-looking IDs that could be misinterpreted as scientific notation
		{
			name:     "alphanumeric ID starting with digits should be quoted",
			input:    "69701e53728ad014454d19f7",
			expected: `"69701e53728ad014454d19f7"`,
		},
		{
			name:     "hex ID starting with digits should be quoted",
			input:    "123abc456def",
			expected: `"123abc456def"`,
		},
		{
			name:     "valid number should not be quoted",
			input:    "123456",
			expected: "123456",
		},
		{
			name:     "valid scientific notation should not be quoted",
			input:    "1.5e10",
			expected: "1.5e10",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatValueForQuery(tt.input)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
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

// TestEvaluatorCompoundConditions tests that the evaluator handles compound conditions
// with semicolon (AND) and comma (OR) operators correctly.
func TestEvaluatorCompoundConditions(t *testing.T) {
	eval := NewEvaluator()

	// Set up event payload similar to what node.created would provide
	eval.SetCustom("event", map[string]any{
		"topic": "graph.node.created.v1:cognition:participant",
		"kind":  "node_created",
		"payload": map[string]any{
			"id":              "v1:cognition:participant:123",
			"concept":         "v1:cognition:participant",
			"participantType": "human",
			"status":          "active",
			"spaceId":         "space-456",
		},
	})

	tests := []struct {
		name      string
		condition string
		expected  bool
	}{
		// Single conditions (clean syntax - bare paths auto-resolve to $event.{path})
		{
			name:      "single condition true",
			condition: `payload.participantType=="human"`,
			expected:  true,
		},
		{
			name:      "single condition false",
			condition: `payload.participantType=="si"`,
			expected:  false,
		},
		// AND conditions with semicolon
		{
			name:      "AND both true",
			condition: `payload.participantType=="human";payload.status=="active"`,
			expected:  true,
		},
		{
			name:      "AND first true second false",
			condition: `payload.participantType=="human";payload.status=="inactive"`,
			expected:  false,
		},
		{
			name:      "AND first false second true",
			condition: `payload.participantType=="si";payload.status=="active"`,
			expected:  false,
		},
		{
			name:      "AND both false",
			condition: `payload.participantType=="si";payload.status=="inactive"`,
			expected:  false,
		},
		// Three-way AND
		{
			name:      "three-way AND all true",
			condition: `payload.participantType=="human";payload.status=="active";payload.concept=="v1:cognition:participant"`,
			expected:  true,
		},
		{
			name:      "three-way AND one false",
			condition: `payload.participantType=="human";payload.status=="inactive";payload.concept=="v1:cognition:participant"`,
			expected:  false,
		},
		// OR conditions with comma
		{
			name:      "OR both true",
			condition: `payload.participantType=="human",payload.participantType=="si"`,
			expected:  true,
		},
		{
			name:      "OR first true second false",
			condition: `payload.participantType=="human",payload.participantType=="bot"`,
			expected:  true,
		},
		{
			name:      "OR first false second true",
			condition: `payload.participantType=="bot",payload.participantType=="human"`,
			expected:  true,
		},
		{
			name:      "OR both false",
			condition: `payload.participantType=="si",payload.participantType=="bot"`,
			expected:  false,
		},
		// Empty condition
		{
			name:      "empty condition",
			condition: "",
			expected:  true,
		},
		// Explicit $event syntax still works
		{
			name:      "explicit $event syntax",
			condition: `$event.payload.participantType=="human"`,
			expected:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := eval.EvaluateCondition(tt.condition)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tt.expected {
				t.Errorf("EvaluateCondition(%q) = %v, want %v", tt.condition, result, tt.expected)
			}
		})
	}
}

// TestEvaluatorStepReference tests that bare step references like "stepName.result"
// are auto-resolved to "$steps.stepName.result" for cleaner .memql syntax.
func TestEvaluatorStepReference(t *testing.T) {
	eval := NewEvaluator()

	// Simulate a step result with query output (Bundle.nodes structure)
	eval.SetStepResult("getAgents", &StepResult{
		StepId: "getAgents",
		Status: "success",
		Result: map[string]any{
			"Bundle": map[string]any{
				"nodes": []any{
					map[string]any{"id": "agent-1", "payload": map[string]any{"name": "Assistant"}},
					map[string]any{"id": "agent-2", "payload": map[string]any{"name": "Helper"}},
				},
			},
		},
		Metadata: map[string]any{
			"itemCount": 2,
		},
	})

	tests := []struct {
		name     string
		expr     string
		wantType string // "slice", "map", "int", "string"
		wantLen  int    // for slices
	}{
		{
			name:     "bare step reference to Bundle.nodes",
			expr:     "getAgents.result.Bundle.nodes",
			wantType: "slice",
			wantLen:  2,
		},
		{
			name:     "bare step reference to metadata",
			expr:     "getAgents.metadata.itemCount",
			wantType: "int",
		},
		{
			name:     "explicit $steps reference still works",
			expr:     "$steps.getAgents.result.Bundle.nodes",
			wantType: "slice",
			wantLen:  2,
		},
		{
			name:     "non-existent step returns literal",
			expr:     "unknownStep.result",
			wantType: "string",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := eval.EvaluateStepReference(tt.expr)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			switch tt.wantType {
			case "slice":
				slice, ok := result.([]any)
				if !ok {
					t.Errorf("expected []any, got %T", result)
				} else if len(slice) != tt.wantLen {
					t.Errorf("expected slice len %d, got %d", tt.wantLen, len(slice))
				}
			case "int":
				_, ok := result.(int)
				if !ok {
					// Also accept float64 (JSON numbers)
					_, ok = result.(float64)
				}
				if !ok {
					t.Errorf("expected int or float64, got %T", result)
				}
			case "string":
				_, ok := result.(string)
				if !ok {
					t.Errorf("expected string, got %T", result)
				}
			}
		})
	}
}
