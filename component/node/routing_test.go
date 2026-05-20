package node

import "testing"

func TestEvaluateRouting_BlockRules(t *testing.T) {
	rules := defaultRoutingRules()

	tests := []struct {
		topic   string
		blocked bool
	}{
		{"automation.started", true},
		{"automation.completed", true},
		{"telemetry.metrics", true},
		{"session.opened", true},
		{"query.executed", true},
		{"graph.node.created.v1:cluster:node", false},
	}

	for _, tt := range tests {
		d := evaluateRouting(rules, tt.topic)
		if tt.blocked && d.Forward {
			t.Errorf("topic %q should be blocked but was forwarded", tt.topic)
		}
		if !tt.blocked && !d.Forward {
			t.Errorf("topic %q should be forwarded but was blocked", tt.topic)
		}
	}
}

func TestEvaluateRouting_ForwardRules(t *testing.T) {
	rules := defaultRoutingRules()

	tests := []struct {
		topic      string
		forward    bool
		broadcast  bool
		targetType NodeType
	}{
		{"graph.node.created.v1:cluster:node", true, true, ""},
		{"graph.node.updated.v1:cluster:spawnEvent", true, true, ""},
		{"graph.node.created.v1:cognition:participant", true, true, ""},
		{"graph.node.updated.v1:cognition:utterance", true, true, ""},
	}

	for _, tt := range tests {
		d := evaluateRouting(rules, tt.topic)
		if d.Forward != tt.forward {
			t.Errorf("topic %q: expected forward=%v, got %v", tt.topic, tt.forward, d.Forward)
		}
		if d.Forward {
			if d.Broadcast != tt.broadcast {
				t.Errorf("topic %q: expected broadcast=%v, got %v", tt.topic, tt.broadcast, d.Broadcast)
			}
			if d.TargetType != tt.targetType {
				t.Errorf("topic %q: expected targetType=%q, got %q", tt.topic, tt.targetType, d.TargetType)
			}
		}
	}
}

func TestRegisterRoutingRule_PluginAdds(t *testing.T) {
	// Verify a product plug-in can add forward rules through
	// RegisterRoutingRule and have them picked up by defaultRoutingRules.
	// Uses a namespace that main never ships so the test stays isolated.
	RegisterRoutingRule(RoutingRule{Pattern: "graph.node.created.v1:testproduct:*", TargetType: ""})

	rules := defaultRoutingRules()
	d := evaluateRouting(rules, "graph.node.created.v1:testproduct:thing")
	if !d.Forward {
		t.Fatalf("registered rule did not take effect: topic should forward")
	}
	if !d.Broadcast {
		t.Fatalf("registered rule should broadcast (TargetType empty)")
	}
}

func TestEvaluateRouting_DefaultDeny(t *testing.T) {
	rules := defaultRoutingRules()

	// Topics that match no rules should not be forwarded
	topics := []string{
		"custom.event.something",
		"graph.node.created.v1:unknown:thing",
		"ai.completion.started",
		"tool.called",
	}

	for _, topic := range topics {
		d := evaluateRouting(rules, topic)
		if d.Forward {
			t.Errorf("topic %q should not be forwarded (default deny), but was", topic)
		}
	}
}

func TestEvaluateRouting_BlockTakesPrecedence(t *testing.T) {
	// If both a block and forward rule match, block wins
	rules := []RoutingRule{
		{Pattern: "test.#", Block: true},
		{Pattern: "test.#", TargetType: ""},
	}

	d := evaluateRouting(rules, "test.something")
	if d.Forward {
		t.Error("block rule should take precedence over forward rule")
	}
}
