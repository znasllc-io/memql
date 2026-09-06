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

// TestEvaluateRouting_UserCreatedBroadcast pins the per-user provisioning
// fan-out: v1:identity:user creation is written on the identity node while
// its consumers (the seed materializer's perUser runtime hook, pack
// provisioning automations) live on carrier nodes. Losing this rule kills
// signup provisioning in cluster mode silently -- the event just never
// leaves identity.
func TestEvaluateRouting_UserCreatedBroadcast(t *testing.T) {
	d := evaluateRouting(defaultRoutingRules(), "graph.node.created.v1:identity:user")
	if !d.Forward || d.TargetType != "" {
		t.Fatalf("graph.node.created.v1:identity:user must broadcast to all node types, got forward=%v target=%q", d.Forward, d.TargetType)
	}
	// Updates and other identity concepts are NOT forwarded by this rule
	// (auth sessions, magic links etc. stay local to their writer).
	for _, topic := range []string{
		"graph.node.updated.v1:identity:user",
		"graph.node.created.v1:identity:authSession",
		"graph.node.created.v1:identity:magiclink",
	} {
		if d := evaluateRouting(defaultRoutingRules(), topic); d.Forward {
			t.Fatalf("topic %q must not be forwarded by the user-created rule", topic)
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
		{"graph.node.updated.v1:library:artifact", true, true, ""},
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

// TestEvaluateRouting_LibraryFolderBroadcast (memql#4781): folder rows are
// written by the bff (the OS Files app's create/rename/move/archive all land
// there) and read live by every browser watching the tree -- the desk-folder
// popover included, which may be dialed to a different replica. Without the
// broadcast the tree is correct on load and frozen after, which looks like
// it is working. Deletes are NOT crossed: nothing hard-deletes a folder
// (archive is an update), so a delete rule would be surface nothing sends.
func TestEvaluateRouting_LibraryFolderBroadcast(t *testing.T) {
	for _, topic := range []string{
		"graph.node.created.v1:library:folder",
		"graph.node.updated.v1:library:folder",
	} {
		d := evaluateRouting(defaultRoutingRules(), topic)
		if !d.Forward || !d.Broadcast || d.TargetType != "" {
			t.Fatalf("topic %q must broadcast to all node types (memql#4781), got forward=%v broadcast=%v target=%q",
				topic, d.Forward, d.Broadcast, d.TargetType)
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
		"v1:skills:skill",
		"v1:router:budget",
		"v1:library:artifact",
		"v1:knowledge:document",     // arbitrary concept -- broadcast covers it
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

// TestEvaluateRouting_PreconditionMissBroadcast locks in the cross-node
// guarantee for the self-healing repair-trigger signal (Epic 4 / memql#2139).
// The automation harness (producer) and the LLM repair loop (consumer) may
// live on different replicas, and automation.# is mesh-BLOCKED -- so the
// dedicated healing.precondition.missed topic MUST forward (broadcast) or the
// miss signal silently dies in cluster mode and self-healing never triggers.
func TestEvaluateRouting_PreconditionMissBroadcast(t *testing.T) {
	rules := defaultRoutingRules()

	// The miss topic must forward to all replicas.
	d := evaluateRouting(rules, "healing.precondition.missed")
	if !d.Forward {
		t.Fatalf("healing.precondition.missed must forward cross-node, but it does not")
	}
	if !d.Broadcast {
		t.Errorf("healing.precondition.missed must broadcast to all replicas, got targeted %q", d.TargetType)
	}

	// Sanity: the sibling automation.* lifecycle events stay LOCAL (blocked),
	// proving the miss signal needed its own non-automation topic.
	if a := evaluateRouting(rules, "automation.completed"); a.Forward {
		t.Errorf("automation.completed must stay local (blocked), but it forwards -- the miss signal cannot ride automation.*")
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
// NOTE: the cognition rules this note used to describe went with the cognition
// node type (epic memql#4988).
//
// # What memql#4542 changed here, and what it deliberately did not
//
// This test originally asserted its invariant through a PROXY: it listed the
// topics the retired rules used to forward and required all of them to be
// dark. That was sound while nothing else wanted them -- and it stopped being
// sound the moment something did. memql#4542 added browser-reach rules for
// v1:agents:* (the Agents view and Nexus) and for DELETES (rows that
// vanished everywhere except the screen), so several of those topics now
// forward again for a reason that has nothing to do with caching.
//
// THE INVARIANT IS UNCHANGED and is still worth a gate: cache eviction must
// not ride per-concept graph-write forwarding. What changed is how it is
// measured. v1:router:budget is now the witness -- the one concept in the
// retired set that no surface subscribes to and no other rule covers -- so a
// resurrected per-concept cache rule still fails here, while a reasoned reach
// rule does not. The positive half of the eviction invariant lives in
// TestEvaluateRouting_CacheInvalidateBroadcast, which pins the channel that
// replaced them.
//
// Deleting this test instead would have been the tempting repair and the wrong
// one: it would leave "eviction does not ride graph forwarding" asserted
// nowhere, and the next person to add a per-concept rule for cache reasons
// would get a green suite.
func TestEvaluateRouting_PerConceptCacheRulesRetired(t *testing.T) {
	rules := defaultRoutingRules()

	// The witness. These had a 5.5 cache forward rule, they are not
	// subscribed by any surface, and nothing else has a reason to carry
	// them -- so a forward here can only mean a retired rule came back.
	retired := []string{
		"graph.node.created.v1:router:budget",
		"graph.node.updated.v1:router:budget",
		"graph.node.deleted.v1:router:budget",
	}
	for _, topic := range retired {
		if d := evaluateRouting(rules, topic); d.Forward {
			t.Errorf("topic %q must NOT be forwarded -- the 5.5 per-concept cache routing rule is retired in 5.6 (eviction rides cache.invalidate.* now); a forward here means a retired rule lingered", topic)
		}
	}

	// The reachable positive, in the same test, so the assertion above can
	// never quietly become vacuous: the topics that LEFT this list forward
	// today, and they forward because memql#4542 gave them a stated reason
	// in routing.go. If these ever go dark again, the Agents view and Nexus
	// are frozen in the mesh and this is where it is written down.
	for _, topic := range []string{
		"graph.node.created.v1:agents:agent",
		"graph.node.updated.v1:agents:agentAuthorization",
		"graph.node.deleted.v1:library:artifact",
	} {
		if d := evaluateRouting(rules, topic); !d.Forward {
			t.Errorf("topic %q must be forwarded -- memql#4542 added a reasoned browser-reach rule for it; losing that rule freezes the surface that subscribes to it", topic)
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

// TestEvaluateRouting_SiteEdgeInvalidationBroadcast pins the cross-node
// correctness guarantee for the site edge's resolver cache (memql#3714, Task
// 9): a v1:platform:site write must reach EVERY edge replica's own
// process-local cache (component/edge/resolve.go), not just the replica that
// handled the write. A single-node green test would not catch a missing
// forward -- each replica's Resolver cache is independent.
//
// Both created and updated forward: createSite is an insert(), while
// updateSiteBundle / updateSiteStatus (the deploy/rollback and
// draft/live/disabled transitions) go through update(). deleted is
// deliberately NOT asserted here -- there is no deleteSite mutation in the
// DSL today, so asserting a forward rule for it would pin behaviour nothing
// exercises.
func TestEvaluateRouting_SiteEdgeInvalidationBroadcast(t *testing.T) {
	rules := defaultRoutingRules()

	for _, topic := range []string{
		"graph.node.created.v1:platform:site",
		"graph.node.updated.v1:platform:site",
	} {
		d := evaluateRouting(rules, topic)
		if !d.Forward {
			t.Errorf("topic %q must forward cross-node so every edge replica invalidates its cache, but it does not", topic)
		}
		if !d.Broadcast {
			t.Errorf("topic %q must broadcast to every edge replica, got targeted %q", topic, d.TargetType)
		}
	}

	// A sibling platform concept must NOT ride this rule -- it is scoped to
	// site, not to the whole v1:platform:* namespace.
	if d := evaluateRouting(rules, "graph.node.created.v1:platform:globalVariable"); d.Forward {
		t.Error("graph.node.created.v1:platform:globalVariable must NOT be forwarded by the site invalidation rule (no wildcard on the concept segment)")
	}
}

// TestPackageRowsReachABrowserOnAnotherNode is the cross-node half of epic
// memql#4794, and it is a routing assertion rather than a live-cluster lane on
// purpose.
//
// THE PIPELINE ITSELF MAKES NO CROSS-NODE HANDOFF. A deployment advances
// through the D6 order inside ONE Deploy() call on ONE node -- component/
// packages/pipeline.go -- so there is no stage-to-stage hop to test. The design
// anticipated stage handoffs as row events and this implementation does not
// need them; what it costs is stated there: a node lost mid-deploy strands its
// row at a non-terminal status, and the recovery is the one the append-only
// rule already prescribes, a new attempt.
//
// What IS cross-node is the READ. Every row the pipeline writes is written on
// whichever node ran it and read in an OS window some bff is serving, so
// without these rules default-deny leaves the packages list correct on load and
// frozen afterwards -- the failure that looks like it is working.
//
// `updated` carries most of the weight: a deploy advances by writing its own
// status six times, and a person watching a deploy is watching exactly those.
func TestPackageRowsReachABrowserOnAnotherNode(t *testing.T) {
	rules := defaultRoutingRules()
	for _, topic := range []string{
		"graph.node.created.v1:platform:package",
		"graph.node.updated.v1:platform:package",
		"graph.node.created.v1:platform:packageDeployment",
		"graph.node.updated.v1:platform:packageDeployment",
	} {
		d := evaluateRouting(rules, topic)
		if !d.Forward {
			t.Errorf("%s does not forward: the OS would show a list that is correct on load and never moves again", topic)
		}
		if !d.Broadcast {
			t.Errorf("%s forwards to one node type rather than broadcasting: whichever bff is serving the window has to receive it", topic)
		}
	}

	// The reachable positive that makes the assertions above evidence about
	// the table rather than about evaluateRouting: a concept with no rule
	// does NOT forward.
	if d := evaluateRouting(rules, "graph.node.updated.v1:platform:packageNotAThing"); d.Forward {
		t.Error("control failed: an unlisted concept forwarded, so the checks above prove nothing")
	}
}

// TestSourceCredentialRowsReachABrowserOnAnotherNode is the packages
// assertion above, for the rows epic memql#4885 added beside them.
//
// A credential is created by a capability on whichever bff the person's
// stream landed on, revoked the same way, and touched by the fetcher or the
// poll on whichever node ran those -- and read in the Settings Sources group
// and on a Source stop's credential chip in a window some other bff is
// serving. Both verbs: a revoke is an UPDATE, and a chip that stays green
// after a revoke is the failure that looks like it is working.
func TestSourceCredentialRowsReachABrowserOnAnotherNode(t *testing.T) {
	rules := defaultRoutingRules()
	for _, topic := range []string{
		"graph.node.created.v1:platform:sourceCredential",
		"graph.node.updated.v1:platform:sourceCredential",
	} {
		d := evaluateRouting(rules, topic)
		if !d.Forward {
			t.Errorf("%s does not forward: a revoked credential would stay green on every other bff's Source stop", topic)
		}
		if !d.Broadcast {
			t.Errorf("%s forwards to one node type rather than broadcasting: whichever bff is serving the window has to receive it", topic)
		}
	}

	// The reachable positive, so the checks above are evidence about the
	// table rather than about evaluateRouting: a sibling concept with no rule
	// does NOT forward.
	if d := evaluateRouting(rules, "graph.node.updated.v1:platform:sourceCredentialNotAThing"); d.Forward {
		t.Error("control failed: an unlisted concept forwarded, so the checks above prove nothing")
	}
}
