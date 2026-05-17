package cognition

import (
	"testing"

	"github.com/znasllc-io/memql/component/events"
)

// TestSubscriptionPatternsMatchEmittedTopics is the regression guard for the
// shipped "SI responses silently stop" bug.
//
// Root cause was integration code subscribing to 4-segment patterns like
// `graph.node.created.v1:cognition:utterance` while emitters produced
// 5-segment topics `graph.node.created.{partition}.{concept}`. The bus
// matcher requires exact segment count, so the subscription never fired,
// handleUtteranceForCognition never ran, and no response was generated.
//
// This test builds representative emitted topics via the canonical helper
// (BuildTopicWithPartitionAndConcept) and asserts every subscription pattern
// declared at package level matches the topic it's supposed to match --
// across default, named, and _system partitions.
func TestSubscriptionPatternsMatchEmittedTopics(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		concept string
	}{
		{"space", eventPatternSpaceCreated, "v1:cognition:space"},
		{"participant", eventPatternParticipantAdded, "v1:cognition:participant"},
		{"session", eventPatternSessionChanged, "v1:cognition:session"},
		{"utterance", eventPatternUtteranceCreated, "v1:cognition:utterance"},
	}

	partitions := []string{"default", "acme", ""}

	for _, tc := range cases {
		for _, partition := range partitions {
			t.Run(tc.name+"/partition="+partition, func(t *testing.T) {
				topic := events.BuildTopicWithPartitionAndConcept(
					events.TopicGraphNodeCreated,
					partition,
					tc.concept,
				)
				if !events.Match(tc.pattern, topic) {
					t.Fatalf("pattern %q did not match emitted topic %q -- bus matcher requires exact segment count",
						tc.pattern, topic)
				}
			})
		}
	}
}

// TestSubscriptionPatternsRejectWrongConcept guards against the partition
// wildcard accidentally bleeding into the concept segment.
// Pattern `graph.node.created.*.v1:cognition:utterance` must NOT match a
// topic for a different concept just because the partition differs.
func TestSubscriptionPatternsRejectWrongConcept(t *testing.T) {
	topic := events.BuildTopicWithPartitionAndConcept(
		events.TopicGraphNodeCreated,
		"default",
		"v1:cognition:space", // not utterance
	)
	if events.Match(eventPatternUtteranceCreated, topic) {
		t.Fatalf("utterance pattern %q should not match space topic %q",
			eventPatternUtteranceCreated, topic)
	}
}

// TestSubscriptionPatternsRejectDifferentAction guards against matching on
// updated/deleted when only created was requested.
func TestSubscriptionPatternsRejectDifferentAction(t *testing.T) {
	topic := events.BuildTopicWithPartitionAndConcept(
		events.TopicGraphNodeUpdated,
		"default",
		"v1:cognition:utterance",
	)
	if events.Match(eventPatternUtteranceCreated, topic) {
		t.Fatalf("created pattern %q should not match updated topic %q",
			eventPatternUtteranceCreated, topic)
	}
}
