package node

import "testing"

func TestIsClientToolRelayTopic(t *testing.T) {
	cases := map[string]bool{
		"graph.node.created.v1:cognition:client:tool:request":  true,
		"graph.node.created.v1:cognition:client:tool:response": true,
		"graph.node.created.v1:cognition:utterance":            false,
		"graph.node.created.v1:planner:plan":                   false,
		"voice.gate.directive":                                 false,
	}
	for topic, want := range cases {
		if got := isClientToolRelayTopic(topic); got != want {
			t.Errorf("isClientToolRelayTopic(%q) = %v, want %v", topic, got, want)
		}
	}
}

func TestRelayCallIdFromPayload(t *testing.T) {
	if got := relayCallIdFromPayload(map[string]any{"callId": "abc-123"}); got != "abc-123" {
		t.Errorf("top-level callId: got %q, want abc-123", got)
	}
	nested := map[string]any{"payload": map[string]any{"callId": "nested-9"}}
	if got := relayCallIdFromPayload(nested); got != "nested-9" {
		t.Errorf("nested callId: got %q, want nested-9", got)
	}
	if got := relayCallIdFromPayload(nil); got != "" {
		t.Errorf("nil payload: got %q, want empty", got)
	}
	if got := relayCallIdFromPayload(map[string]any{"other": 1}); got != "" {
		t.Errorf("missing callId: got %q, want empty", got)
	}
}
