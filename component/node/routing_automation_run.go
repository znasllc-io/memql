package node

// routing_automation_run.go registers the mesh forwarding rule for the
// operator-initiated automation run path (memql#3310).
//
// WHY THIS FILE EXISTS AT ALL
// ---------------------------
// The run path relays a request from the node that received it to the node
// type that should execute it, and streams the step trace back the same way,
// over the event bus (component/automations/run_relay.go). Event forwarding in
// this package is DEFAULT-DENY: evaluateRouting forwards only what a rule
// matches, and an event matching no rule stays on the node that published it.
//
// So without this registration the relay works perfectly on one node and dies
// in silence in the mesh -- the request is published, nothing forwards it, no
// peer ever sees it, and the caller waits out its deadline with no error to
// explain why. That is precisely the failure mode CLAUDE.md's multi-node rule
// names, and it is the reason the cluster-e2e coverage for this feature is
// written to fail with this file removed.
//
// NO BUILD TAG, DELIBERATELY
// --------------------------
// Build tags on a registering file decide which binaries carry the rule. Here
// the answer is EVERY binary, because both ends of the hop are open-ended: any
// node type can serve MemqlService.Stream and therefore receive a run request,
// and any node type can be the target that executes it. A tagged registration
// would mean some node pair in the mesh silently cannot relay -- the same bug
// in a smaller box.
//
// WHY A SEPARATE TOPIC TREE
// -------------------------
// defaultRoutingRules BLOCKS "automation.#" from crossing the mesh: automation
// lifecycle chatter (started / completed / failed) is node-local by design and
// broadcasting it would multiply into an event storm. The run topics must
// cross, so they live under their own "automationrun." prefix rather than
// under "automation.". Block rules evaluate before forward rules, so a run
// topic moved under "automation." would be blocked by that rule and this
// forward rule would never be reached -- silently, with every unit test still
// green. If you rename these topics, rename them somewhere the block rule does
// not reach.
//
// The topic strings are duplicated here rather than imported from
// component/automations: this package is depended on BY the mesh bootstrap and
// must not pull in the automation engine to state a routing pattern. The
// pattern covers both topics with one rule, and
// TestAutomationRunTopicsAreForwarded pins the two literals against the
// automations package's constants so the duplication cannot drift.
func init() {
	RegisterRoutingRule(RoutingRule{
		// Covers automationrun.request (origin -> executing node) and
		// automationrun.trace (executing node -> origin).
		//
		// Broadcast rather than targeted: the request's target node type is a
		// runtime value carried IN the event, not something a static rule can
		// know, and the trace has to reach whichever node happens to have
		// asked. Both are low-volume by construction -- an operator pressing
		// Run in an editor -- and every receiver filters on (target type,
		// origin node id) before doing anything, so a broadcast that reaches
		// an uninterested node costs one map lookup and a drop.
		Pattern:    "automationrun.#",
		TargetType: "",
	})
}

// ForwardDecisionFor reports how an event on topic would be routed under the
// effective rule set: whether it forwards to peers at all, whether it
// broadcasts, and which node type it targets when it does not.
//
// Exported for tests that need to exercise a cross-node hop WITHOUT standing
// up gRPC peers -- notably the automation-run routing coverage, which drives
// two in-process nodes through a bridge that applies this decision exactly as
// EventBridge.onLocalEvent does. Without an exported view of the decision such
// a test would have to hard-code "this topic forwards", which is the assertion
// it is supposed to be making.
func ForwardDecisionFor(topic string) (forward bool, broadcast bool, targetType NodeType) {
	d := evaluateRouting(defaultRoutingRules(), topic)
	return d.Forward, d.Broadcast, d.TargetType
}
