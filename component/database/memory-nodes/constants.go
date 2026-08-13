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

// reservedPayloadFields is every name a concept may NOT declare as a
// top-level payload property. It is one half of a two-halved contract with
// component/memql's reservedFilterHead, which lists the names that address an
// ENGINE NAMESPACE at the head of a filter path rather than the payload.
//
// The two must satisfy: reservedPayloadFields SUPERSET-OF reservedFilterHead.
// Pinned by TestReservedPayloadFieldsSupersetOfFilterHeads
// (component/memql/reserved_name_containment_test.go). It is derived from the
// filter side, so a head added there and not here fails.
//
// Why containment is the property, and not merely tidiness (memql#3613): the
// two lists disagreed by eight names, and every one of those eight was
// declarable. A concept could declare `provenance`, `actor`, `args`, `now`,
// `config`, `trace`, `meta` or `row` as a payload field; the concept
// registered intact, with no skip and no warning. Every filter naming it bare
// then read the engine namespace instead of the payload it was written for:
//
//	provenance.kind == args.v  ->  provenance->>'kind'   (the row's own column)
//	actor.userId    == args.v  ->  ? = ?                 (a query-time constant)
//
// `provenance` was fully silent -- the SQL push-down compiled clean and the
// in-process post-filter agreed on the WRONG field, so the query returned the
// wrong rows forever with nothing to indicate it. `actor` was silent AND
// authorization-relevant: `actor.userId == args.v`, the natural spelling of
// "rows whose actor is the caller", const-folds to true whenever the caller
// passes their own id, so the predicate contributes nothing and the query
// degenerates to every row of the concept.
var reservedPayloadFields = []string{
	// Columns of "MemoryNodes" -- a payload property of the same name would
	// be indistinguishable from the column it compiles against.
	// `partition` is the exception and is NOT a column: partitioning was
	// retired in #56, and the name is kept as a retired-name guard (see
	// docs/public/language/authoring-rules.md #12).
	"id",
	"createdAt",
	"createdBy",
	"concept",
	"partition",
	"payload",
	"schema",
	"type",
	// Engine namespaces the plan parser refuses to payload-prefix
	// (reservedFilterHead). `provenance` is both -- a real column AND the one
	// object-valued intrinsic, so it takes nested paths. Note `meta` is
	// reserved as a NAMESPACE and is not the `metadata` column, which is not
	// reserved at all.
	"provenance",
	"row",
	"actor",
	"args",
	"now",
	"config",
	"trace",
	"meta",
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
