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
		// Result-cache invalidation (epic 5, issue 5.5 / memql#1969): a
		// cached cognition read must be evicted on EVERY replica when a
		// dependent row is written anywhere in the mesh. created+updated
		// are already forwarded above; cognition `deleted` was not, so a
		// cross-node hard-delete of a cached concept (e.g. a purged
		// utterance/space) would leave a stale cached page on the sibling
		// replicas. Forward deletes too so the invalidation subscriber
		// fires on every node regardless of write kind.
		{Pattern: "graph.node.deleted.v1:cognition:*", TargetType: ""},
		// Cached bounded catalogs/registries (epic 5, issue 5.5 / memql#1969).
		// These namespaces were NOT forwarded cross-node by default, so the
		// moment a query against them carries @cache(ttl=N) a write on one
		// replica would evict only that replica's Ristretto and go stale on
		// its siblings. Forward all three write kinds so the result-cache
		// invalidation subscriber (graph.node.*.#) fires on every node.
		//   v1:agents:agentRole + v1:agents:skill -- admin-defined role/skill
		//     catalogs read on every agent detail render / routing decision.
		//   v1:router:budget -- the router budget config consulted on routing.
		{Pattern: "graph.node.created.v1:agents:*", TargetType: ""},
		{Pattern: "graph.node.updated.v1:agents:*", TargetType: ""},
		{Pattern: "graph.node.deleted.v1:agents:*", TargetType: ""},
		{Pattern: "graph.node.created.v1:router:*", TargetType: ""},
		{Pattern: "graph.node.updated.v1:router:*", TargetType: ""},
		{Pattern: "graph.node.deleted.v1:router:*", TargetType: ""},
		// Planner graph events: BFF owns the writes (mutationCreatePlan
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
