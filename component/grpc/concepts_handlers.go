package memql

import (
	"sort"
	"strings"

	"google.golang.org/grpc/codes"

	"github.com/visionarys-io/memql/component/events"
	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
)

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
		version, domain, entity := parseConceptId(c.Name)
		infos = append(infos, &memqlv1.ConceptInfo{
			Id:          c.Name,
			Version:     version,
			Domain:      domain,
			Entity:      entity,
			Description: c.Description,
			Type:        c.NodeType,
		})
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
