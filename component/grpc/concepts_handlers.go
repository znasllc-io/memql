package memql

import (
	"sort"
	"strings"

	"google.golang.org/grpc/codes"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// conceptInfoFromConcept projects a registry Concept onto the wire ConceptInfo,
// including the @displayCard(...) rendering hints (memql#160). Shared by the
// one-shot ConceptsListMsg and the follow-mode registry-delta stream (memql#4238)
// so the two can never disagree on the descriptor a client sees.
func conceptInfoFromConcept(c *memoryNodes.Concept) *memqlv1.ConceptInfo {
	if c == nil {
		return nil
	}
	version, domain, entity := parseConceptId(c.Name)
	info := &memqlv1.ConceptInfo{
		Id:          c.Name,
		Version:     version,
		Domain:      domain,
		Entity:      entity,
		Description: c.Description,
		Type:        c.NodeType,
	}
	if c.DisplayCard != nil {
		info.DisplayCard = &memqlv1.DisplayCard{
			Primary:   c.DisplayCard.Primary,
			Secondary: c.DisplayCard.Secondary,
			Tertiary:  c.DisplayCard.Tertiary,
			Status:    c.DisplayCard.Status,
		}
	}
	// The data-origins declaration (epic memql#4378). Sent on EVERY
	// concept, including native ones, because "native" is an answer a
	// client needs and its absence is not: a badge that renders only
	// when a field is present cannot distinguish "MemQL owns this" from
	// "this server is too old to say".
	//
	// Origin carries the EFFECTIVE value rather than the stored one --
	// "memql" where the concept declared nothing -- so no client
	// re-derives the default, and the three of them (portal, both SDKs)
	// cannot drift about it.
	info.DataState = string(c.DataState())
	info.DataOrigin = c.EffectiveOrigin()
	info.DataMirroredTo = append([]string(nil), c.MirroredTo...)
	// The declared SHAPE (epic memql#4661): the concept's fields and its
	// relationships, projected in concept_shape.go. It lands here rather
	// than in a second projection for the same reason the display card and
	// the data-origins block do -- this function is the ONE thing both wire
	// paths call, so the one-shot list and the follow-mode registry delta
	// cannot disagree about what a client was told.
	info.Fields, info.Relationships = conceptShape(c)
	return info
}

// handleConceptsList returns metadata for all registered concepts and available event topics.
func (s *streamSession) handleConceptsList(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.ConceptsListMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	if s.service.conceptRegistry == nil {
		s.sendQueryError(requestId, correlate, codes.Unavailable, "concept registry not configured")
		return nil
	}

	concepts := s.service.conceptRegistry.List()
	infos := make([]*memqlv1.ConceptInfo, 0, len(concepts))
	for _, c := range concepts {
		infos = append(infos, conceptInfoFromConcept(c))
	}

	s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ConceptsListResult{
			ConceptsListResult: &memqlv1.ConceptsListResult{
				RequestId: requestId,
				Concepts:  infos,
				BaseTopics: []string{
					events.TopicGraphNodeCreated,
					events.TopicGraphNodeUpdated,
					events.TopicGraphNodeDeleted,
				},
				SystemTopics: []string{
					events.TopicSessionOpened,
					events.TopicSessionClosed,
				},
			},
		},
	})
	return nil
}

// handleConceptsSubscribe returns CDC subscription filters grouped by domain.
func (s *streamSession) handleConceptsSubscribe(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.ConceptsSubscribeMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	if s.service.conceptRegistry == nil {
		s.sendQueryError(requestId, correlate, codes.Unavailable, "concept registry not configured")
		return nil
	}

	// follow mode (memql#4238): snapshot + live registry-change deltas, instead
	// of the one-shot CDC-filter catalog below.
	if msg.GetFollow() {
		return s.handleConceptsFollow(requestId, correlate)
	}

	concepts := s.service.conceptRegistry.List()

	// Build domain -> filters map.
	domainMap := make(map[string][]string)
	for _, c := range concepts {
		_, domain, _ := parseConceptId(c.Name)
		if domain == "" {
			continue
		}
		filter := "node.created." + c.Name
		domainMap[domain] = append(domainMap[domain], filter)
	}

	// Apply optional domain filter.
	requestedDomains := msg.GetDomains()
	requested := make(map[string]bool, len(requestedDomains))
	for _, d := range requestedDomains {
		d = strings.TrimSpace(d)
		if d != "" {
			requested[d] = true
		}
	}

	// Sort domains for stable output.
	domains := make([]string, 0, len(domainMap))
	for d := range domainMap {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	result := make([]*memqlv1.DomainSubscription, 0, len(domains))
	for _, d := range domains {
		if len(requested) > 0 && !requested[d] {
			continue
		}
		result = append(result, &memqlv1.DomainSubscription{
			Domain:  d,
			Filters: domainMap[d],
		})
	}

	s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ConceptsSubscribeResult{
			ConceptsSubscribeResult: &memqlv1.ConceptsSubscribeResult{
				RequestId: requestId,
				Domains:   result,
			},
		},
	})
	return nil
}

// handleConceptsFollow opens a registry-DELTA subscription (memql#4238): it
// answers with a snapshot delta (reset=true, the whole concept set at the
// current generation) and then streams add/remove deltas from the engine's
// in-process broadcaster until the client unsubscribes by the returned
// subscription_id or the stream closes.
//
// The subscription is registered in s.unsubscribers under that id, so BOTH an
// explicit UnsubscribeMsg (handleUnsubscribe) and stream teardown (shutdown()
// ranges s.unsubscribers) stop delivery -- the same lifecycle the CDC
// subscriptions use.
func (s *streamSession) handleConceptsFollow(requestId, correlate string) error {
	if s.service.engine == nil {
		s.sendQueryError(requestId, correlate, codes.FailedPrecondition,
			"concept registry follow requires an engine on this node")
		return nil
	}

	sub := s.service.engine.SubscribeConceptRegistry()
	subscriptionId := id.NewShortId()
	s.unsubscribers.Store(subscriptionId, sub.Unsubscribe)

	added := make([]*memqlv1.ConceptInfo, 0, len(sub.Snapshot))
	for _, c := range sub.Snapshot {
		added = append(added, conceptInfoFromConcept(c))
	}

	// The snapshot. reset=true tells the client to replace its whole registry
	// with `added`. Correlated to the request's message id so a caller can pair
	// the first reply; every delta also carries request_id, which is what the
	// SDK matches on across the whole stream.
	if err := s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ConceptsRegistryDelta{
			ConceptsRegistryDelta: &memqlv1.ConceptsRegistryDelta{
				RequestId:      requestId,
				Generation:     sub.Generation,
				Added:          added,
				Reset_:         true,
				SubscriptionId: subscriptionId,
			},
		},
	}); err != nil {
		sub.Unsubscribe()
		s.unsubscribers.Delete(subscriptionId)
		return err
	}

	go s.forwardConceptRegistryDeltas(subscriptionId, requestId, sub.Deltas)
	return nil
}

// forwardConceptRegistryDeltas relays deltas from the engine broadcaster to the
// client as ConceptsRegistryDelta pushes (correlate "" -- unsolicited, matched
// client-side by request_id) until the stream closes or the channel is closed
// by the unsubscribe. Mirrors forwardEvents' closeChan discipline.
func (s *streamSession) forwardConceptRegistryDeltas(subscriptionId, requestId string, deltas <-chan memqlengine.ConceptRegistryDelta) {
	for {
		select {
		case <-s.closeChan:
			return
		case delta, ok := <-deltas:
			if !ok {
				return
			}
			added := make([]*memqlv1.ConceptInfo, 0, len(delta.Added))
			for _, c := range delta.Added {
				added = append(added, conceptInfoFromConcept(c))
			}
			_ = s.sendServerMessage("", &memqlv1.MemqlServerMessage{
				Payload: &memqlv1.MemqlServerMessage_ConceptsRegistryDelta{
					ConceptsRegistryDelta: &memqlv1.ConceptsRegistryDelta{
						RequestId:      requestId,
						Generation:     delta.Generation,
						Added:          added,
						Removed:        delta.Removed,
						SubscriptionId: subscriptionId,
					},
				},
			})
		}
	}
}

// parseConceptId splits a concept ID like "v1:cognition:space" into version, domain, and entity.
func parseConceptId(id string) (version, domain, entity string) {
	parts := strings.SplitN(id, ":", 3)
	switch len(parts) {
	case 3:
		return parts[0], parts[1], parts[2]
	case 2:
		return parts[0], parts[1], ""
	case 1:
		return parts[0], "", ""
	default:
		return "", "", ""
	}
}
