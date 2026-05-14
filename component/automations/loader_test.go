package automations

import (
	"testing"
)

func TestExtractConceptFromTopic(t *testing.T) {
	tests := []struct {
		topic    string
		expected string
	}{
		// Canonical 5-segment format: graph.node.{action}.{partition}.{concept}
		{"graph.node.created.default.v1:cognition:participant", "v1:cognition:participant"},
		{"graph.node.created.acme.v1:crm:lead", "v1:crm:lead"},
		{"graph.node.deleted._system.v1:cluster:node", "v1:cluster:node"},
		{"graph.node.updated.default.v1:cognition:utterance", "v1:cognition:utterance"},

		// Partition wildcard -- the canonical automation trigger form.
		// Concept side is still unambiguous.
		{"graph.node.created.*.v1:cognition:participant", "v1:cognition:participant"},
		{"graph.node.updated.*.v1:cognition:utterance", "v1:cognition:utterance"},

		// Concept-side wildcards -- no single concept identified.
		{"graph.node.created.default.*", ""},
		{"graph.node.created.default.v1:cognition:*", ""},
		{"graph.node.*", ""},
		{"graph.#", ""},

		// Non-graph topics
		{"session.opened", ""},
		{"automation.completed", ""},

		// Edge cases -- fewer than 5 segments does not identify a concept.
		{"", ""},
		{"graph.node", ""},
		{"graph.node.created", ""},
		{"graph.node.created.*", ""},
		{"graph.node.created.v1:cognition:participant", ""},
	}

	for _, tt := range tests {
		t.Run(tt.topic, func(t *testing.T) {
			got := extractConceptFromTopic(tt.topic)
			if got != tt.expected {
				t.Errorf("extractConceptFromTopic(%q) = %q, want %q", tt.topic, got, tt.expected)
			}
		})
	}
}

func TestExtractConceptFromFilter(t *testing.T) {
	tests := []struct {
		filter   string
		expected string
	}{
		// Simple concept filters
		{"concept==v1:cognition:participant", "v1:cognition:participant"},
		{"concept==v1:crm:lead", "v1:crm:lead"},
		{`concept=="v1:cognition:participant"`, "v1:cognition:participant"},

		// Concept filter with other conditions
		{"concept==v1:cognition:participant;payload.status==\"active\"", "v1:cognition:participant"},
		{"payload.status==\"active\";concept==v1:cognition:participant", "v1:cognition:participant"},

		// No concept filter
		{"payload.status==\"active\"", ""},
		{"payload.participantType==\"human\"", ""},

		// Edge cases
		{"", ""},
		{"concept=v1:cognition:participant", ""}, // Single = is not valid
	}

	for _, tt := range tests {
		t.Run(tt.filter, func(t *testing.T) {
			got := extractConceptFromFilter(tt.filter)
			if got != tt.expected {
				t.Errorf("extractConceptFromFilter(%q) = %q, want %q", tt.filter, got, tt.expected)
			}
		})
	}
}

func TestValidateTrigger_ContradictingConcept(t *testing.T) {
	// This test verifies that validateTrigger logs a warning for contradicting concepts
	// We can't easily test the logging, but we can verify the function doesn't panic

	loader := NewLoader(LoaderOptions{})

	tests := []struct {
		name    string
		trigger *TriggerConfig
	}{
		{
			name: "contradicting concept",
			trigger: &TriggerConfig{
				Event:  "graph.node.created.*.v1:cognition:participant",
				Filter: "concept==v1:cognition:space", // Contradicts topic
			},
		},
		{
			name: "matching concept (redundant)",
			trigger: &TriggerConfig{
				Event:  "graph.node.created.*.v1:cognition:participant",
				Filter: "concept==v1:cognition:participant",
			},
		},
		{
			name: "no concept in filter",
			trigger: &TriggerConfig{
				Event:  "graph.node.created.*.v1:cognition:participant",
				Filter: "payload.status==\"active\"",
			},
		},
		{
			name: "wildcard topic with concept filter",
			trigger: &TriggerConfig{
				Event:  "graph.node.created.*",
				Filter: "concept==v1:cognition:participant",
			},
		},
		{
			name:    "nil trigger",
			trigger: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			automation := &Automation{
				Name:    "test",
				Trigger: tt.trigger,
			}
			// Should not panic
			loader.validateTrigger(automation)
		})
	}
}
