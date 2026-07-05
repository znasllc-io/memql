package node

import (
	"fmt"
	"sync"

	"github.com/znasllc-io/memql/component/events"
)

// chat_reply_registry.go is the product extension point for the chat-reply
// delivery substrate (chat_reply_delivery.go). The engine's own gate covers
// the core v1:cognition:* concepts; a product pack whose concept rows must
// reach the WS-owning bff replica the same way (e.g. a canvas/board surface
// rendered live in the browser) registers that concept here from an init()
// in its registration-anchor package -- the same compile-in contract as
// RegisterRoutingRule: build tags / pack imports on the caller control which
// binaries include it, and a pure-engine binary simply has an empty registry.

// ChatReplyConcept declares one pack-owned concept whose
// graph.node.created/updated events ride the durable delivery substrate.
type ChatReplyConcept struct {
	// ConceptID is the graph concept id (e.g. "v1:acme:board"). Required.
	ConceptID string
	// SpaceKeyField optionally names an extra top-level payload field
	// carrying the space id for this concept's events. The core
	// "partitionId" field is always checked first; leave empty when the
	// concept's payload carries partitionId.
	SpaceKeyField string
}

var (
	chatReplyRegistryMu sync.RWMutex
	// extraChatReplyConcepts is the registration record (read API).
	extraChatReplyConcepts []ChatReplyConcept
	// extraChatReplyTopics is the materialized hot-path set: the
	// created/updated topics of every registered concept.
	extraChatReplyTopics = map[string]struct{}{}
	// extraSpaceKeyFields is the materialized, deduped list of registered
	// space-key payload fields (checked after the core "partitionId").
	extraSpaceKeyFields []string
)

// RegisterChatReplyConcept adds a pack-owned concept to the chat-reply
// substrate gate. Call from init(); panics on an empty ConceptID or a
// duplicate registration (loud programmer error, matching RegisterPlugin).
func RegisterChatReplyConcept(c ChatReplyConcept) {
	if c.ConceptID == "" {
		panic("node.RegisterChatReplyConcept: empty ConceptID")
	}
	chatReplyRegistryMu.Lock()
	defer chatReplyRegistryMu.Unlock()
	for _, existing := range extraChatReplyConcepts {
		if existing.ConceptID == c.ConceptID {
			panic(fmt.Sprintf("node.RegisterChatReplyConcept: duplicate registration for %q", c.ConceptID))
		}
	}
	extraChatReplyConcepts = append(extraChatReplyConcepts, c)
	extraChatReplyTopics[events.BuildTopicWithConcept(events.TopicGraphNodeCreated, c.ConceptID)] = struct{}{}
	extraChatReplyTopics[events.BuildTopicWithConcept(events.TopicGraphNodeUpdated, c.ConceptID)] = struct{}{}
	if c.SpaceKeyField != "" {
		seen := false
		for _, f := range extraSpaceKeyFields {
			if f == c.SpaceKeyField {
				seen = true
				break
			}
		}
		if !seen {
			extraSpaceKeyFields = append(extraSpaceKeyFields, c.SpaceKeyField)
		}
	}
}

// RegisteredChatReplyConcepts returns the pack-registered concepts, so pack
// tests can pin their registration against drift.
func RegisteredChatReplyConcepts() []ChatReplyConcept {
	chatReplyRegistryMu.RLock()
	defer chatReplyRegistryMu.RUnlock()
	out := make([]ChatReplyConcept, len(extraChatReplyConcepts))
	copy(out, extraChatReplyConcepts)
	return out
}

// isRegisteredChatReplyTopic reports whether a topic belongs to a
// pack-registered chat-reply concept.
func isRegisteredChatReplyTopic(topic string) bool {
	chatReplyRegistryMu.RLock()
	defer chatReplyRegistryMu.RUnlock()
	_, ok := extraChatReplyTopics[topic]
	return ok
}

// resolveKeyFields returns the payload fields resolveSpaceKey checks, in
// precedence order: the core "partitionId" first, then every registered
// space-key field.
func resolveKeyFields() []string {
	chatReplyRegistryMu.RLock()
	defer chatReplyRegistryMu.RUnlock()
	fields := make([]string, 0, 1+len(extraSpaceKeyFields))
	fields = append(fields, "partitionId")
	fields = append(fields, extraSpaceKeyFields...)
	return fields
}
