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
		// Result-cache invalidation forwarding (epic 5, issue 5.5 /
		// memql#1969). A cached read must be evicted on EVERY replica when a
		// dependent row is written anywhere in the mesh; the invalidation
		// subscriber (graph.node.*.#) only fires on a node when the write
		// reaches it. So for each concept a query @cache's whose writes are
		// NOT already forwarded by the default set above, forward exactly that
		// concept's create/update/delete -- SCOPED TO THE CONCEPT, not the
		// namespace. Concept-scoping is deliberate: forwarding a whole
		// namespace (e.g. v1:agents:*) would newly republish unrelated
		// lifecycle events onto peer buses and can double-fire automations that
		// assumed single-node delivery -- e.g. reRouteNeedsAgentOnAgentCreate
		// keys on graph.node.created.v1:agents:agent (memql#1396, has a history
		// of over-firing). The three cached concepts below are admin-config
		// concepts with NO automation keyed on them, so scoping them is both
		// safe and the minimum the cache needs.
		//   v1:agents:agentRole -- queryActiveAgentRoles / queryAgentRoleBySlug
		//   v1:agents:skill     -- queryActiveSkills / queryActiveSkillsFull / querySkillBySlug
		//   v1:router:budget    -- queryRouterBudgets
		{Pattern: "graph.node.created.v1:agents:agentRole", TargetType: ""},
		{Pattern: "graph.node.updated.v1:agents:agentRole", TargetType: ""},
		{Pattern: "graph.node.deleted.v1:agents:agentRole", TargetType: ""},
		{Pattern: "graph.node.created.v1:agents:skill", TargetType: ""},
		{Pattern: "graph.node.updated.v1:agents:skill", TargetType: ""},
		{Pattern: "graph.node.deleted.v1:agents:skill", TargetType: ""},
		{Pattern: "graph.node.created.v1:router:budget", TargetType: ""},
		{Pattern: "graph.node.updated.v1:router:budget", TargetType: ""},
		{Pattern: "graph.node.deleted.v1:router:budget", TargetType: ""},
		// v1:cognition:utterance create/update are ALREADY forwarded by the
		// broad cognition rules above; only its delete needs adding (the
		// append-only utterance stream is rarely hard-deleted, but a cross-node
		// purge must still evict the cached page on sibling replicas). Scoped
		// to the utterance concept rather than broadening the cognition delete
		// to the whole namespace.
		{Pattern: "graph.node.deleted.v1:cognition:utterance", TargetType: ""},
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
