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

		// Graph events expect the 4-segment form in the filter
		// (graph.node.{action}.{concept}). The translator just prepends "graph.".
		{
			name:   "graph 4-segment filter",
			kind:   memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
			filter: "node.created.v1:cognition:text:chunk",
			want:   "graph.node.created.v1:cognition:text:chunk",
		},
		{
			name:   "graph intra-segment concept wildcard",
			kind:   memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
			filter: "node.created.v1:cognition:*",
			want:   "graph.node.created.v1:cognition:*",
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
// loop end-to-end: a graph filter produces a pattern that matches the topic
// TopicNodeCreated / TopicNodeUpdated actually produces.
func TestTopicPatternFromSubscriptionKind_GraphEventsMatchesEmitted(t *testing.T) {
	cases := []struct {
		filter  string
		action  string
		concept string
	}{
		{"node.created.v1:cognition:text:chunk", "created", "v1:cognition:text:chunk"},
		{"node.created.v1:cognition:utterance", "created", "v1:cognition:utterance"},
		{"node.updated.v1:cognition:participant:presence", "updated", "v1:cognition:participant:presence"},
		// Intra-segment wildcard matches siblings under the same concept namespace.
		{"node.created.v1:cognition:*", "created", "v1:cognition:utterance"},
		{"node.created.v1:cognition:*", "created", "v1:cognition:text:chunk"},
	}

	for _, tc := range cases {
		t.Run(tc.filter+"_concept="+tc.concept, func(t *testing.T) {
			pattern := TopicPatternFromSubscriptionKind(
				memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
				tc.filter,
			)
			var topic string
			switch tc.action {
			case "created":
				topic = TopicNodeCreated(tc.concept)
			case "updated":
				topic = TopicNodeUpdated(tc.concept)
			case "deleted":
				topic = TopicNodeDeleted(tc.concept)
			}
			if !Match(pattern, topic) {
				t.Fatalf("pattern %q does not match emitted topic %q (filter=%q)",
					pattern, topic, tc.filter)
			}
		})
	}
}
