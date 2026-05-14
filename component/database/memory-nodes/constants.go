package memoryNodes

import "strings"

const (
	NodeTypeObject     = "object"
	NodeTypeCollection = "collection"
	NodeTypeReference  = "reference"
)

const (
	FieldSourcePayload = "payload"
	FieldSourceTable   = "table"
)

// Deprecated: Use ConceptMemQLBackendUser instead.
const ConceptUser = ConceptMemQLBackendUser

var reservedPayloadFields = []string{
	"id",
	"createdAt",
	"createdBy",
	"concept",
	"partition",
	"payload",
	"schema",
	"type",
}

var reservedPayloadFieldSet = buildReservedPayloadFieldSet()

func IsReservedPayloadField(name string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(name))
	if trimmed == "" {
		return false
	}
	_, exists := reservedPayloadFieldSet[trimmed]
	return exists
}

func buildReservedPayloadFieldSet() map[string]struct{} {
	set := make(map[string]struct{}, len(reservedPayloadFields))
	for _, name := range reservedPayloadFields {
		if trimmed := strings.ToLower(strings.TrimSpace(name)); trimmed != "" {
			set[trimmed] = struct{}{}
		}
	}
	return set
}
