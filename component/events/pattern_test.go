package events

import "testing"

func TestMatch(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		topic   string
		want    bool
	}{
		// Exact matches
		{"exact match", "graph.node.created", "graph.node.created", true},
		{"exact no match", "graph.node.created", "graph.node.deleted", false},
		{"single segment exact", "graph", "graph", true},

		// Single wildcard (*)
		{"star matches one segment", "graph.node.*", "graph.node.created", true},
		{"star matches one segment 2", "graph.node.*", "graph.node.deleted", true},
		{"star does not match multi", "graph.node.*", "graph.node.created.Skills", false},
		{"star at start", "*.node.created", "graph.node.created", true},
		{"star in middle", "graph.*.created", "graph.node.created", true},
		{"multiple stars", "*.node.*", "graph.node.created", true},
		{"star only matches one", "*", "graph", true},
		{"star only no multi", "*", "graph.node", false},

		// Multi-segment wildcard (#)
		{"hash matches all", "#", "graph.node.created.Skills", true},
		{"hash matches single", "#", "graph", true},
		{"hash matches empty", "#", "", true},
		{"hash at end", "graph.#", "graph.node.created", true},
		{"hash at end single", "graph.#", "graph", true},
		{"hash at end deep", "graph.#", "graph.node.created.Skills.Programming", true},
		{"hash in middle", "graph.#.created", "graph.node.created", true},
		{"hash in middle multi", "graph.#.Skills", "graph.node.created.Skills", true},

		// Edge cases
		{"empty pattern empty topic", "", "", true},
		{"empty pattern", "", "graph", false},
		{"empty topic", "graph", "", false},
		{"pattern longer than topic", "graph.node.created.extra", "graph.node.created", false},
		{"topic longer than pattern", "graph.node", "graph.node.created", false},

		// Concept suffix patterns
		{"concept suffix exact", "graph.node.created.Skills", "graph.node.created.Skills", true},
		{"concept suffix star", "graph.node.created.*", "graph.node.created.Skills", true},
		{"concept suffix hash", "graph.node.#", "graph.node.created.Skills", true},

		// Intra-segment wildcard (glob within a dot-segment)
		{"intra-segment wildcard", "graph.node.created.v1:cognition:*", "graph.node.created.v1:cognition:utterance", true},
		{"intra-segment wildcard agent", "graph.node.created.v1:cognition:*", "graph.node.created.v1:cognition:agent", true},
		{"intra-segment wildcard nested", "graph.node.created.v1:cognition:*", "graph.node.created.v1:cognition:canvas:element", true},
		{"intra-segment no match prefix", "graph.node.created.v1:cognition:*", "graph.node.created.v1:data:record", false},
		{"intra-segment no match wrong base", "graph.node.created.v1:cognition:*", "graph.node.deleted.v1:cognition:utterance", false},
		{"intra-segment wildcard data", "graph.node.created.v1:data:*", "graph.node.created.v1:data:record", true},
		{"intra-segment question mark", "graph.node.?.Skills", "graph.node.x.Skills", true},
		{"intra-segment question mark no match", "graph.node.?.Skills", "graph.node.xx.Skills", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Match(tt.pattern, tt.topic)
			if got != tt.want {
				t.Errorf("Match(%q, %q) = %v, want %v", tt.pattern, tt.topic, got, tt.want)
			}
		})
	}
}

func TestNormalizePattern(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"graph.node.created", "graph.node.created"},
		{"  graph.node.created  ", "graph.node.created"},
		{"graph..node", "graph.node"},
		{".graph.node.", "graph.node"},
		{"", "#"},
		{"   ", "#"},
		{"#", "#"},
		{"*", "*"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := NormalizePattern(tt.input)
			if got != tt.want {
				t.Errorf("NormalizePattern(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBuildTopicWithConcept(t *testing.T) {
	tests := []struct {
		baseTopic string
		concept   string
		want      string
	}{
		{"graph.node.created", "Skills", "graph.node.created.Skills"},
		{"graph.node.created", "", "graph.node.created"},
		{"graph.node.created", "  ", "graph.node.created"},
		{"query.executed", "Users", "query.executed.Users"},
	}

	for _, tt := range tests {
		t.Run(tt.baseTopic+"+"+tt.concept, func(t *testing.T) {
			got := BuildTopicWithConcept(tt.baseTopic, tt.concept)
			if got != tt.want {
				t.Errorf("BuildTopicWithConcept(%q, %q) = %q, want %q", tt.baseTopic, tt.concept, got, tt.want)
			}
		})
	}
}
