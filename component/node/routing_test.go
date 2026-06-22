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
		// #1412 regression: the voice gate directive must reach the BFF's
		// per-turn waiter across the mesh; default-deny stranded it on the
		// cognition node and every gate-path voice turn timed out.
		{"voice.gate.directive", true, false, NodeTypeBFF},
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

// TestEvaluateRouting_CachedConceptForwarding pins the cross-node
// correctness guarantee for result-cache adoption (epic 5, issue 5.5 /
// memql#1969): every concept a query @cache's must have ALL THREE graph write
// kinds (created / updated / deleted) forwarded to peers, or its 5.4
// invalidation subscriber never fires on sibling replicas and the cached read
// goes stale cross-node. A single-node green test would not catch this -- the
// eviction is per-Ristretto-per-node.
func TestEvaluateRouting_CachedConceptForwarding(t *testing.T) {
	rules := defaultRoutingRules()

	// Concepts cached in 5.5. Each must forward create+update+delete.
	cachedConcepts := []string{
		"v1:agents:agentRole",    // queryActiveAgentRoles / queryAgentRoleBySlug
		"v1:agents:skill",        // queryActiveSkills / queryActiveSkillsFull / querySkillBySlug
		"v1:router:budget",       // queryRouterBudgets
		"v1:cognition:utterance", // querySpaceUtterances
	}

	for _, concept := range cachedConcepts {
		for _, action := range []string{"created", "updated", "deleted"} {
			topic := "graph.node." + action + "." + concept
			d := evaluateRouting(rules, topic)
			if !d.Forward {
				t.Errorf("cached concept %q write %q must forward cross-node (else 5.4 eviction goes stale on siblings), but it does not", concept, action)
			}
			if !d.Broadcast {
				t.Errorf("cached concept %q write %q must broadcast to all replicas, got targeted %q", concept, action, d.TargetType)
			}
		}
	}
}

// TestEvaluateRouting_CacheRulesAreConceptScoped is the blast-radius guard for
// the 5.5 forwarding rules (memql#1969 coordinator review): the new cache rules
// must be SCOPED TO THE CACHED CONCEPT, never the whole namespace. Forwarding
// all of v1:agents:* / v1:router:* would newly republish unrelated lifecycle
// events onto peer buses and can double-fire automations that assumed
// single-node delivery (e.g. reRouteNeedsAgentOnAgentCreate on
// graph.node.created.v1:agents:agent, memql#1396). These sibling concepts share
// the namespace with a cached concept but are NOT cached, so the cache rules
// must NOT forward them. (v1:cognition:* is intentionally excluded -- it is
// already broadly forwarded by the pre-existing default cognition rules.)
func TestEvaluateRouting_CacheRulesAreConceptScoped(t *testing.T) {
	rules := defaultRoutingRules()

	// Non-cached siblings in the namespaces the 5.5 rules touch. None of these
	// may be forwarded by the cache rules (created/updated; the namespaces have
	// no default forward, so a forward here means an over-broad wildcard crept
	// back in).
	notNewlyForwarded := []string{
		"graph.node.created.v1:agents:agent",              // the over-firing automation's trigger
		"graph.node.updated.v1:agents:agent",              //
		"graph.node.created.v1:agents:agentAuthorization", // trust grants
		"graph.node.created.v1:router:route",              // hypothetical sibling
	}
	for _, topic := range notNewlyForwarded {
		if d := evaluateRouting(rules, topic); d.Forward {
			t.Errorf("topic %q must NOT be forwarded -- the 5.5 cache rules must be concept-scoped, not namespace-wide (blast-radius regression: a namespace wildcard would republish unrelated lifecycle events and risk a cross-node automation double-fire)", topic)
		}
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
