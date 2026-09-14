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

// TestExtractConceptFromFilter covers both editions (epic memql#5363): a
// legacy raw-text filter, and an edition-2026 lambda filter, which reaches the
// loader as its canonical source.
func TestExtractConceptFromFilter(t *testing.T) {
	for _, tc := range []struct{ filter, want string }{
		{`concept=="v1:cognition:participant"`, "v1:cognition:participant"},
		{`concept==v1:cognition:participant;status=="x"`, "v1:cognition:participant"},
		// The `&&` AND read its value as "X && y".
		{`concept==v1:cognition:participant && status=="x"`, "v1:cognition:participant"},
		{`row => row.concept == "v1:cognition:participant" && row.status == "x"`, "v1:cognition:participant"},
		// A concept test that does not narrow is not the filter's concept.
		{`row => row.concept == "v1:a:b" || row.status == "x"`, ""},
		{`row => row.status == "x"`, ""},
		{``, ""},
	} {
		if got := extractConceptFromFilter(tc.filter); got != tc.want {
			t.Errorf("extractConceptFromFilter(%q) = %q, want %q", tc.filter, got, tc.want)
		}
	}
}
