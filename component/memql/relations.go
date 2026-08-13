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

// A relationship's direction says which side of the edge carries the pointer
// field: outgoing means the declaring row's field holds the target id, incoming
// means the target's field holds the declaring row's id.
//
// There is deliberately no third value. `bidirectional` was one until
// memql#3668, and it was a category error rather than an unimplemented feature
// -- an edge carried on both sides is two relationships, one declared from each
// concept. Nothing implemented it coherently, and both id canonicalizers
// skipped such a field outright, persisting non-canonical ids that
// (concept, id) lookups then quietly failed to find.
const (
	relationshipDirectionOutgoing = "outgoing"
	relationshipDirectionIncoming = "incoming"
)

const (
	relationshipTypeParent     = "parent"
	relationshipTypeAlias      = "alias"
	relationshipTypeEquals     = "equals"
	relationshipTypeReferences = "references"
	relationshipTypeContains   = "contains"
	relationshipTypeOwns       = "owns"
	relationshipTypeCreatedBy  = "createdBy"
	// `dependsOn` and `formedFrom` used to live here as structural types. They
	// never were structural: their own comments recorded that graph-expansion
	// traversal was deliberately unwired and the field was read directly by the
	// controller, so the engine did nothing with either. They existed only
	// because, at the time, there was no other way to say the word -- and each
	// cost an engine release after a boot refusal took the cluster down
	// (42aeff3b, d45efaa2).
	//
	// They are now `as` domain labels (memql#3655), which is the axis they
	// always belonged on:
	//
	//   @relationship(type="references", as="dependsOn",  field="dependsOn", ...)
	//   @relationship(type="references", as="formedFrom", field="sourceEpisodes", ...)
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
	"parent":     relationshipTypeParent,
	"alias":      relationshipTypeAlias,
	"equals":     relationshipTypeEquals,
	"references": relationshipTypeReferences,
	"contains":   relationshipTypeContains,
	"owns":       relationshipTypeOwns,
	"createdby":  relationshipTypeCreatedBy,
}

// renamedRelationshipTypes maps a NORMALIZED retired spelling to the structural
// type it became, so the loader can tell an author the word MOVED rather than
// only that it is invalid (memql#3663).
//
// This is a MESSAGE table, not a compatibility shim: nothing here is accepted at
// load. The distinction matters -- silently normalizing the old spelling would
// leave two spellings for one concept, which is the drift memql#3657 exists to
// remove, and which CLAUDE.md forbids pre-release.
var renamedRelationshipTypes = map[string]string{
	"interactswith": relationshipTypeReferences,
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
