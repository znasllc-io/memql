package node

import (
	"strings"

	"github.com/znasllc-io/memql/component/events"
)

// CROSS-NODE REACH, as a question anything can ask (memql#4542/#4543).
//
// routing.go answers "does this topic forward" for the event bridge, in
// unexported code the bridge calls on every publish. This file is the
// same answer, exported, plus the other half of the decision that had no
// home at all until now: the concepts the mesh deliberately does NOT
// carry, and why.
//
// # Why the exclusions are code rather than a comment
//
// Default-deny means an unrouted concept is indistinguishable from a
// concept nobody has thought about. Both are silence. The failure that
// produces -- a page that is correct on load and frozen after -- is the
// one routing.go's own header calls "the worst of the three possible
// behaviours because it looks like it is working", and it has now been
// found by a human four times (memql#4349's fleet pages, and the saved
// views / agents / library / cognition-deletes holes memql#4542 closed).
//
// A recorded exclusion is what makes the two cases different. After
// this file, "not forwarded" is either a decision with a reason attached
// or a gate failure -- and TestRecordedExclusionsStayExcluded, in
// routing_reach_test.go beside this file, is what turns that into a
// build failure rather than a convention. (It lived in the repo-root
// portal_subscription_routing_test.go until epic memql#4984 retired the
// portal; the gate is the same one, moved next to what it guards.)
//
// # What an exclusion is NOT
//
// It is not a to-do, and it is not permission to skip the thinking. An
// exclusion says the mesh should not carry this concept and states the
// cost that decided it. Reversing one is a one-line change with a new
// reason, which is exactly how much ceremony that decision deserves --
// see the v1:identity:user UPDATED entry, which is reversible the moment
// someone is willing to pay for it.

// GraphEventTopic composes the bus topic for a graph row event.
//
// THE ONE PLACE THIS FORMAT IS SPELLED for callers outside the engine's
// publish path. component/memql/concept_resolver.go builds the same
// string when it rewrites @trigger(on=x.created); a second hand-rolled
// fmt.Sprintf in a test or a gate is how a checker comes to assert
// against a topic nothing publishes -- which passes, forever, while
// measuring nothing.
//
// verb is one of "created" / "updated" / "deleted". No validation: a
// caller passing something else gets a topic that matches no rule, which
// is the honest answer rather than a panic in a routing helper.
func GraphEventTopic(verb, conceptId string) string {
	return "graph.node." + strings.TrimSpace(verb) + "." + strings.TrimSpace(conceptId)
}

// ForwardsGraphEvent reports whether an event on this topic is forwarded
// to peers under the EFFECTIVE rule set -- the built-ins plus everything
// registered through RegisterRoutingRule by whichever packages this
// binary compiled in.
//
// It calls the same evaluateRouting the event bridge calls, deliberately.
// A gate that re-implemented the matcher, or compared against a string
// copy of the table, would be asserting about its own copy: the wildcard
// semantics alone (`v1:planner:*` is an INTRA-segment glob, not a
// segment wildcard, because a concept id contains no dots) are enough to
// make a plausible re-implementation disagree with the real one on
// exactly the rules that matter.
func ForwardsGraphEvent(topic string) bool {
	return evaluateRouting(defaultRoutingRules(), topic).Forward
}

// RoutingExclusion is one concept-and-verb the mesh deliberately does
// not carry.
type RoutingExclusion struct {
	// Pattern is matched against event topics with the same matcher the
	// routing rules use, so `graph.node.*.v1:worker:invocation` excludes
	// all three verbs and a fully-spelled topic excludes exactly one.
	Pattern string

	// Reason is why. REQUIRED, and the gate fails on an empty one: an
	// exclusion without a why is a hole with paperwork, and the next
	// reader cannot tell it from an oversight that someone silenced.
	Reason string
}

// RoutingExclusions returns the recorded exclusions.
//
// Ordered by how likely a reader is to be looking for them, not
// alphabetically: the judgment calls first, then the volume denials.
func RoutingExclusions() []RoutingExclusion {
	return []RoutingExclusion{
		{
			Pattern: "graph.node.updated.v1:identity:user",
			Reason: "The user row churns on every token refresh -- lastSeenAt moves on " +
				"an ordinary request -- so forwarding its updates turns every " +
				"refresh in the cluster into a mesh-wide broadcast. That is a " +
				"heartbeat amplifier whose volume scales with logged-in users " +
				"times replicas, bought for a Users view that changes when " +
				"somebody's role changes: a few times a year. The view re-seeds " +
				"on navigation, which covers it. CREATED is forwarded and stays " +
				"forwarded -- per-user provisioning depends on it (see the rule " +
				"in routing.go). Decision recorded 2026-08-25 (memql#4541, D1); " +
				"reversible with one rule and a new reason if a surface ever " +
				"needs live role changes badly enough to pay for it.",
		},
		{
			Pattern: "graph.node.deleted.v1:library:file",
			Reason: "Deliberately asymmetric with the created/updated rules beside it. " +
				"A file is the BACKING row for a kind=file artifact and nothing " +
				"subscribes to it directly: the Artifacts page and Nexus both " +
				"watch v1:library:artifact, whose own delete is forwarded, so " +
				"the surface already learns the thing is gone. Forwarding the " +
				"second row would deliver a second event for one user-visible " +
				"removal.",
		},
		{
			Pattern: "graph.node.*.v1:worker:invocation",
			Reason: "One row per tool call. This is the volume exclusion the rest are " +
				"measured against, and it predates the exclusions table " +
				"(memql#4349 recorded it as a comment beside the Fleet rules). " +
				"The Fleet page reads a machine's recent calls on demand rather " +
				"than tailing them, so nothing is lost.",
		},
		{
			Pattern: "graph.node.*.v1:campaigns:delivery",
			Reason: "One row per recipient per send. A single campaign to a modest " +
				"list is a larger burst than everything else in this table put " +
				"together, and the campaigns surface reports progress by " +
				"counting rows rather than by watching them arrive.",
		},
		{
			Pattern: "graph.node.*.v1:campaigns:engagementEvent",
			Reason: "One row per open and per click, which is the delivery row's volume " +
				"and then some: a pixel is fetched every time a message is " +
				"OPENED, and mail-client prefetchers fetch it on arrival, so a " +
				"campaign produces more engagement rows than it sent messages. " +
				"Recorded beside its delivery sibling deliberately (memql#4827): " +
				"the five operator-facing campaigns concepts ARE forwarded now, " +
				"so without this entry these two would be indistinguishable from " +
				"the concepts nobody had thought about -- which is what all of " +
				"them were until that issue. Stats read the rows through " +
				"campaignStats, an aggregate that computes unique by (delivery, " +
				"kind), rather than by tailing them.",
		},
		{
			Pattern: "graph.node.*.v1:campaigns:recipient",
			Reason: "A roster row, and the judgment call of the three campaigns " +
				"entries rather than a flat volume denial. Hand-editing an " +
				"audience is human-paced and would be affordable; CSV IMPORT is " +
				"not, and it is the same concept. An import writes one row per " +
				"address in one pass, so a 20k-address list is a 20k-event burst " +
				"proportional to the file somebody dropped rather than to " +
				"anything a person did -- the property that separates this table's " +
				"forwarded rules from its exclusions. The cost is stated rather " +
				"than hidden: an address added in one tab does not appear in " +
				"another, and an unsubscribe does not flip the roster row live; " +
				"the roster re-reads on navigation and the app pages it. " +
				"Reversible, and the natural repair is narrower than a reversal " +
				"-- forwarding UPDATED only would buy the live unsubscribe flip " +
				"without the import burst, in the shape the v1:identity:user " +
				"entry above uses in the other direction.",
		},
		{
			Pattern: "graph.node.*.v1:identity:authActivity",
			Reason: "The refresh-rotation mechanics log, split out from " +
				"v1:identity:auditEvent by memql#4328 precisely because it is " +
				"'two orders of magnitude more numerous'. Forwarding it would " +
				"undo that split at the mesh layer. auditEvent -- the DECISIONS " +
				"log -- IS forwarded on created; that asymmetry is the whole " +
				"point of there being two concepts.",
		},
		{
			Pattern: "graph.node.*.v1:observability:invocation",
			Reason: "A row per instrumented call, on a hypertable sized for exactly " +
				"that. Invocation-class volume by the same measure as " +
				"v1:worker:invocation. The read this data is shaped for is " +
				"codeMetricsInWindow -- an aggregate over a time range, asked " +
				"on navigation -- not a subscription. The portal's module " +
				"drill-in was the caller until epic memql#4984 retired it; no " +
				"client asks today, which lowers the cost of this exclusion " +
				"rather than changing it.",
		},
		{
			Pattern: "graph.node.*.v1:observability:codeMetric",
			Reason: "Per-(FQN, window) aggregate rows, written continuously by the " +
				"1m/1h continuous aggregates. Lower volume than the invocations " +
				"underneath, still far above anything else here, and read the " +
				"same way: a windowed query on navigation, not a tail.",
		},
	}
}

// ExcludedFromForwarding reports whether a topic is covered by a
// recorded exclusion, and returns the record.
//
// Uses events.Match rather than string equality so a pattern excluding
// all three verbs of a concept is one entry rather than three, matching
// how the forward rules are written.
func ExcludedFromForwarding(topic string) (RoutingExclusion, bool) {
	for _, ex := range RoutingExclusions() {
		if events.Match(ex.Pattern, topic) {
			return ex, true
		}
	}
	return RoutingExclusion{}, false
}
