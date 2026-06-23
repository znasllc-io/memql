package node

import (
	"sync"

	"github.com/znasllc-io/memql/component/events"
)

// RoutingRule determines how an event should be forwarded to peers.
type RoutingRule struct {
	// Pattern is a glob pattern matched against event topics.
	Pattern string

	// TargetType restricts forwarding to peers of this type.
	// Empty string means broadcast to all connected peers.
	TargetType NodeType

	// Block when true means events matching this pattern should NOT be forwarded.
	Block bool
}

var (
	extraRulesMu sync.Mutex
	extraRules   []RoutingRule
)

// RegisterRoutingRule adds a routing rule from an init() function. Build
// tags on the caller control which binaries include the registration, so
// product code (e.g. integrations/copresent/) can declare its own concept
// patterns without editing the core node package. Order matches
// registration order; block rules still evaluate first across the
// combined set (built-in + registered).
func RegisterRoutingRule(rule RoutingRule) {
	extraRulesMu.Lock()
	defer extraRulesMu.Unlock()
	extraRules = append(extraRules, rule)
}

// defaultRoutingRules returns the effective rule set: the built-ins
// (core infrastructure events) plus any rules registered by product
// plug-ins via RegisterRoutingRule.
//
// Block rules are evaluated first; if any block rule matches, the event
// is not forwarded. Forward rules are evaluated next; if any forward
// rule matches, the event is forwarded. Events that match no rules are
// not forwarded (default-deny).
func defaultRoutingRules() []RoutingRule {
	core := []RoutingRule{
		// Block rules -- these events stay local.
		{Pattern: "automation.#", Block: true},
		{Pattern: "telemetry.#", Block: true},
		{Pattern: "session.#", Block: true},
		{Pattern: "query.#", Block: true},

		// Forward rules -- core/infrastructure events.
		// The "*" before the concept matches any partition segment.
		{Pattern: "graph.node.created.v1:cluster:*", TargetType: ""},
		{Pattern: "graph.node.updated.v1:cluster:*", TargetType: ""},
		{Pattern: "graph.node.deleted.v1:cluster:*", TargetType: ""},
		{Pattern: "graph.node.created.v1:cognition:*", TargetType: ""},
		{Pattern: "graph.node.updated.v1:cognition:*", TargetType: ""},
		// Result-cache invalidation forwarding (epic 5, issue 5.6 /
		// memql#1970). ONE broadcast rule forwards the dedicated
		// cache-invalidation channel to every node type. Every graph write
		// emits cache.invalidate.<concept> on this separate topic (see
		// MemQLEngine.publishCacheInvalidate); ONLY the result-cache evictor
		// subscribes to it (no automations, no other consumers), so
		// forwarding it everywhere has ZERO side effects. This SUPERSEDES the
		// per-concept graph-write cache rules 5.5 added (the
		// v1:agents:agentRole / v1:agents:skill / v1:router:budget
		// create/update/delete rules and the v1:cognition:utterance delete
		// rule), which are now retired: they coupled cache eviction to
		// per-concept graph-write forwarding and carried an automation-
		// double-fire risk if ever broadened (e.g.
		// reRouteNeedsAgentOnAgentCreate on graph.node.created.v1:agents:agent,
		// memql#1396). A cached read on any replica is now evicted purely via
		// this broadcast channel, with zero per-concept routing rules.
		{Pattern: "cache.invalidate.*", TargetType: ""},
		// Planner graph events: BFF owns the writes (createPlan
		// fires on BFF), the planner-tagged binary subscribes
		// graph.node.created.v1:planner:plan in its
		// PlannerAgentLoop.HandlePlanCreated. Without this forward
		// rule, default-deny in the mesh meant the planner node never
		// saw plan-creation events from the BFF -- the user-cockpit's
		// submitted plans showed status=queued in the DB forever
		// because no subscriber was listening on the right node.
		// Broadcast so any planner-tagged peer in the mesh hears it.
		{Pattern: "graph.node.created.v1:planner:*", TargetType: ""},
		{Pattern: "graph.node.updated.v1:planner:*", TargetType: ""},
		{Pattern: "graph.node.deleted.v1:planner:*", TargetType: ""},
		// Self-healing precondition-miss signal (Epic 4 / memql#2139). A
		// first-class automation precondition that evaluates false emits
		// healing.precondition.missed; the LLM repair loop (E4.4) subscribes.
		// The producer (the automation harness) and the consumer (the repair
		// loop) may live on different replicas -- and automation.# is mesh-
		// BLOCKED above -- so the miss signal needs its OWN forward rule or it
		// silently dies in cluster mode. Broadcast so any repair-loop-bearing
		// peer hears it. Healing is low-volume (a miss is an exception path),
		// so broadcasting the whole healing.# tree carries negligible cost.
		{Pattern: "healing.#", TargetType: ""},
		{Pattern: "cognition.response.audio", TargetType: NodeTypeVoice},
		// Voice gate directive (#479 gate path): cognition publishes the
		// per-turn gate decision, the BFF's voice-turn waiter subscribes
		// (component/grpc/voice_agent_handlers.go). Without this rule the
		// default-deny strands the directive on the cognition node and every
		// gate-path voice turn times out after 30s in cluster mode (#1412).
		{Pattern: "voice.gate.directive", TargetType: NodeTypeBFF},
	}

	extraRulesMu.Lock()
	defer extraRulesMu.Unlock()
	out := make([]RoutingRule, 0, len(core)+len(extraRules))
	out = append(out, core...)
	out = append(out, extraRules...)
	return out
}

// routingDecision represents the outcome of evaluating routing rules for an event.
type routingDecision struct {
	Forward    bool
	Broadcast  bool     // true = send to all peers
	TargetType NodeType // if not broadcast, send only to this type
}

// evaluateRouting checks the event topic against routing rules and returns a decision.
func evaluateRouting(rules []RoutingRule, topic string) routingDecision {
	// Check block rules first
	for _, rule := range rules {
		if rule.Block && events.Match(rule.Pattern, topic) {
			return routingDecision{Forward: false}
		}
	}

	// Check forward rules
	for _, rule := range rules {
		if !rule.Block && events.Match(rule.Pattern, topic) {
			return routingDecision{
				Forward:    true,
				Broadcast:  rule.TargetType == "",
				TargetType: rule.TargetType,
			}
		}
	}

	// Default: do not forward
	return routingDecision{Forward: false}
}
