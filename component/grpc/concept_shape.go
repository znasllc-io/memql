package memql

import (
	"encoding/json"
	"sort"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// concept_shape.go -- projecting a concept's DECLARED SHAPE onto the wire
// (epic memql#4661, task memql#4662).
//
// ===========================================================================
// WHY THIS EXISTS AT ALL
// ===========================================================================
// ConceptInfo carried identity, prose and the display card. It did not carry
// what a row of the concept HOLDS, so every client that needed the shape
// derived it from a sample of rows -- which answers a different question and
// gets three things wrong that matter for rendering:
//
//   * a declared field that no loaded row carries is INVISIBLE, so a view
//     composed over page one of a walk cannot offer a column that exists;
//   * an enum is indistinguishable from free text until enough distinct
//     values happen to show up in the sample;
//   * a relationship cannot be seen AT ALL. A foreign key is a string like
//     any other string, so "this field points at an agent" is not a fact the
//     rows contain.
//
// The projection below is the answer, and it is deliberately a PROJECTION
// rather than a dump: the stored schema is a full JSON-Schema document with
// constraint keywords, nested variant discriminators and x- extensions, none
// of which a renderer wants and all of which would make the registry payload
// several times its size.
//
// ===========================================================================
// KIND IS THE AUTHORING VOCABULARY, NOT THE JSON-SCHEMA ONE
// ===========================================================================
// The DSL author wrote `datetime` and `enum(...)`; the stored schema records
// those as `{"type":"string","format":"date-time"}` and
// `{"type":"string","enum":[...]}`. A client picking a cell renderer wants
// the word the author wrote, so the mapping back happens HERE, once, rather
// than in each of the SDKs and the portal. See conceptFieldKind.
//
// ===========================================================================
// ORDER IS SORTED, AND THAT IS A CHOICE
// ===========================================================================
// Declaration order is not recoverable: the schema is built as a Go map and
// encoding/json sorts map keys on the way out, so by the time a schema is
// stored its properties are already alphabetical. Sorting explicitly says so
// rather than depending on that, and gives the two wire paths (the one-shot
// list and the follow-mode delta) an order they cannot disagree about.

// conceptShape projects a concept's declared fields and relationships.
//
// Returns two empty slices for a concept with no stored definition schema
// rather than an error. A concept with no schema is a real state -- the
// registry holds entries whose definition never parsed into properties -- and
// an empty field list is the honest projection of it. The wire contract says
// empty means "no shape published", which is precisely true.
func conceptShape(c *memoryNodes.Concept) ([]*memqlv1.ConceptField, []*memqlv1.ConceptRelationship) {
	return conceptFields(c), conceptRelationships(c)
}

// conceptSchemaDocument is the slice of the stored JSON Schema this projection
// reads. Everything else in the document -- constraints, allOf discriminators,
// x- extensions -- is deliberately not read: a renderer does not want it and
// carrying it would multiply the registry payload.
type conceptSchemaDocument struct {
	Properties map[string]conceptSchemaProperty `json:"properties"`
	Required   []string                         `json:"required"`
}

type conceptSchemaProperty struct {
	Type        string            `json:"type"`
	Format      string            `json:"format"`
	Description string            `json:"description"`
	Enum        []json.RawMessage `json:"enum"`
	// OneOf is how an OPTIONAL datetime is stored: the date-time member plus
	// the two "unset" sentinels (empty string, null). Read so an optional
	// datetime projects as `datetime` rather than falling through to the
	// unknown-type case -- otherwise every nullable timestamp in the tree
	// would reach a client as an untyped field (memql#1629 explains why the
	// storage shape is what it is).
	OneOf []conceptSchemaProperty `json:"oneOf"`
}

func conceptFields(c *memoryNodes.Concept) []*memqlv1.ConceptField {
	if c == nil {
		return nil
	}
	raw, err := c.DefinitionSchema()
	if err != nil || len(raw) == 0 {
		return nil
	}
	var doc conceptSchemaDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	required := make(map[string]bool, len(doc.Required))
	for _, name := range doc.Required {
		required[name] = true
	}

	names := make([]string, 0, len(doc.Properties))
	for name := range doc.Properties {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]*memqlv1.ConceptField, 0, len(names))
	for _, name := range names {
		prop := doc.Properties[name]
		out = append(out, &memqlv1.ConceptField{
			Name:        name,
			Kind:        conceptFieldKind(prop),
			Required:    required[name],
			EnumValues:  conceptEnumValues(prop),
			Description: prop.Description,
		})
	}
	return out
}

// conceptFieldKind maps a stored property back onto the word its author wrote.
//
// The two non-obvious cases are the ones the DSL stores structurally rather
// than nominally:
//
//	datetime -> {"type":"string","format":"date-time"}   (required)
//	         -> {"oneOf":[date-time, maxLength:0, null]} (optional)
//	enum     -> {"type":"string","enum":[...]}
//
// Anything unrecognised projects as its JSON-Schema type, and a property with
// no type at all projects as "object" -- the most permissive answer, which is
// what an unconstrained block is.
func conceptFieldKind(prop conceptSchemaProperty) string {
	if len(prop.Enum) > 0 {
		return "enum"
	}
	if prop.Format == "date-time" {
		return "datetime"
	}
	for _, member := range prop.OneOf {
		if member.Format == "date-time" {
			return "datetime"
		}
	}
	switch prop.Type {
	case "string", "boolean", "integer", "number", "array", "object":
		return prop.Type
	case "":
		// A bare `oneOf` with no top-level type: a nullable something. Fall
		// back to the first member that names a type, so a nullable string
		// still reads as a string rather than as an object.
		for _, member := range prop.OneOf {
			if member.Type != "" && member.Type != "null" {
				return member.Type
			}
		}
		return "object"
	default:
		return prop.Type
	}
}

// conceptEnumValues renders the declared members as strings.
//
// The stored members are `[]any` in JSON -- the DSL's `@enum` takes string
// literals, so they are strings in practice, but the schema does not promise
// it. A non-string member is SKIPPED rather than stringified: a client shows
// these as pill labels, and rendering `true` as the word "true" beside three
// real enum values would read as a fourth member that does not exist.
func conceptEnumValues(prop conceptSchemaProperty) []string {
	if len(prop.Enum) == 0 {
		return nil
	}
	out := make([]string, 0, len(prop.Enum))
	for _, raw := range prop.Enum {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			continue
		}
		out = append(out, s)
	}
	return out
}

// conceptRelationships projects the declarations verbatim, both axes.
//
// No normalisation and no defaulting: `as` is empty on every declaration that
// predates memql#3652, and filling it in from `type` here would hand a client
// the engine's structural word ("references") as if it were the domain's
// label. A client labelling an edge falls back to `field`, which is at least
// the author's own noun.
func conceptRelationships(c *memoryNodes.Concept) []*memqlv1.ConceptRelationship {
	if c == nil || len(c.Relationships) == 0 {
		return nil
	}
	out := make([]*memqlv1.ConceptRelationship, 0, len(c.Relationships))
	for _, rel := range c.Relationships {
		out = append(out, &memqlv1.ConceptRelationship{
			Type:      rel.Type,
			As:        rel.As,
			Field:     rel.Field,
			Target:    rel.TargetConcept,
			Direction: rel.Direction,
		})
	}
	return out
}
