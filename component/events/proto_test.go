package events

import (
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// TestTopicPatternFromSubscriptionKind_Prefix asserts that each kind prepends
// the right namespace and leaves the filter shape alone. Callers own filter
// correctness; this function is just the prefix glue.
func TestTopicPatternFromSubscriptionKind_Prefix(t *testing.T) {
	cases := []struct {
		name   string
		kind   memqlv1.SubscriptionKind
		filter string
		want   string
	}{
		// Root hash short-circuits per kind.
		{"telemetry #", memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_TELEMETRY, "#", "telemetry.#"},
		{"message #", memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_MESSAGE, "#", "message.#"},
		{"graph #", memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS, "#", "graph.#"},
		{"ai #", memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_AI_STREAM, "#", "ai.#"},
		{"query #", memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_QUERY_SPEC, "#", "query.#"},
		{"automation #", memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_AUTOMATION_EVENTS, "#", "automation.#"},
		{"domain #", memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_DOMAIN_EVENTS, "#", "#"},
		{"all", memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_ALL, "anything", "#"},

		// Graph events expect the full 5-segment form in the filter
		// (see BuildTopicWithPartitionAndConcept). The translator just
		// prepends "graph.".
		{
			name:   "graph 5-segment filter",
			kind:   memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
			filter: "node.created.*.v1:cognition:text:chunk",
			want:   "graph.node.created.*.v1:cognition:text:chunk",
		},
		{
			name:   "graph intra-segment concept wildcard",
			kind:   memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
			filter: "node.created.*.v1:cognition:*",
			want:   "graph.node.created.*.v1:cognition:*",
		},
		{
			name:   "graph literal partition",
			kind:   memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
			filter: "node.updated.default.v1:cognition:participant:presence",
			want:   "graph.node.updated.default.v1:cognition:participant:presence",
		},

		// Other kinds: plain prefix.
		{
			name:   "domain filter is prefix-free",
			kind:   memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_DOMAIN_EVENTS,
			filter: "hr.checkin.completed",
			want:   "hr.checkin.completed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TopicPatternFromSubscriptionKind(tc.kind, tc.filter)
			if got != tc.want {
				t.Fatalf("TopicPatternFromSubscriptionKind(%v, %q) = %q, want %q",
					tc.kind, tc.filter, got, tc.want)
			}
		})
	}
}

// TestTopicPatternFromSubscriptionKind_GraphEventsMatchesEmitted closes the
// loop end-to-end: a 5-segment graph filter produces a pattern that matches
// the topic TopicNodeCreated / TopicNodeUpdated actually produces. If
// subscription and emission ever drift apart again (they have three times
// now), this test fails.
func TestTopicPatternFromSubscriptionKind_GraphEventsMatchesEmitted(t *testing.T) {
	cases := []struct {
		filter    string
		action    string
		partition string
		concept   string
	}{
		{"node.created.*.v1:cognition:text:chunk", "created", "default", "v1:cognition:text:chunk"},
		{"node.created.*.v1:cognition:utterance", "created", "acme", "v1:cognition:utterance"},
		{"node.updated.*.v1:cognition:participant:presence", "updated", "", "v1:cognition:participant:presence"},
		// Intra-segment wildcard matches siblings under the same concept namespace.
		{"node.created.*.v1:cognition:*", "created", "default", "v1:cognition:utterance"},
		{"node.created.*.v1:cognition:*", "created", "default", "v1:cognition:text:chunk"},
		// Literal partition filter matches only that partition.
		{"node.created.default.v1:cognition:utterance", "created", "default", "v1:cognition:utterance"},
	}

	for _, tc := range cases {
		t.Run(tc.filter+"_partition="+tc.partition+"_concept="+tc.concept, func(t *testing.T) {
			pattern := TopicPatternFromSubscriptionKind(
				memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
				tc.filter,
			)
			var topic string
			switch tc.action {
			case "created":
				topic = TopicNodeCreated(tc.partition, tc.concept)
			case "updated":
				topic = TopicNodeUpdated(tc.partition, tc.concept)
			case "deleted":
				topic = TopicNodeDeleted(tc.partition, tc.concept)
			}
			if !Match(pattern, topic) {
				t.Fatalf("pattern %q does not match emitted topic %q (filter=%q)",
					pattern, topic, tc.filter)
			}
		})
	}
}
