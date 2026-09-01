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
// product code (e.g. a pack's integrations package) can declare its own
// concept patterns without editing the core node package. Order matches
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
		// Cognition DELETES (memql#4542). Creates and updates have crossed
		// since the mesh existed; deletes never did, so a row removed on
		// one replica stayed on the screen of every browser attached to
		// another until the tab reloaded. The asymmetry is worse than a
		// uniformly dark concept: the surface demonstrably updates -- new
		// utterances and participants arrive live -- which is exactly what
		// makes a row that will not go away read as a rendering bug rather
		// than as an event that never arrived.
		//
		// Deletes are rarer than the creates already crossing here, so the
		// added volume is strictly below a rule that has been in place for
		// the mesh's whole life. No automation in the tree triggers on a
		// DELETED event (checked across dsl/), so there is no consumer to
		// double-fire.
		{Pattern: "graph.node.deleted.v1:cognition:*", TargetType: ""},
		// Per-user provisioning fan-out: v1:identity:user rows are written
		// by the identity node, but their consumers live on OTHER node
		// types -- the seed materializer's per-user runtime hook
		// (component/memql/seed_materializer.go) materializes pack-declared
		// perUser seeds only on carrier nodes that mount the pack tree, and
		// pack automations (e.g. daily-space provisioning) trigger on the
		// same event. Every consumer is content-addressed / create-only
		// (multi-fire safe by contract), so the event broadcasts. Without
		// this rule, per-user provisioning on signup is dead in cluster
		// mode: the event never leaves the identity node.
		{Pattern: "graph.node.created.v1:identity:user", TargetType: ""},
		// Result-cache invalidation forwarding (epic 5, issue 5.6 /
		// memql#1970). ONE broadcast rule forwards the dedicated
		// cache-invalidation channel to every node type. Every graph write
		// emits cache.invalidate.<concept> on this separate topic (see
		// MemQLEngine.InvalidateCacheForConcept); ONLY the result-cache evictor
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
		// Authoring-promote forwarding (issue znasllc-io/memql-cockpit#232).
		// ONE broadcast rule forwards the dedicated authoring-promote channel to
		// every node type. A durable promote emits authoring.promote.<bundleId>
		// (see MemQLEngine.publishAuthoringPromote); ONLY the authoring-promote
		// subscriber consumes it (no automations, no other consumers), so
		// forwarding it everywhere has ZERO side effects. This is what makes a
		// promote on node A callable on node B within seconds with no restart:
		// every replica re-hydrates the promoted bundle from the shared DB on
		// the broadcast. Mirrors the cache.invalidate.* single-broadcast pattern.
		{Pattern: "authoring.promote.*", TargetType: ""},
		// Authoring-demote forwarding (memql#2163). The inverse of the
		// authoring.promote.* rule above: a durable DEMOTE emits
		// authoring.demote.<bundleId> (see MemQLEngine.publishAuthoringDemote);
		// ONLY the authoring-demote subscriber consumes it (no automations, no
		// other consumers), so forwarding it everywhere has ZERO side effects.
		// This is what makes a demote on node A REMOVE the construct from node B's
		// shared registry within seconds with no restart: every replica removes
		// the demoted bundle's constructs on the broadcast. Same single-broadcast
		// pattern as cache.invalidate.* / authoring.promote.*.
		{Pattern: "authoring.demote.*", TargetType: ""},
		// Providers-reload forwarding (epic memql#4440). Same shape and same
		// reasoning as the three rules above: the owner-gated
		// `providersReload` builtin emits providers.reload.<requestId>, ONLY
		// the providers-reload subscriber consumes it (no automations, no
		// other consumers), so forwarding it everywhere has ZERO side
		// effects.
		//
		// WITHOUT THIS RULE THE FEATURE IS WORSE THAN ABSENT. Provider auth
		// resolves per process at boot, and the portal's Apply lands on
		// whichever replica the front door picked -- so default-deny would
		// leave a fleet where one replica can call the vendor and the others
		// cannot, presenting to a user as an assistant that works on every
		// other message. That is strictly harder to diagnose than "the key
		// did not take anywhere".
		{Pattern: "providers.reload.*", TargetType: ""},
		// Planner graph events: BFF owns the writes (createPlan
		// fires on BFF), the planner-tagged binary subscribes
		// graph.node.created.v1:planner:plan in its
		// PlannerAgentLoop.HandlePlanCreated. Without this forward
		// rule, default-deny in the mesh meant the planner node never
		// saw plan-creation events from the BFF -- the user-cockpit's
		// submitted plans showed status=queued in the DB forever
		// because no subscriber was listening on the right node.
		// Broadcast so any planner-tagged peer in the mesh hears it.
		// The Fleet's rows (epic memql#4349). A registration is WRITTEN by
		// the agent node -- every heartbeat flush moves lastSeenAt,
		// connectedNodeId and activeCount -- and READ by the Fleet page,
		// which the bff serves. Without these rules default-deny keeps the
		// event on the agent node and the page's subscription never fires:
		// the machine list would be correct on load and frozen thereafter,
		// which is the worst of the three possible behaviours because it
		// looks like it is working.
		//
		// SAFE TO BROADCAST, checked rather than assumed: no automation in
		// the tree triggers on v1:worker:* or v1:workbench:* node events
		// (dsl/worker/automations.memql triggers on v1:identity:user and a
		// schedule; dsl/workbench/automations.memql on v1:planner:plan), so
		// there is no consumer to double-fire.
		//
		// v1:worker:invocation is deliberately NOT here. One row per tool
		// call is a volume the mesh does not need to carry, and the page
		// reads a machine's recent calls on demand rather than tailing them.
		{Pattern: "graph.node.created.v1:worker:registration", TargetType: ""},
		{Pattern: "graph.node.updated.v1:worker:registration", TargetType: ""},
		{Pattern: "graph.node.created.v1:worker:routingPolicy", TargetType: ""},
		{Pattern: "graph.node.updated.v1:worker:routingPolicy", TargetType: ""},
		{Pattern: "graph.node.created.v1:workbench:workspace", TargetType: ""},
		{Pattern: "graph.node.updated.v1:workbench:workspace", TargetType: ""},
		// DELETES, added by memql#4542. The three rules above were written
		// for the Fleet's create/update flow and stopped there, which left
		// a remove invisible on every replica but the writer's: the list is
		// correct on load, gains rows live, and never loses one. That is
		// the same "looks like it is working" failure the block above
		// describes, in the one direction it did not cover. Revocation and
		// workspace teardown are both low-volume by nature -- a machine is
		// revoked once, a workspace released once per plan -- so these cost
		// the mesh nothing measurable. No automation in the tree triggers
		// on a DELETED event at all (checked across dsl/: every
		// @trigger(event="node.*") names created or updated), so there is
		// no consumer to double-fire.
		{Pattern: "graph.node.deleted.v1:worker:registration", TargetType: ""},
		{Pattern: "graph.node.deleted.v1:worker:routingPolicy", TargetType: ""},
		{Pattern: "graph.node.deleted.v1:workbench:workspace", TargetType: ""},
		{Pattern: "graph.node.created.v1:planner:*", TargetType: ""},
		{Pattern: "graph.node.updated.v1:planner:*", TargetType: ""},
		{Pattern: "graph.node.deleted.v1:planner:*", TargetType: ""},
		// THE ROAMING DESKTOP (epic memql#4746). A person's desktop
		// document is written by whichever replica served the save and
		// read by their OTHER signed-in browser, which is talking to a
		// different one. Without this rule the second browser's
		// subscription is correct on load and then never moves -- the
		// same "looks like it is working" shape the worker block above
		// describes, and the reason that block exists.
		//
		// CREATED ONLY, and that is not an omission. saveMyDesktop is an
		// insert{}, and executeWrite publishes graph.node.created for
		// every write it takes -- the overwrites included; only the
		// update() path adds graph.node.updated. So an `updated` rule
		// here would forward an event this concept cannot emit, and a
		// `deleted` one an event no path produces (v1:os:desktop has no
		// delete: a desktop is emptied by saving an empty one). Adding
		// either to "complete the set" is the change to resist.
		//
		// SAFE TO BROADCAST, checked rather than assumed: no automation
		// in the tree triggers on v1:os:*, so there is no consumer to
		// double-fire. Volume is one event per settled desktop edit, per
		// person -- a drag lands as one save because the client debounces
		// before it writes.
		{Pattern: "graph.node.created.v1:os:desktop", TargetType: ""},
		// Site edge cache invalidation (memql#3714, Task 9). The site row
		// (v1:platform:site) is written wherever an admin surface writes it
		// -- typically the bff -- and read on EVERY edge replica's own
		// process-local resolver cache (component/edge/resolve.go). Without
		// this rule the write stays on the writer's node and every OTHER
		// edge replica keeps serving its stale cached resolution until the
		// TTL backstop expires (MEMQL_EDGE_SITE_CACHE_TTL_SECONDS). Both
		// verbs are forwarded: `created` so a brand-new hostname resolves on
		// every replica immediately rather than only after the writer's
		// replica happens to serve it once, and `updated` because a status
		// flip (live -> disabled) or a bundle rollback (updateSiteBundle)
		// both go through update(), not insert(). Same broadcast shape as
		// the v1:cluster:* / v1:planner:* rules above -- this concept has no
		// automation subscribers of its own, so broadcasting it carries no
		// double-fire risk. component/edge's SiteInvalidationSubscriber is
		// the consumer.
		{Pattern: "graph.node.created.v1:platform:site", TargetType: ""},
		{Pattern: "graph.node.updated.v1:platform:site", TargetType: ""},
		// ---- The browser-facing completion (memql#4542) -------------------
		//
		// Everything from here to the end of this block was added by ONE
		// sweep with ONE method: enumerate every concept a portal surface
		// subscribes to, ask whether its events cross the mesh, and add a
		// reasoned rule or record a reasoned exclusion for each. The sweep
		// is not a document that can drift out of date -- it is the gate in
		// portal_subscription_routing_test.go (memql#4543), which reads the
		// portal source and THIS table on every run.
		//
		// Two replicas per mesh node is the default topology, so "written
		// on the node that served the write, read on the node that serves
		// the browser" is the ordinary case rather than the exotic one.
		// Each rule below names who writes the row and who reads it, in the
		// memql#4349 comment shape.
		//
		// Saved views (memql#4542). A view is written through whichever bff
		// replica the front door picked and read by the rail on a browser
		// attached to EITHER replica. With two bff replicas -- the default
		// -- a view saved in one tab never appeared in another until a
		// reload, and the rail's own subscription made that look like a
		// bug in the rail. This is the marquee case for this sweep: the
		// concept is portal-only, so nothing but the portal ever noticed.
		//
		// SAFE TO BROADCAST, checked rather than assumed: no automation in
		// the tree triggers on v1:portalviews:*.
		{Pattern: "graph.node.created.v1:portalviews:view", TargetType: ""},
		{Pattern: "graph.node.updated.v1:portalviews:view", TargetType: ""},
		{Pattern: "graph.node.deleted.v1:portalviews:view", TargetType: ""},
		// Agents (memql#4542). Agent rows are written on agent and
		// cognition nodes -- specialist creation is the planner's, and a
		// capability edit is the bff's -- and read by the Agents view and
		// by Nexus, both served by the bff. Every one of those writers is a
		// different node type from the reader, so default-deny left the
		// agents surface frozen for its entire life in the mesh.
		//
		// SAFE TO BROADCAST, checked rather than assumed: no automation in
		// the tree triggers on v1:agents:*. The header of the
		// cache.invalidate.* rule above cites reRouteNeedsAgentOnAgentCreate
		// as the double-fire hazard this class carries; that automation is
		// no longer in the tree (it survives only in those comments and in
		// a provenance example string), and the check was re-run over dsl/
		// rather than inherited from the citation.
		{Pattern: "graph.node.created.v1:agents:*", TargetType: ""},
		{Pattern: "graph.node.updated.v1:agents:*", TargetType: ""},
		{Pattern: "graph.node.deleted.v1:agents:*", TargetType: ""},
		// The Library's two row-bearing concepts (memql#4542). An artifact
		// index row is written wherever the thing it indexes was produced
		// -- an agent node promoting a deliverable, a planner node's
		// generated output, the bff's own upload handler -- and read by the
		// Artifacts page and by Nexus's artifact slot. v1:library:file is
		// the backing row for kind=file and moves on the same paths.
		// Deletes cross too, because Nexus subscribes to them: an artifact
		// removed elsewhere would otherwise stay on the map forever.
		//
		// BROADCAST WITH A CONSUMER TO CHECK, and it was checked. Unlike
		// every other rule in this sweep, these two topics DO have
		// automation subscribers, so the multi-fire question is live:
		//
		//   - graph.node.created.v1:library:file fires indexFileOnCreate
		//     (dsl/library/automations.memql), which promotes the file to
		//     an artifact index row at a DERIVED id -- "a re-run version[s]
		//     the same row rather than add a second one", in its own doc
		//     comment. Multi-fire safe by construction.
		//   - graph.node.updated.v1:library:artifact fires
		//     archiveFileOnArtifactArchive, which writes archived=true onto
		//     the backing file. "Idempotent: archiving an already-archived
		//     file writes the same value", again in its own doc comment.
		//
		// So both are multi-fire safe BY CONTRACT, which is the same
		// standard the graph.node.created.v1:identity:user rule above is
		// held to ("every consumer is content-addressed / create-only").
		// component/automations' ClusterExecutionGuard (#561) collapses the
		// duplicates on top of that, but it is the SECOND line of defence
		// and not the argument: it fails open under a bounded window when
		// the database is unreachable, so a rule that needed it would be a
		// rule that double-fires during an outage.
		{Pattern: "graph.node.created.v1:library:artifact", TargetType: ""},
		{Pattern: "graph.node.updated.v1:library:artifact", TargetType: ""},
		{Pattern: "graph.node.deleted.v1:library:artifact", TargetType: ""},
		{Pattern: "graph.node.created.v1:library:file", TargetType: ""},
		{Pattern: "graph.node.updated.v1:library:file", TargetType: ""},
		// The Files app's folder tree (memql#4781). Folder rows are written
		// by the bff (the OS's create/rename/move/archive land there) and
		// read live by every browser watching the tree -- including the
		// desk-folder popover, which may be dialed to a DIFFERENT replica
		// than the one that took the write. Without these rules the tree is
		// correct on load and frozen after, which looks like it is working.
		//
		// BROADCAST WITH A CONSUMER TO CHECK, and it was checked: no
		// automation in the tree triggers on v1:library:folder (verified
		// over dsl/ at memql#4781, the same sweep the artifact/file rules
		// above record), so the multi-fire question the library block warns
		// about does not arise here. No delete rule: nothing hard-deletes a
		// folder -- archiveLibraryFolder is an UPDATE -- so a delete rule
		// would be surface nothing sends.
		{Pattern: "graph.node.created.v1:library:folder", TargetType: ""},
		{Pattern: "graph.node.updated.v1:library:folder", TargetType: ""},
		// Authoring rows behind Nexus's Constructs page (memql#4542). A
		// bundle, its constructs and the dependency edges between them are
		// written by whichever node ran the authoring promote -- an agent
		// or planner node, in the flow Nexus exists to show -- and read by
		// the bff serving the page. The authoring.promote.* /
		// authoring.demote.* rules above forward the REGISTRY signal, which
		// is what makes a promoted construct callable on every replica;
		// they say nothing about the graph rows, which is what the page
		// draws. Two different things, and only one of them crossed.
		//
		// SAFE TO BROADCAST, checked rather than assumed: no automation in
		// the tree triggers on v1:authoring:*.
		{Pattern: "graph.node.created.v1:authoring:*", TargetType: ""},
		{Pattern: "graph.node.updated.v1:authoring:*", TargetType: ""},
		{Pattern: "graph.node.deleted.v1:authoring:*", TargetType: ""},
		// Pending invitations on the People page (memql#4542). An
		// invitation is written by the identity node and read by the bff;
		// its whole point is that somebody is waiting for it to change
		// state. Low volume by nature -- an invitation is a human action.
		//
		// SAFE TO BROADCAST, checked rather than assumed: no automation in
		// the tree triggers on v1:identity:invitation. Note the narrowness:
		// this is a per-CONCEPT rule, not v1:identity:*, so auth sessions,
		// magic links and audit mechanics stay local exactly as the
		// v1:identity:user rule above intends.
		{Pattern: "graph.node.created.v1:identity:invitation", TargetType: ""},
		{Pattern: "graph.node.updated.v1:identity:invitation", TargetType: ""},
		// The two Home tiles that count identity rows (memql#4542). The
		// tiles subscribe to CREATED only -- they show a count and the most
		// recent few -- so only created is forwarded. An account is created
		// by the identity node; an auditEvent by whichever node made the
		// decision it records, which is precisely why the tile is dark
		// today.
		//
		// v1:identity:auditEvent is the DECISIONS log, not the mechanics
		// one: memql#4328 split the routine per-request churn out to
		// v1:identity:authActivity ("two orders of magnitude more
		// numerous") specifically so the decisions log stays small enough
		// to read. That split is what makes this rule affordable, and it is
		// why authActivity is a recorded exclusion rather than a sibling
		// rule -- see RoutingExclusions.
		//
		// SAFE TO BROADCAST, checked rather than assumed: no automation in
		// the tree triggers on either concept.
		{Pattern: "graph.node.created.v1:identity:account", TargetType: ""},
		{Pattern: "graph.node.created.v1:identity:auditEvent", TargetType: ""},
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
