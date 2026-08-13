package memql

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// relationshipAsLabelMaxLen caps the length of an `as` domain label. The cap
// exists so a label stays a name rather than becoming a sentence; it is not a
// semantic constraint.
const relationshipAsLabelMaxLen = 64

// relationshipAsLabelPattern is the FORM of an `as` domain label:
// lowerCamelCase, matching how every other author-facing identifier in the DSL
// is spelled.
var relationshipAsLabelPattern = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)

// validateRelationshipAsLabel checks the FORM of an `as` label and NOTHING
// ELSE (memql#3652).
//
// There is deliberately no membership check here, and there must never be one.
// `as` carries the DOMAIN meaning of an edge, and the set of domain verbs
// belongs to whoever authors the DSL -- including product repos that mount
// their own tree at MEMQL_DSL_PATH and cannot patch this engine. A closed
// vocabulary is exactly what made an unrecognised verb a boot refusal that took
// the mesh down twice (d45efaa2, 42aeff3b), each fix adding one word to a Go
// switch. Adding an allowed-values list here would rebuild that treadmill.
//
// An empty label is valid: `as` is optional on every relationship type.
func validateRelationshipAsLabel(as string) error {
	if as == "" {
		return nil
	}
	if len(as) > relationshipAsLabelMaxLen {
		return fmt.Errorf("as label %q is %d characters, over the %d limit", as, len(as), relationshipAsLabelMaxLen)
	}
	if !relationshipAsLabelPattern.MatchString(as) {
		return fmt.Errorf("as label %q must be lowerCamelCase (letters and digits, starting lowercase), e.g. as=\"assignedTo\"", as)
	}
	return nil
}

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

// structuralRelationshipTypeSet maps a NORMALIZED spelling (lowercased, with
// underscores stripped) to its canonical type. It is the single source of truth
// for the closed structural set: both canonicalRelationshipType and
// structuralRelationshipTypes derive from it, so the set the engine accepts and
// the set an error message advertises cannot drift apart.
//
// The old form was a switch plus, in principle, a second hand-maintained list
// for messages -- and a message that lists types the engine no longer accepts
// is worse than no message, because it sends the author down a dead end while
// looking authoritative.
var structuralRelationshipTypeSet = map[string]string{
	"parent":        relationshipTypeParent,
	"alias":         relationshipTypeAlias,
	"equals":        relationshipTypeEquals,
	"interactswith": relationshipTypeInteracts,
	"contains":      relationshipTypeContains,
	"owns":          relationshipTypeOwns,
	"createdby":     relationshipTypeCreatedBy,
	"dependson":     relationshipTypeDependsOn,
	"formedfrom":    relationshipTypeFormedFrom,
}

func canonicalRelationshipType(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", false
	}

	normalized := strings.ToLower(strings.ReplaceAll(trimmed, "_", ""))
	canonical, ok := structuralRelationshipTypeSet[normalized]
	return canonical, ok
}

// structuralRelationshipTypes returns the canonical structural types in a
// stable order, for error messages and tooling.
func structuralRelationshipTypes() []string {
	out := make([]string, 0, len(structuralRelationshipTypeSet))
	for _, canonical := range structuralRelationshipTypeSet {
		out = append(out, canonical)
	}
	sort.Strings(out)
	return out
}
