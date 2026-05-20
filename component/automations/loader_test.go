package automations

import (
	"testing"
)

func TestExtractConceptFromTopic(t *testing.T) {
	tests := []struct {
		topic    string
		expected string
	}{
		// Canonical 4-segment format: graph.node.{action}.{concept}
		{"graph.node.created.v1:cognition:participant", "v1:cognition:participant"},
		{"graph.node.created.v1:crm:lead", "v1:crm:lead"},
		{"graph.node.deleted.v1:cluster:node", "v1:cluster:node"},
		{"graph.node.updated.v1:cognition:utterance", "v1:cognition:utterance"},

		// Concept-side wildcards -- no single concept identified.
		{"graph.node.created.*", ""},
		{"graph.node.created.v1:cognition:*", ""},
		{"graph.node.*", ""},
		{"graph.#", ""},

		// Non-graph topics
		{"session.opened", ""},
		{"automation.completed", ""},

		// Edge cases -- fewer than 4 segments does not identify a concept.
		{"", ""},
		{"graph.node", ""},
		{"graph.node.created", ""},
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
