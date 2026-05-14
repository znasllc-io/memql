package memql

import (
	"strings"

	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
)

// scopeGraphPatternToPartition narrows a graph-event subscription
// pattern to the caller's envelope partition. Emitted graph topics
// are `graph.node.{action}.{partition}.{concept}`; this helper
// substitutes the envelope partition for any `*` partition segment
// the client supplied so a single session can never observe other
// tenants' events.
//
// The rewrite is a no-op for non-graph subscription kinds (telemetry,
// AI streams, query specs, etc.) because their topics aren't
// partition-tagged the same way. It's also a no-op when the envelope
// partition is empty (handshake / control messages aren't supposed
// to subscribe at all, but failing-open here keeps the pattern
// deliverable rather than silently mismatching everything).
//
// The pattern arrives already normalized by
// events.TopicPatternFromSubscriptionKind (the only call site), so
// `graph.node.created.*.v1:cognition:utterance` is the canonical
// shape we substitute into. Patterns that don't match the canonical
// shape (custom prefix tries, multi-segment partition wildcards,
// etc.) pass through unchanged -- caller knows what they're doing.
func scopeGraphPatternToPartition(pattern string, kind memqlv1.SubscriptionKind, envelopePartition string) string {
	if kind != memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS {
		return pattern
	}
	envelopePartition = strings.TrimSpace(envelopePartition)
	if envelopePartition == "" {
		return pattern
	}

	// Graph topics are 5 segments: `graph.{noun}.{action}.{partition}.{concept}`.
	// Anything else stays as-is.
	const partitionIdx = 3
	segments := strings.Split(pattern, ".")
	if len(segments) < partitionIdx+1 {
		return pattern
	}
	if segments[0] != "graph" {
		return pattern
	}
	if segments[partitionIdx] != "*" {
		// Already concrete (or some other wildcard); leave the
		// caller's intent alone. The only thing we forbid is the
		// implicit "give me every partition" wildcard.
		return pattern
	}
	segments[partitionIdx] = envelopePartition
	return strings.Join(segments, ".")
}
