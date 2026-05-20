package cognition

import (
	"testing"

	"github.com/znasllc-io/memql/component/events"
)

// TestSubscriptionPatternsMatchEmittedTopics is the regression guard for
// the historical "SI responses silently stop" bug. Now that #56 phase 8
// retired the partition segment, the topic shape is
// `graph.node.{action}.{concept}` and patterns must match it exactly.
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

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			topic := events.BuildTopicWithConcept(events.TopicGraphNodeCreated, tc.concept)
			if !events.Match(tc.pattern, topic) {
				t.Fatalf("pattern %q did not match emitted topic %q",
					tc.pattern, topic)
			}
		})
	}
}

// TestSubscriptionPatternsRejectWrongConcept guards against a pattern
// accidentally matching a different concept.
func TestSubscriptionPatternsRejectWrongConcept(t *testing.T) {
	topic := events.BuildTopicWithConcept(events.TopicGraphNodeCreated, "v1:cognition:space")
	if events.Match(eventPatternUtteranceCreated, topic) {
		t.Fatalf("utterance pattern %q should not match space topic %q",
			eventPatternUtteranceCreated, topic)
	}
}

// TestSubscriptionPatternsRejectDifferentAction guards against matching on
// updated/deleted when only created was requested.
func TestSubscriptionPatternsRejectDifferentAction(t *testing.T) {
	topic := events.BuildTopicWithConcept(events.TopicGraphNodeUpdated, "v1:cognition:utterance")
	if events.Match(eventPatternUtteranceCreated, topic) {
		t.Fatalf("created pattern %q should not match updated topic %q",
			eventPatternUtteranceCreated, topic)
	}
}
