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

// TestEvaluateRouting_CacheInvalidateBroadcast pins the cross-node
// correctness guarantee for default-on result caching (epic 5, issue 5.6 /
// memql#1970): the dedicated cache-invalidation channel is forwarded to EVERY
// node by a SINGLE broadcast rule, so a write to ANY concept evicts the
// dependent cached read on sibling replicas. A single-node green test would
// not catch a missing forward -- the eviction is per-Ristretto-per-node.
//
// This SUPERSEDES the 5.5 per-concept graph-write cache rules: eviction no
// longer depends on a routing rule per cached concept. Any concept's
// cache.invalidate event rides the one broadcast rule.
func TestEvaluateRouting_CacheInvalidateBroadcast(t *testing.T) {
	rules := defaultRoutingRules()

	// Arbitrary concepts (including ones with no per-concept rule whatsoever)
	// must all have their cache-invalidation event forwarded cross-node via the
	// single cache.invalidate.* broadcast rule.
	concepts := []string{
		"v1:agents:agentRole",
		"v1:agents:skill",
		"v1:router:budget",
		"v1:cognition:utterance",
		"v1:cognition:space",       // default-cached, never had a per-concept cache rule
		"v1:knowledge:document",    // arbitrary concept -- broadcast covers it
		"v1:somenamespace:whatever", // even a concept the engine has never seen
	}

	for _, concept := range concepts {
		topic := "cache.invalidate." + concept
		d := evaluateRouting(rules, topic)
		if !d.Forward {
			t.Errorf("cache.invalidate for concept %q must forward cross-node via the broadcast rule, but it does not", concept)
		}
		if !d.Broadcast {
			t.Errorf("cache.invalidate for concept %q must broadcast to all replicas, got targeted %q", concept, d.TargetType)
		}
	}
}

// TestEvaluateRouting_PerConceptCacheRulesRetired confirms the 5.5 per-concept
// cache routing rules (memql#1969) are GONE (epic 5, issue 5.6 / memql#1970):
// cache eviction now rides the dedicated cache.invalidate.* broadcast channel,
// not per-concept graph-write forwarding. The retired rules forwarded these
// graph.node.* topics solely to drive cross-node eviction; with the broadcast
// channel they must no longer be forwarded as graph writes, so they can't
// republish lifecycle events onto peer buses (the automation-double-fire risk
// the old approach carried -- e.g. reRouteNeedsAgentOnAgentCreate on
// v1:agents:agent, memql#1396).
//
// NOTE: v1:cognition:* create/update stays forwarded by the PRE-EXISTING broad
// cognition rules (cognition's own delivery, not cache) -- so we assert only on
// the agents/router cache concepts and the cognition utterance DELETE (whose
// forward existed ONLY for the cache and is now retired).
func TestEvaluateRouting_PerConceptCacheRulesRetired(t *testing.T) {
	rules := defaultRoutingRules()

	// These graph.node.* topics had a 5.5 cache forward rule that is now
	// retired. None may be forwarded as a graph write any longer.
	retired := []string{
		"graph.node.created.v1:agents:agentRole",
		"graph.node.updated.v1:agents:agentRole",
		"graph.node.deleted.v1:agents:agentRole",
		"graph.node.created.v1:agents:skill",
		"graph.node.updated.v1:agents:skill",
		"graph.node.deleted.v1:agents:skill",
		"graph.node.created.v1:router:budget",
		"graph.node.updated.v1:router:budget",
		"graph.node.deleted.v1:router:budget",
		"graph.node.deleted.v1:cognition:utterance", // cache-only forward, retired
	}
	for _, topic := range retired {
		if d := evaluateRouting(rules, topic); d.Forward {
			t.Errorf("topic %q must NOT be forwarded -- the 5.5 per-concept cache routing rule is retired in 5.6 (eviction rides cache.invalidate.* now); a forward here means a retired rule lingered", topic)
		}
	}

	// Sanity: the non-cache sibling concepts that were never forwarded stay so.
	for _, topic := range []string{
		"graph.node.created.v1:agents:agent",              // the over-firing automation's trigger
		"graph.node.created.v1:agents:agentAuthorization", // trust grants
	} {
		if d := evaluateRouting(rules, topic); d.Forward {
			t.Errorf("topic %q must NOT be forwarded (no cache rule, no default forward)", topic)
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
