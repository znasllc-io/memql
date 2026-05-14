package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	memoryNodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	"github.com/visionarys-io/memql/component/events"
)

// ConceptMetadata is the JSON representation of a single concept in the API response.
type ConceptMetadata struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Domain      string `json:"domain"`
	Entity      string `json:"entity"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"`
}

// ConceptsResponse is the JSON response for GET /api/concepts.
type ConceptsResponse struct {
	Concepts     []ConceptMetadata `json:"concepts"`
	BaseTopics   []string          `json:"baseTopics"`
	SystemTopics []string          `json:"systemTopics"`
}

// DomainSubscription describes the subscription filters for a single domain.
type DomainSubscription struct {
	Domain  string   `json:"domain"`
	Filters []string `json:"filters"`
}

// DomainSubscribeResponse is the JSON response for GET /api/concepts/subscribe.
type DomainSubscribeResponse struct {
	Domains []DomainSubscription `json:"domains"`
}

// RegisterConceptsEndpoint registers the GET /api/concepts handler on the given mux.
// The handler returns metadata for all registered concepts and available event topics.
//
// It also registers GET /api/concepts/subscribe?domains=cognition,data which returns
// subscription filters grouped by domain for the domain-based subscription pattern.
func RegisterConceptsEndpoint(mux *http.ServeMux, registry memoryNodes.Registry) {
	response := buildConceptsResponse(registry)
	domainFilters := buildDomainFilters(response.Concepts)
	mux.HandleFunc("GET /api/concepts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		json.NewEncoder(w).Encode(response)
	})

	mux.HandleFunc("GET /api/concepts/subscribe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=3600")

		domainsParam := r.URL.Query().Get("domains")
		if domainsParam == "" {
			// Return all domains
			json.NewEncoder(w).Encode(DomainSubscribeResponse{Domains: domainFilters})
			return
		}

		// Filter to requested domains
		requested := make(map[string]bool)
		for _, d := range strings.Split(domainsParam, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				requested[d] = true
			}
		}

		var filtered []DomainSubscription
		for _, ds := range domainFilters {
			if requested[ds.Domain] {
				filtered = append(filtered, ds)
			}
		}

		json.NewEncoder(w).Encode(DomainSubscribeResponse{Domains: filtered})
	})
}

func buildConceptsResponse(registry memoryNodes.Registry) ConceptsResponse {
	concepts := registry.List()
	metadata := make([]ConceptMetadata, 0, len(concepts))

	for _, c := range concepts {
		version, domain, entity := parseConceptId(c.Name)
		metadata = append(metadata, ConceptMetadata{
			ID:          c.Name,
			Version:     version,
			Domain:      domain,
			Entity:      entity,
			Description: c.Description,
			Type:        c.NodeType,
		})
	}

	return ConceptsResponse{
		Concepts: metadata,
		BaseTopics: []string{
			events.TopicGraphNodeCreated,
			events.TopicGraphNodeUpdated,
			events.TopicGraphNodeDeleted,
		},
		SystemTopics: []string{
			events.TopicSessionOpened,
			events.TopicSessionClosed,
		},
	}
}

// ConceptAPIPaths returns public paths for the concept metadata API.
func ConceptAPIPaths() []string {
	return []string{"/api/concepts", "/api/concepts/subscribe"}
}

// buildDomainFilters groups concepts by domain and generates CDC subscription
// filters for each. This powers the domain-based subscription pattern where
// clients subscribe by domain and automatically get all concepts in that domain.
func buildDomainFilters(concepts []ConceptMetadata) []DomainSubscription {
	domainMap := make(map[string][]string)
	for _, c := range concepts {
		if c.Domain == "" {
			continue
		}
		// Build the CDC filter (without "graph." prefix, matching CDCFilters convention)
		filter := "node.created." + c.ID
		domainMap[c.Domain] = append(domainMap[c.Domain], filter)
	}

	// Sort domains for stable output
	domains := make([]string, 0, len(domainMap))
	for d := range domainMap {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	result := make([]DomainSubscription, 0, len(domains))
	for _, d := range domains {
		result = append(result, DomainSubscription{
			Domain:  d,
			Filters: domainMap[d],
		})
	}
	return result
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
