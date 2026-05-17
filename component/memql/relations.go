package memql

import (
	"strings"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// RelationshipDefinition reuses the concept-level structure for engine consumption.
type RelationshipDefinition = concept.RelationshipDefinition

const (
	relationshipDirectionOutgoing      = "outgoing"
	relationshipDirectionIncoming      = "incoming"
	relationshipDirectionBidirectional = "bidirectional"
)

const (
	relationshipTypeParent    = "parent"
	relationshipTypeAlias     = "alias"
	relationshipTypeEquals    = "equals"
	relationshipTypeInteracts = "interactsWith"
	relationshipTypeContains  = "contains"
	relationshipTypeOwns      = "owns"
	relationshipTypeCreatedBy = "createdBy"
)

type relationshipRegistry struct {
	ByConcept map[string][]RelationshipDefinition
}

type relationshipEdge struct {
	Source     string
	Definition RelationshipDefinition
}

func canonicalRelationshipType(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", false
	}

	normalized := strings.ToLower(strings.ReplaceAll(trimmed, "_", ""))

	switch normalized {
	case "parent":
		return relationshipTypeParent, true
	case "alias":
		return relationshipTypeAlias, true
	case "equals":
		return relationshipTypeEquals, true
	case "interactswith":
		return relationshipTypeInteracts, true
	case "contains":
		return relationshipTypeContains, true
	case "owns":
		return relationshipTypeOwns, true
	case "createdby":
		return relationshipTypeCreatedBy, true
	default:
		return "", false
	}
}
