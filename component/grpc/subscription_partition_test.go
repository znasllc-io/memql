package memql

import (
	"testing"

	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
)

func TestScopeGraphPatternToPartition(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		pattern   string
		kind      memqlv1.SubscriptionKind
		partition string
		want      string
	}{
		{
			name:      "rewrites wildcard partition for graph events",
			pattern:   "graph.node.created.*.v1:cognition:utterance",
			kind:      memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
			partition: "acme",
			want:      "graph.node.created.acme.v1:cognition:utterance",
		},
		{
			name:      "leaves concrete partition alone",
			pattern:   "graph.node.created.acme.v1:cognition:utterance",
			kind:      memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
			partition: "wrong-partition",
			want:      "graph.node.created.acme.v1:cognition:utterance",
		},
		{
			name:      "no-op when envelope partition empty",
			pattern:   "graph.node.created.*.v1:cognition:utterance",
			kind:      memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
			partition: "",
			want:      "graph.node.created.*.v1:cognition:utterance",
		},
		{
			name:      "no-op for non-graph kinds",
			pattern:   "telemetry.foo",
			kind:      memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_TELEMETRY,
			partition: "acme",
			want:      "telemetry.foo",
		},
		{
			name:      "no-op for ai stream",
			pattern:   "ai.chat.delta",
			kind:      memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_AI_STREAM,
			partition: "acme",
			want:      "ai.chat.delta",
		},
		{
			name:      "leaves non-canonical short patterns alone",
			pattern:   "graph.x",
			kind:      memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
			partition: "acme",
			want:      "graph.x",
		},
		{
			name:      "trims envelope partition whitespace before substitution",
			pattern:   "graph.node.created.*.v1:platform:partition",
			kind:      memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
			partition: "  team-42  ",
			want:      "graph.node.created.team-42.v1:platform:partition",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := scopeGraphPatternToPartition(tc.pattern, tc.kind, tc.partition)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
