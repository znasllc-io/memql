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
	// dependsOn models a DAG in-edge between rows (a step depends on other
	// steps): an outgoing edge whose field is a []string of target ids. Used
	// by the harness state model (v1:harness:step, #582). Registered so the
	// engine accepts the concept; graph-expansion traversal of dependsOn edges
	// is intentionally not wired here (no query needs it yet -- the DAG is read
	// from the step's dependsOn field directly by the controller, #583).
	relationshipTypeDependsOn = "dependsOn"
	// formedFrom models a derivation in-edge: a row was synthesised from a set
	// of source rows (a semanticMemory formed from sourceEpisodes observations).
	// Outgoing, field is a []string of target ids. Used by the harness
	// consolidation / semantic-memory model (v1:harness:semanticMemory, #586).
	// Same treatment as dependsOn -- registered for concept load; graph-expansion
	// traversal not wired (the source list is read from the field directly).
	relationshipTypeFormedFrom = "formedFrom"
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
	case "dependson":
		return relationshipTypeDependsOn, true
	case "formedfrom":
		return relationshipTypeFormedFrom, true
	default:
		return "", false
	}
}
