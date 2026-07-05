package node

import (
	"testing"

	"github.com/znasllc-io/memql/component/events"
)

// The registry proves the second-product property of the chat-reply seam: a
// pack registers a concept (+ optional space-key field) at init() and its
// events join the substrate gate with zero engine edits. The fixture concept
// is deliberately neutral -- no real product name.

func init() {
	RegisterChatReplyConcept(ChatReplyConcept{
		ConceptID:     "v1:exampleproduct:board",
		SpaceKeyField: "board",
	})
}

func TestRegisteredChatReplyConceptJoinsGate(t *testing.T) {
	created := events.BuildTopicWithConcept(events.TopicGraphNodeCreated, "v1:exampleproduct:board")
	updated := events.BuildTopicWithConcept(events.TopicGraphNodeUpdated, "v1:exampleproduct:board")
	for _, topic := range []string{created, updated} {
		if !isChatReplyTopic(topic) {
			t.Fatalf("registered concept topic %q must be on the substrate path", topic)
		}
	}
	deleted := events.BuildTopicWithConcept(events.TopicGraphNodeDeleted, "v1:exampleproduct:board")
	if isChatReplyTopic(deleted) {
		t.Fatalf("deleted events are not chat-reply payload even for registered concepts")
	}
}

func TestRegisteredSpaceKeyFieldResolves(t *testing.T) {
	key, ok := resolveSpaceKey(map[string]any{"board": "s9"})
	if !ok || key.String() != "space:s9" {
		t.Fatalf("registered field must resolve: got ok=%v key=%v", ok, key)
	}
	// The core field keeps precedence when both are present.
	key, ok = resolveSpaceKey(map[string]any{"partitionId": "core", "board": "extra"})
	if !ok || key.String() != "space:core" {
		t.Fatalf("partitionId must win over registered fields: got ok=%v key=%v", ok, key)
	}
	// Nested payload fallback covers registered fields too.
	key, ok = resolveSpaceKey(map[string]any{"payload": map[string]any{"board": "s10"}})
	if !ok || key.String() != "space:s10" {
		t.Fatalf("nested registered field must resolve: got ok=%v key=%v", ok, key)
	}
}

func TestRegisteredChatReplyConceptsReadAPI(t *testing.T) {
	var found bool
	for _, c := range RegisteredChatReplyConcepts() {
		if c.ConceptID == "v1:exampleproduct:board" && c.SpaceKeyField == "board" {
			found = true
		}
	}
	if !found {
		t.Fatalf("RegisteredChatReplyConcepts must expose the fixture registration")
	}
}

func TestRegisterChatReplyConceptPanics(t *testing.T) {
	assertPanics := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s: expected panic", name)
			}
		}()
		fn()
	}
	assertPanics("empty ConceptID", func() {
		RegisterChatReplyConcept(ChatReplyConcept{})
	})
	assertPanics("duplicate", func() {
		RegisterChatReplyConcept(ChatReplyConcept{ConceptID: "v1:exampleproduct:board"})
	})
}
