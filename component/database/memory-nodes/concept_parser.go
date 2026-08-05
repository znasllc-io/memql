// Package memoryNodes -- concept_parser.go
//
// Parses a concept.memql file into a *Concept. As of Phase 2 of the
// language-improvements plan, the grammar parsing happens in the
// shared component/language/parser; this file holds only the bridge
// from the shared AST (parser.ConceptDecl) to the memory-nodes
// Concept type, plus the JSON-Schema builder that emits draft-07
// schemas from the parsed shape.
//
// The legacy hand-rolled parser (~500 LOC of bespoke lexing) used to
// live here; it was retired in Phase 2 once parser.parseConceptDecl
// landed.
package memoryNodes

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/parser"
)

// ParseConceptMemQL parses a concept.memql file into a *Concept using
// the shared language parser. dirPath is used to derive the fully-
// qualified concept name (e.g. "concepts/v1/identity/user" ->
// "v1:identity:user").
//
// This is the LEGACY entry point: it expects the path-derived name
// convention from the original kind-first tree. The unified loader
// (see component/memql/unified_loader.go) calls BuildConceptFromDecl
// directly with a name assembled from the structural @version +
// @namespace attributes.
func ParseConceptMemQL(content []byte, dirPath string) (*Concept, error) {
	conceptName, err := deriveConceptName(dirPath)
	if err != nil {
		return nil, fmt.Errorf("derive concept name: %w", err)
	}

	file, err := parser.ParseFile(string(content))
	if err != nil {
		return nil, fmt.Errorf("parse concept.memql for %s: %w", conceptName, err)
	}

	var decl *parser.ConceptDecl
	for _, def := range file.Definitions {
		if cd, ok := def.(*parser.ConceptDecl); ok {
			decl = cd
			break
		}
	}
	if decl == nil {
		return nil, fmt.Errorf("concept.memql for %s contains no concept declaration", conceptName)
	}

	return BuildConceptFromDecl(decl, conceptName)
}

// BuildConceptFromDecl builds a *Concept from an already-parsed
// ConceptDecl + an externally-derived concept name. This is the
// shared core both ParseConceptMemQL and the unified loader call;
// the difference is only in how the name was computed.
//
// Used by the unified loader to register concepts found in the
// new domain-first tree where the file's path no longer encodes
// the concept name (the name comes from @version + @namespace +
// declaration name, assembled via languageAst.AssembleConceptIdFromDecl).
func BuildConceptFromDecl(decl *parser.ConceptDecl, conceptName string) (*Concept, error) {
	parsed, err := conceptDeclToParsed(decl)
	if err != nil {
		return nil, fmt.Errorf("translate concept %s: %w", conceptName, err)
	}

	schema, err := buildJSONSchema(conceptName, parsed)
	if err != nil {
		return nil, fmt.Errorf("build schema for %s: %w", conceptName, err)
	}

	schemaBytes, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("marshal schema for %s: %w", conceptName, err)
	}

	if err := ensureReservedFieldsNotDeclared(conceptName, schemaBytes); err != nil {
		return nil, err
	}

	schemas := map[string]json.RawMessage{
		"definition": json.RawMessage(schemaBytes),
	}

	nodeType := parsed.conceptType
	if nodeType == "" {
		nodeType = NodeTypeObject
	}

	// Late-pass validation: now that the full property set is
	// available, verify any @displayCard slot references point at a
	// real, displayable field. Done here (not inside
	// applyConceptAttribute) because attributes are folded before
	// properties on the conceptDeclToParsed path.
	if parsed.displayCard != nil {
		if err := validateDisplayCard(conceptName, parsed.displayCard, parsed.properties); err != nil {
			return nil, err
		}
	}

	// Same late pass, same reason: `@rowAuthz(owner="<field>")` names
	// a payload field, and the property set only exists now.
	if parsed.rowAuthz != nil {
		if err := validateRowAuthz(conceptName, parsed.rowAuthz, parsed.properties); err != nil {
			return nil, err
		}
	}

	version := parsed.version
	if version == "" {
		// Absent @version means 1.0.0 (#2613), stored in the same
		// "v<major>" prefix form an explicit annotation lands as, so
		// defaulted and annotated concepts report identical metadata.
		// Only a genuinely unannotated flat-tree concept lands here.
		version = "v1"
	}
	return &Concept{
		Name:          conceptName,
		SchemaId:      conceptName,
		Schemas:       schemas,
		NodeType:      nodeType,
		Description:   parsed.description,
		Version:       version,
		Relationships: parsed.relationships,
		DisplayCard:   parsed.displayCard,
		RowAuthz:      parsed.rowAuthz,
	}, nil
}

// validateRowAuthz checks the half of a row-authz declaration that can
// only be checked once the property set is known: an `owner="<field>"`
// tier must name a field the concept actually declares.
//
// The other tiers need no late pass. `public` and `clusterOwner` take
// no parameter, and `via="<spec>"` names a spec in another file, which
// this package cannot see -- resolving that is Phase 2's problem,
// where the predicate is actually computed. Declaring it here without
// resolving it is deliberate: the vocabulary has to be able to EXPRESS
// the granted tier before anything can measure it.
//
// See memql#2920.
func validateRowAuthz(conceptName string, decl *parser.RowAuthzDecl, props []parsedProperty) error {
	if decl == nil || decl.Tier != parser.RowAuthzOwned {
		return nil
	}
	// The SELF-OWNED form (memql#3029, ruled in memql#2803). A concept whose
	// owner is the row's own identity has no payload field to name -- a user's
	// owner IS the row -- so `id` is admitted without requiring a declared
	// property. Without it `v1:identity:user`, the concept carrying the
	// cluster-wide `role`, could not declare a tier at all and stayed outside
	// memql#2982's owner-field provenance gate.
	//
	// `id` and ONLY `id`. Every other row intrinsic means something else, and
	// `createdBy` is the dangerous near-miss: it means "who WROTE the row",
	// not "whose row it is". A row an admin creates on a user's behalf has a
	// createdBy that is not the owner, so admitting it would let a concept
	// declare an owner tier that is FALSE -- precisely the class of false
	// declaration #2982 exists to catch. The others need no special handling:
	// no concept can declare an intrinsic as a property, so they fall through
	// to the loop below and are rejected by it.
	if decl.Owner == parser.RowAuthzSelfOwnedField {
		return nil
	}
	for _, p := range props {
		if p.name == decl.Owner {
			return nil
		}
	}
	declared := make([]string, 0, len(props))
	for _, p := range props {
		declared = append(declared, p.name)
	}
	sort.Strings(declared)
	// The self-owned escape is named here because this is the diagnostic an
	// author actually hits when a concept has no field to name. Documenting it
	// only in the reference leaves them stuck at the one moment it would help.
	return fmt.Errorf("@%s(owner=%q) on concept %q references a field the concept does not "+
		"declare (declared: %s). If this concept is SELF-OWNED -- the row IS the owner, as it "+
		"is for a user -- declare owner=%q instead",
		parser.RowAuthzAnnotation, decl.Owner, conceptName, strings.Join(declared, ", "),
		parser.RowAuthzSelfOwnedField)
}

// validateDisplayCard checks that every slot in the parsed card
// names a real top-level property on the concept and that the
// referenced property has a displayable type (string / enum / bool /
// datetime / int / float / map -- everything except nested object
// and array). Returns nil when every slot validates.
//
// Slots are optional except primary; an empty value on
// secondary/tertiary/status means "skip this slot at render time."
// See memql#160.
func validateDisplayCard(conceptName string, card *DisplayCard, props []parsedProperty) error {
	if card == nil {
		return nil
	}
	byName := make(map[string]parsedProperty, len(props))
	for _, p := range props {
		byName[p.name] = p
	}
	check := func(slot, fieldName string) error {
		fieldName = strings.TrimSpace(fieldName)
		if fieldName == "" {
			return nil
		}
		// Row intrinsics (createdAt, createdBy, id, ...) are real,
		// always-present row columns that a card may render, but they
		// can NOT be declared as concept properties (the loader rejects
		// reserved fields), so they never appear in byName. Accept the
		// displayable ones directly -- this is the only way a card can
		// surface e.g. tertiary="createdAt". See memql#771.
		if t, ok := displayableIntrinsicType(fieldName); ok {
			if !isDisplayableType(t) {
				return fmt.Errorf("@displayCard on concept %q: %s=%q references intrinsic of type %q which is not displayable", conceptName, slot, fieldName, t)
			}
			return nil
		}
		prop, ok := byName[fieldName]
		if !ok {
			return fmt.Errorf("@displayCard on concept %q: %s=%q references unknown field (must match a top-level concept property or a displayable row intrinsic)", conceptName, slot, fieldName)
		}
		if !isDisplayableType(prop.typeName) {
			return fmt.Errorf("@displayCard on concept %q: %s=%q references field of type %q which is not displayable (allowed: string, enum, bool, datetime, int, float)", conceptName, slot, fieldName, prop.typeName)
		}
		return nil
	}
	if err := check("primary", card.Primary); err != nil {
		return err
	}
	if err := check("secondary", card.Secondary); err != nil {
		return err
	}
	if err := check("tertiary", card.Tertiary); err != nil {
		return err
	}
	if err := check("status", card.Status); err != nil {
		return err
	}
	return nil
}

// displayableIntrinsicType returns the implicit type of a row
// intrinsic that a displayCard slot may reference, and whether the
// name is such an intrinsic. Intrinsics are the always-present row
// columns (id, createdAt, createdBy, concept, type) that can't be
// declared as concept properties yet are legitimately renderable in a
// card. The non-scalar intrinsics (payload, schema) and the
// isolation-only partition column are intentionally excluded -- they
// don't reduce to a sensible single cell. See memql#771.
func displayableIntrinsicType(name string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "createdat":
		return "datetime", true
	case "id", "createdby", "concept", "type":
		return "string", true
	}
	return "", false
}

// isDisplayableType reports whether a property type can land in a
// displayCard slot. Object / array / map types are rejected because
// they don't reduce to a single sensible cell in a row chrome.
func isDisplayableType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "string", "enum", "bool", "boolean", "datetime", "int", "integer", "float", "number":
		return true
	}
	return false
}

// parsedConcept is the intermediate representation feeding into the
// JSON-Schema builder. Kept around so the builder doesn't need to know
// about the shared AST's ConceptDecl shape.
type parsedConcept struct {
	description   string
	conceptType   string
	version       string // "v<major>" prefix form from @version; empty means the 1.0.0 default (#2613)
	properties    []parsedProperty
	required      []string
	relationships []RelationshipDefinition
	noAdditional  bool // default true for concept.memql

	// displayCard is the optional rendering-hint set declared via
	// `@displayCard(...)`. Validated against the property list AFTER
	// the property pass (named fields must exist + be displayable).
	// See memql#160.
	displayCard *DisplayCard

	// rowAuthz is the optional row-authorization tier declared via
	// `@rowAuthz(...)`. Like displayCard, the `owner="<field>"` form
	// is validated against the property list AFTER the property pass,
	// because attributes are folded before properties on this path.
	// Nil means the concept declared no tier -- a warning in Phase 1
	// and nothing more. See memql#2920.
	rowAuthz *parser.RowAuthzDecl
}

// parsedProperty mirrors the legacy internal type so the JSON-Schema
// builder can stay unchanged. Phase 3 adds fields for the expanded
// property annotation vocabulary (@unique, @pattern, @minLength,
// @maxLength, @minimum, @maximum, @immutable, @secret) and for
// discriminated-union variants (@variant).
type parsedProperty struct {
	name         string
	typeName     string
	description  string
	defaultValue any
	required     bool
	enumValues   []string
	// element is the lowered element type of a WRAPPED property -- the item
	// type of []T and the value type of map[string]T. Recursive, so
	// [][]string and map[string]map[string]int lower all the way down.
	//
	// It replaces the old `arrayItemType string`, which kept only the
	// element's outermost Kind (memql#2951). That flattening threw away
	// every composite element's inner type, and because the builder lowered
	// the surviving string through a switch ending in `default: "string"`,
	// an element type the vocabulary does not know became `string` in
	// silence -- `[]boolean` built a field validating as `[]string`.
	//
	// The element is a parsedProperty rather than a bare type so it can
	// carry the parts of the declaration that describe the VALUE (@pattern,
	// @minLength, a nested block, @variant branches). Those are moved down
	// onto it in propertyDeclToParsed; field MARKERS stay on the wrapper.
	element *parsedProperty
	nested  []parsedProperty
	format  string
	// Phase 3 constraints
	unique    bool
	pattern   string
	minLength *int64
	maxLength *int64
	minimum   *float64
	maximum   *float64
	immutable bool
	secret    bool
	// pii marks the field as personally-identifying data. Surfaces as
	// the x-pii custom JSON-Schema keyword so the engine's hard-delete
	// scrub (memql#1711) can enumerate and zero every such field
	// generically instead of relying on a hand-maintained list.
	pii bool
	// internal marks the field as server-only (memql#2035, concept as
	// single source of truth). An @internal field is NEVER auto-exposed
	// in a shape's default projection and NEVER accepted from a
	// mutation's caller args. Surfaces as the x-internal custom
	// JSON-Schema keyword.
	internal bool
	// serverSet marks the field as stamped server-side (createdAt,
	// createdBy, status, ...). A @serverSet field is NOT accepted from a
	// mutation's caller args (it must be stamped) but MAY be projected.
	// Surfaces as the x-serverSet custom JSON-Schema keyword.
	serverSet bool
	// Phase 3 discriminated-union variants
	variantDiscriminator string
	variants             []parsedPropertyVariant
}

// parsedPropertyVariant captures one branch of a @variant block.
type parsedPropertyVariant struct {
	name       string
	properties []parsedProperty
}

// conceptDeclToParsed bridges the shared AST ConceptDecl into the
// intermediate form the JSON-Schema builder expects.
func conceptDeclToParsed(decl *parser.ConceptDecl) (*parsedConcept, error) {
	out := &parsedConcept{noAdditional: true}

	for _, attr := range decl.Attributes {
		if err := applyConceptAttribute(out, attr); err != nil {
			return nil, err
		}
	}
	// Ruling-3 precedence resolved ONCE here (memql#2634) so the Concept
	// literal and buildJSONSchema's concept-level description agree.
	out.description = parser.EffectiveDescription(decl.DocComment, out.description)

	for _, prop := range decl.Properties {
		pp, err := propertyDeclToParsed(prop)
		if err != nil {
			return nil, fmt.Errorf("property %q: %w", prop.Name, err)
		}
		if pp.required {
			out.required = append(out.required, pp.name)
		}
		out.properties = append(out.properties, pp)
	}

	for _, rel := range decl.Relationships {
		out.relationships = append(out.relationships, RelationshipDefinition{
			Type:          rel.Type,
			Field:         rel.Field,
			FieldSource:   rel.FieldSource,
			TargetConcept: rel.Target,
			Direction:     rel.Direction,
		})
	}

	return out, nil
}

// propertyDeclToParsed converts a shared-AST PropertyDecl into the
// intermediate parsedProperty. Handles primitives, enums, arrays, and
// nested object blocks.
func propertyDeclToParsed(prop *parser.PropertyDecl) (parsedProperty, error) {
	out := parsedProperty{name: prop.Name}

	if prop.Type != nil {
		out.typeName = prop.Type.Kind
		out.format = prop.Type.Format
		if prop.Type.Kind == "enum" {
			out.enumValues = append(out.enumValues, prop.Type.EnumValues...)
		}
	}

	for _, nested := range prop.Nested {
		pp, err := propertyDeclToParsed(nested)
		if err != nil {
			return parsedProperty{}, fmt.Errorf("nested property %q: %w", nested.Name, err)
		}
		out.nested = append(out.nested, pp)
	}

	for _, attr := range prop.Attributes {
		if err := applyPropertyAttribute(&out, attr); err != nil {
			return parsedProperty{}, err
		}
	}

	// Fold variant branches in. The discriminator comes from the
	// @variant(discriminator="fieldName") attribute that the parser
	// collected earlier; we extract it here so the schema builder
	// has everything in one place.
	if len(prop.Variants) > 0 {
		for _, attr := range prop.Attributes {
			if attr != nil && attr.Name == "variant" {
				if d, ok := attr.Args["discriminator"].(string); ok {
					out.variantDiscriminator = d
				}
			}
		}
		for _, v := range prop.Variants {
			variant := parsedPropertyVariant{name: v.Name}
			for _, nested := range v.Properties {
				pp, err := propertyDeclToParsed(nested)
				if err != nil {
					return parsedProperty{}, fmt.Errorf("variant %q property %q: %w", v.Name, nested.Name, err)
				}
				variant.properties = append(variant.properties, pp)
			}
			out.variants = append(out.variants, variant)
		}
	}

	// A WRAPPED property lowers its element from the full element TypeRef,
	// and the parts of the declaration that describe the VALUE move down onto
	// that element (memql#2951).
	//
	// The split is what the annotation is ABOUT. @pattern, @minLength/
	// @maxLength, @minimum/@maximum, a nested block and @variant branches all
	// describe the value, and JSON Schema ignores every one of them on an
	// array -- `[]string @pattern` emitted `pattern` on the array, where it
	// constrains nothing, and `[]object @variant` dropped the discriminated
	// union outright. On the element they mean what the author wrote: each
	// item matches the pattern, each item is one of the variants.
	//
	// Field MARKERS (@required and the `!` sigil, @description, @default,
	// @unique, @immutable, @secret, @pii, @internal, @serverSet) describe the
	// FIELD, so the wrapper is already the right place for them and they stay
	// put -- `capabilities []string!` keeps behaving exactly as it does today.
	if out.typeName == "array" || out.typeName == "map" {
		var itemRef *parser.TypeRef
		if prop.Type != nil {
			itemRef = prop.Type.ArrayItem
		}
		elem := elementFromTypeRef(itemRef)

		// The move is ONE level, so it only means something when the element
		// can carry the constraint. On a COMPOSITE element -- [][]T,
		// []map[string]T, map[string][]T -- it lands on the inner array or map,
		// where JSON Schema ignores `pattern`/`minimum`/... outright and where
		// propertyToJSONSchema's array and map branches never read `variants` at
		// all, so a discriminated union is discarded rather than merely
		// misplaced. Every one of those built a schema that CONTRADICTS the
		// declaration in silence: `[][]string @pattern("^[a-z]+$")` accepted
		// [["ZZZ"]], `[][]object @variant(...)` accepted [[{"nonsense":1}]].
		//
		// memql#2951's definition of done required a constraining annotation to
		// apply to elements OR be rejected when the field is wrapped. For a
		// composite element the code did NEITHER, which is the silent-wrong
		// option that issue exists to eliminate (memql#3049).
		//
		// REJECTED rather than recursed to the leaf, which was the other option
		// on the table. `[][]string @pattern` is genuinely ambiguous -- each
		// string, or each inner array? -- so recursing would pick one reading
		// and silently impose it, trading a silent no-op for a silent guess. No
		// construct in the tree or in the product bundle carries this shape, so
		// there is no caller whose intent recursion would be honouring. An
		// author who means the leaf can say so once the grammar has a spelling
		// for it; until then the declaration is refused with the reason.
		if elem.typeName == "array" || elem.typeName == "map" {
			if names := valueAnnotationNames(&out); len(names) > 0 {
				return parsedProperty{}, fmt.Errorf(
					"%s cannot be declared on `%s`: a value constraint applies to the IMMEDIATE "+
						"element, and that element is itself %s, where JSON Schema ignores it "+
						"(@variant is dropped entirely) -- the field would validate as though the "+
						"annotation were absent. Single-wrap the field instead (`[]string "+
						"@pattern(\"…\")`, `[]object @variant(…)`), or move the constraint into a "+
						"named object property. No ANNOTATION reaches the leaf of a composite "+
						"element; a leaf TYPE does, so `[][]enum(\"a\",\"b\")` and `[][]datetime` "+
						"already constrain each leaf value (memql#3049)",
					strings.Join(names, " and "),
					renderTypeRef(prop.Type),
					anArrayOrMap(elem.typeName),
				)
			}
		}

		elem.pattern, out.pattern = out.pattern, ""
		elem.minLength, out.minLength = out.minLength, nil
		elem.maxLength, out.maxLength = out.maxLength, nil
		elem.minimum, out.minimum = out.minimum, nil
		elem.maximum, out.maximum = out.maximum, nil
		elem.nested, out.nested = out.nested, nil
		elem.variants, out.variants = out.variants, nil
		elem.variantDiscriminator, out.variantDiscriminator = out.variantDiscriminator, ""

		out.element = elem
	}

	return out, nil
}

// valueAnnotationNames lists the value-describing parts of a declaration that
// are actually set on prop, spelled as an author wrote them, in the order the
// annotation table in docs/public/language/memql.md lists them.
//
// The set is exactly the "value constraints" row of that table -- the parts
// that move down onto the element in propertyDeclToParsed. Field MARKERS
// (@required, @description, @default, @unique, @immutable, @secret, @pii,
// @internal, @serverSet) are deliberately absent: they describe the FIELD, stay
// on the wrapper, and are unaffected by how deeply the element is wrapped, so
// `capabilities [][]string!` is still perfectly fine.
//
// A nested block rides the same move as @variant (both populate the element's
// object shape) and is listed for the same reason, even though the grammar has
// no way to attach one to a wrapped type today -- if that changes, it is
// already covered rather than newly silent.
func valueAnnotationNames(prop *parsedProperty) []string {
	var names []string
	if prop.pattern != "" {
		names = append(names, "@pattern")
	}
	if prop.minLength != nil {
		names = append(names, "@minLength")
	}
	if prop.maxLength != nil {
		names = append(names, "@maxLength")
	}
	if prop.minimum != nil {
		names = append(names, "@minimum")
	}
	if prop.maximum != nil {
		names = append(names, "@maximum")
	}
	if len(prop.variants) > 0 {
		names = append(names, "@variant")
	}
	if len(prop.nested) > 0 {
		names = append(names, "a nested block")
	}
	return names
}

// anArrayOrMap spells an element kind for the diagnostic, so the message reads
// as a sentence rather than as a field dump.
func anArrayOrMap(kind string) string {
	if kind == "map" {
		return "a map"
	}
	return "an array"
}

// renderTypeRef spells a TypeRef the way an author writes it, so a diagnostic
// can quote the declaration back rather than describing it. Only the wrapper
// kinds recurse; everything else is already its authored spelling.
//
// The nil arm is defensive, not a live case: the parser never yields a nil
// TypeRef here. Bare `array` is given `ArrayItem: &TypeRef{Kind: "string"}` at
// parser.go:2728, and the sole external call site sits inside a branch that
// already dereferenced prop.Type. It renders "string" so a future nil cannot
// produce an empty declaration in a diagnostic.
func renderTypeRef(ref *parser.TypeRef) string {
	if ref == nil {
		return "string"
	}
	switch ref.Kind {
	case "array":
		return "[]" + renderTypeRef(ref.ArrayItem)
	case "map":
		return "map[string]" + renderTypeRef(ref.ArrayItem)
	default:
		return ref.Kind
	}
}

// elementFromTypeRef lowers an element TypeRef into the parsedProperty the
// schema builder consumes, recursing so a composite element ([][]string,
// map[string]map[string]int) keeps its inner type instead of collapsing to its
// outermost kind.
//
// The element is marked `required` because an element is always PRESENT: there
// is no such thing as an unset entry in an array, so the empty-string / null
// sentinels an optional scalar `datetime` has to accept (memql#1629) do not
// apply one level in.
//
// That gives the element the plain `{type: string, format: date-time}` shape
// rather than the sentinel `oneOf`, and the reason that matters is NOT format
// enforcement. `format` is annotation-only here: the concept-schema compiler
// leaves Draft2019's default alone and never sets AssertFormat (see
// concept_schema.go), and nothing in this repo sets it -- so `[]datetime`
// validates byte-equivalently to `[]string` at runtime. An earlier version of
// this comment claimed the opposite; corrected in the memql#2951 review.
//
// What `required` actually buys is soundness of the sentinel union: the
// optional-scalar `oneOf` carries both `{type: string, format: date-time}` and
// `{type: string, maxLength: 0}`, and with format unasserted BOTH match `""` --
// so `oneOf`'s exactly-one rule would REJECT an empty-string entry. Marking the
// element required routes around that. (That double-match looks like a live
// defect for optional SCALAR datetime fields too; it predates this change and
// is tracked separately.)
//
// A nil ref is the legacy bare `array` form, whose parser default is `string`.
func elementFromTypeRef(ref *parser.TypeRef) *parsedProperty {
	if ref == nil {
		return &parsedProperty{typeName: "string", required: true}
	}
	elem := &parsedProperty{typeName: ref.Kind, format: ref.Format, required: true}
	if ref.Kind == "enum" {
		elem.enumValues = append(elem.enumValues, ref.EnumValues...)
	}
	if ref.Kind == "array" || ref.Kind == "map" {
		elem.element = elementFromTypeRef(ref.ArrayItem)
	}
	return elem
}

// applyConceptAttribute folds an @annotation into the intermediate
// concept representation.
func applyConceptAttribute(c *parsedConcept, attr *parser.Attribute) error {
	if attr == nil {
		return nil
	}
	switch attr.Name {
	case "description":
		c.description = attrString(attr)
	case "type":
		c.conceptType = strings.ToLower(attrString(attr))
	case "scope":
		// `@scope` was retired in #56 (partition removal). Every
		// concept lives in one partition; the per-concept scope
		// distinction is gone. Reject explicitly so stale concept
		// files surface a clear error instead of silently parsing
		// to the post-removal default.
		return fmt.Errorf("`@scope` is retired -- remove the annotation; every concept lives in the default partition post-#56")
	// @visibility was removed in the genesis simplification. Every
	// binary now loads every concept; functional specialization
	// happens at the build-tag layer (which integrations are active),
	// not the DSL layer. The parser no longer recognizes @visibility;
	// the strip-from-files migration removed it from every .memql.
	case "version":
		// @version("MAJOR.MINOR.PATCH") -- strict semver, metadata
		// only (canonical ids are PARTITION-prefixed -- v1: is the
		// partition, not this version; the older id-prefix claim here
		// was stale, #2613). Absent means 1.0.0 (the lifecycle-epoch
		// default); an explicit annotation wins and marks a genuine
		// non-default.
		s := strings.TrimSpace(attrString(attr))
		if s == "" {
			return fmt.Errorf("@version requires a semver string like \"1.0.0\"")
		}
		// Validate via the AST helper so the parser fails fast on
		// malformed values; the prefix stored here is "v<major>".
		v, err := languageAst.ParseSemver(s)
		if err != nil {
			return err
		}
		c.version = fmt.Sprintf("v%d", v.Major)
	case "namespace":
		// @namespace("foo") or @namespace("foo:bar:baz") -- colon-
		// separated lowercase identifiers. Validated against the
		// shared namespace pattern; the engine assembles the
		// canonical ID via ast.AssembleConceptId during registration.
		// The value is currently stored implicitly (path-derived
		// during the transition), so we only validate here.
		ns := strings.TrimSpace(attrString(attr))
		if ns == "" {
			return fmt.Errorf("@namespace requires a string value")
		}
		if _, _, err := languageAst.ExtractNamespaceAttribute([]*parser.Attribute{attr}); err != nil {
			return err
		}
	case "displayCard":
		// @displayCard(primary="name", secondary="role", tertiary="ownerUserId", status="active")
		//   -- per-concept rendering hints for concept-agnostic
		//   clients (the cockpit's Concepts tab, future browsers).
		//   Field-existence + type-compatibility checks run in
		//   BuildConceptFromDecl AFTER the property pass, because
		//   attributes are folded before properties on this code
		//   path. See memql#160.
		card, err := parseDisplayCardAttr(attr)
		if err != nil {
			return err
		}
		c.displayCard = card
	case parser.RowAuthzAnnotation:
		// @rowAuthz(public) / @rowAuthz(clusterOwner) /
		// @rowAuthz(owner="<field>") / @rowAuthz(via="<spec>")
		//   -- the declared row-authorization tier (memql#2920).
		//   PHASE 1 IS INERT: the tier is parsed, validated and
		//   carried on the Concept, and nothing reads it at query
		//   time. The `owner=` field-existence check runs in
		//   BuildConceptFromDecl AFTER the property pass, for the
		//   same reason @displayCard's does.
		//
		//   The declaration is read through parser.ParseRowAuthz --
		//   the SAME function the memqlmigrate codemod renders
		//   against -- so the loader and the codemod cannot disagree
		//   about what a declaration means (#2621's lesson).
		//
		//   A SECOND @rowAuthz is a load error, not a silent
		//   overwrite. The parser folds attributes in source order, so
		//   without this a concept carrying two declarations loaded
		//   with the LAST one winning -- a reader scanning top-down
		//   sees a tier the engine does not use. "One tier per
		//   concept" has to be enforced for the declaration to mean
		//   anything.
		if c.rowAuthz != nil {
			return fmt.Errorf("@%s declared more than once -- a concept declares exactly one tier",
				parser.RowAuthzAnnotation)
		}
		decl, err := parser.ParseRowAuthz(attr)
		if err != nil {
			return err
		}
		c.rowAuthz = decl
	default:
		return fmt.Errorf("unknown concept annotation @%s", attr.Name)
	}
	return nil
}

// parseDisplayCardAttr extracts the named args from
// @displayCard(primary=..., secondary=..., tertiary=..., status=...).
// Validates that `primary` is non-empty and that no unrecognised
// argument name was supplied; field-existence checks happen later.
// Reference: memql#160.
func parseDisplayCardAttr(attr *parser.Attribute) (*DisplayCard, error) {
	if attr == nil {
		return nil, fmt.Errorf("@displayCard: nil attribute")
	}
	if len(attr.Args) == 0 {
		return nil, fmt.Errorf("@displayCard requires named arguments (at least primary=\"<field>\")")
	}
	out := &DisplayCard{}
	for k, v := range attr.Args {
		val := strings.TrimSpace(asString(v))
		switch k {
		case "primary":
			out.Primary = val
		case "secondary":
			out.Secondary = val
		case "tertiary":
			out.Tertiary = val
		case "status":
			out.Status = val
		default:
			return nil, fmt.Errorf("@displayCard: unknown argument %q (allowed: primary, secondary, tertiary, status)", k)
		}
	}
	if out.Primary == "" {
		return nil, fmt.Errorf("@displayCard requires primary=\"<field>\"")
	}
	return out, nil
}

// asString coerces an attribute-arg value (any) to its string
// form. Used by the displayCard parser; the parser hands us
// `string` for "..."-quoted args but a bool for bare flags.
// Anything that isn't string-shaped returns "" so the caller's
// "primary required" check fires.
func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// applyPropertyAttribute folds an @annotation into the property
// intermediate. Phase 3 expanded the vocabulary: @unique, @pattern,
// @minLength, @maxLength, @minimum, @maximum, @immutable, @secret.
func applyPropertyAttribute(prop *parsedProperty, attr *parser.Attribute) error {
	if attr == nil {
		return nil
	}
	switch attr.Name {
	case "required":
		prop.required = true
	case "default":
		prop.defaultValue = parseDefaultValue(attrString(attr))
	case "description":
		prop.description = attrString(attr)
	case "unique":
		prop.unique = true
	case "pattern":
		prop.pattern = attrString(attr)
	case "minLength":
		n, err := toInt64(attrNumeric(attr))
		if err != nil {
			return fmt.Errorf("@minLength requires an integer: %w", err)
		}
		prop.minLength = &n
	case "maxLength":
		n, err := toInt64(attrNumeric(attr))
		if err != nil {
			return fmt.Errorf("@maxLength requires an integer: %w", err)
		}
		prop.maxLength = &n
	case "minimum":
		f, err := toFloat64(attrNumeric(attr))
		if err != nil {
			return fmt.Errorf("@minimum requires a number: %w", err)
		}
		prop.minimum = &f
	case "maximum":
		f, err := toFloat64(attrNumeric(attr))
		if err != nil {
			return fmt.Errorf("@maximum requires a number: %w", err)
		}
		prop.maximum = &f
	case "immutable":
		prop.immutable = true
	case "secret":
		prop.secret = true
	case "pii":
		prop.pii = true
	case "internal":
		prop.internal = true
	case "serverSet":
		prop.serverSet = true
	case "variant":
		// @variant(discriminator="field") is handled structurally
		// in propertyDeclToParsed (it triggers variant-branch
		// parsing). Nothing to apply at attribute-fold time; the
		// attribute itself is kept on the AST node so the folder
		// can read its discriminator arg.
		return nil
	default:
		return fmt.Errorf("unknown property annotation @%s on field %q", attr.Name, prop.name)
	}
	return nil
}

// attrNumeric returns the first likely-numeric value attached to an
// attribute. Accepts both single-value form (`@minLength(5)`) and
// named-arg form (`@minLength(value=5)`).
func attrNumeric(attr *parser.Attribute) any {
	if attr == nil {
		return nil
	}
	if attr.Value != nil {
		return attr.Value
	}
	for _, v := range attr.Args {
		return v
	}
	return nil
}

func toFloat64(v any) (float64, error) {
	switch t := v.(type) {
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case float64:
		return t, nil
	case string:
		return strconv.ParseFloat(t, 64)
	default:
		return 0, fmt.Errorf("unsupported type %T", v)
	}
}

// attrString extracts the text value of an attribute, preferring the
// single-string form (`@description("text")`) and falling back to a
// stringified `value` argument.
func attrString(attr *parser.Attribute) string {
	if attr == nil {
		return ""
	}
	if s, ok := attr.Value.(string); ok {
		return s
	}
	if v, ok := attr.Args["value"]; ok {
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// toInt64 coerces an annotation argument value into int64. Accepts
// actual integers and stringified decimals; anything else is an error.
func toInt64(v any) (int64, error) {
	switch t := v.(type) {
	case int:
		return int64(t), nil
	case int64:
		return t, nil
	case float64:
		return int64(t), nil
	case string:
		return strconv.ParseInt(t, 10, 64)
	default:
		return 0, fmt.Errorf("unsupported type %T", v)
	}
}

func parseDefaultValue(text string) any {
	if text == "true" {
		return true
	}
	if text == "false" {
		return false
	}
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		return f
	}
	return text
}

// --- JSON Schema generation (unchanged from the legacy implementation) ---

func buildJSONSchema(conceptName string, parsed *parsedConcept) (map[string]any, error) {
	schema := map[string]any{
		"$id":     conceptName,
		"$schema": "http://json-schema.org/draft-07/schema#",
		"type":    "object",
	}

	if parsed.description != "" {
		schema["description"] = parsed.description
	}

	if parsed.noAdditional {
		schema["additionalProperties"] = false
	}

	if len(parsed.properties) > 0 {
		props := make(map[string]any, len(parsed.properties))
		for _, prop := range parsed.properties {
			propSchema, err := propertyToJSONSchema(prop)
			if err != nil {
				return nil, fmt.Errorf("property %q: %w", prop.name, err)
			}
			props[prop.name] = propSchema
		}
		schema["properties"] = props
	}

	if len(parsed.required) > 0 {
		schema["required"] = parsed.required
	}

	return schema, nil
}

func propertyToJSONSchema(prop parsedProperty) (map[string]any, error) {
	schema := make(map[string]any)

	if prop.description != "" {
		schema["description"] = prop.description
	}
	if prop.defaultValue != nil {
		schema["default"] = prop.defaultValue
	}

	// Phase 3 constraints. These map onto JSON-Schema draft-07
	// keywords where there's a direct equivalent; @unique /
	// @immutable / @secret are engine-level semantic flags that
	// surface as `x-` custom keywords so the schema validator passes
	// them through to the storage layer.
	if prop.pattern != "" {
		schema["pattern"] = prop.pattern
	}
	if prop.minLength != nil {
		schema["minLength"] = *prop.minLength
	}
	if prop.maxLength != nil {
		schema["maxLength"] = *prop.maxLength
	}
	if prop.minimum != nil {
		schema["minimum"] = *prop.minimum
	}
	if prop.maximum != nil {
		schema["maximum"] = *prop.maximum
	}
	if prop.unique {
		schema["x-unique"] = true
	}
	if prop.immutable {
		schema["x-immutable"] = true
	}
	if prop.secret {
		schema["x-secret"] = true
	}
	if prop.pii {
		schema["x-pii"] = true
	}
	if prop.internal {
		schema["x-internal"] = true
	}
	if prop.serverSet {
		schema["x-serverSet"] = true
	}

	switch prop.typeName {
	case "string":
		schema["type"] = "string"
	case "bool":
		schema["type"] = "boolean"
	case "int":
		schema["type"] = "integer"
	case "float":
		schema["type"] = "number"
	case "datetime":
		if prop.required {
			schema["type"] = "string"
			schema["format"] = "date-time"
		} else {
			// An OPTIONAL datetime field must also accept the "unset"
			// sentinels callers and templates use for "no value yet": an
			// empty string "" (the coalesce(x,"") / clear-a-field
			// convention) and JSON null. Forcing format:date-time on those
			// made every optional datetime effectively required and blocked
			// creates/updates that legitimately leave the field empty --
			// in-flight worker invocations (completedAt), un-released
			// workspaces (releasedAt), todos with no deadline (dueAt),
			// clearing a scheduled deletion (deletionScheduledAt), etc.
			// (memql#1629). A NON-empty value must still be a valid RFC3339
			// date-time, so garbage strings are still rejected.
			schema["oneOf"] = []any{
				map[string]any{"type": "string", "format": "date-time"},
				map[string]any{"type": "string", "maxLength": 0},
				map[string]any{"type": "null"},
			}
		}
	case "enum":
		schema["type"] = "string"
		if len(prop.enumValues) > 0 {
			enumVals := make([]any, len(prop.enumValues))
			for i, v := range prop.enumValues {
				enumVals[i] = v
			}
			schema["enum"] = enumVals
		}
	case "array":
		schema["type"] = "array"
		items, err := elementToJSONSchema(prop.element)
		if err != nil {
			return nil, err
		}
		schema["items"] = items
	case "map":
		// map[string]T -> {type: object, additionalProperties: {type: T}}.
		// Lets authors express typed string-keyed maps without falling
		// back to unconstrained `object`.
		schema["type"] = "object"
		values, err := elementToJSONSchema(prop.element)
		if err != nil {
			return nil, err
		}
		schema["additionalProperties"] = values
	case "object":
		schema["type"] = "object"
		if len(prop.nested) > 0 {
			nestedProps := make(map[string]any, len(prop.nested))
			for _, nested := range prop.nested {
				nestedSchema, err := propertyToJSONSchema(nested)
				if err != nil {
					return nil, fmt.Errorf("nested property %q: %w", nested.name, err)
				}
				nestedProps[nested.name] = nestedSchema
			}
			schema["properties"] = nestedProps
		}
		// Discriminated-union variants: emit a oneOf where each
		// branch lists the fields required/allowed for that variant.
		// The discriminator field is recorded under x-discriminator
		// so downstream consumers (Sense hover, form generators) can
		// surface which sibling field determines the branch.
		if len(prop.variants) > 0 {
			oneOf := make([]any, 0, len(prop.variants))
			for _, v := range prop.variants {
				branch := map[string]any{
					"title": v.name,
					"type":  "object",
				}
				branchProps := make(map[string]any, len(v.properties))
				var branchRequired []string
				for _, pp := range v.properties {
					ps, err := propertyToJSONSchema(pp)
					if err != nil {
						return nil, fmt.Errorf("variant %q property %q: %w", v.name, pp.name, err)
					}
					branchProps[pp.name] = ps
					if pp.required {
						branchRequired = append(branchRequired, pp.name)
					}
				}
				if len(branchProps) > 0 {
					branch["properties"] = branchProps
					branch["additionalProperties"] = false
				}
				if len(branchRequired) > 0 {
					branch["required"] = branchRequired
				}
				oneOf = append(oneOf, branch)
			}
			schema["oneOf"] = oneOf
			if prop.variantDiscriminator != "" {
				schema["x-discriminator"] = prop.variantDiscriminator
			}
		}
	case "any", "":
		// Free-form -- no type constraint.
	default:
		return nil, fmt.Errorf("unknown type %q%s", prop.typeName, suggestPropertyType(prop.typeName))
	}

	return schema, nil
}

// elementToJSONSchema lowers the element of a wrapped property through the SAME
// builder a scalar of that type goes through, which is the whole of memql#2951.
//
// The old path had a builder of its own -- a switch ending in
// `default: return "string"` -- so element position had a second, weaker answer
// to "what does this type mean" that never consulted suggestPropertyType. An
// unrecognised element type was neither rejected nor corrected, and a
// recognised one lost whatever its scalar form constrains: `[]datetime` came
// out byte-identical to `[]string`, dropping the RFC3339 check that is the only
// reason to write `datetime`. Sharing the builder makes both impossible by
// construction rather than by a parallel table someone has to remember to
// update.
//
// The error is prefixed rather than replaced so the reader gets the same
// `-- did you mean` correction property position gets, plus where it applies.
func elementToJSONSchema(elem *parsedProperty) (map[string]any, error) {
	if elem == nil {
		// Defensive: propertyDeclToParsed populates element for every array
		// and map. A programmatically-built parsedProperty might not, and the
		// legacy bare `array` default is string.
		elem = &parsedProperty{typeName: "string", required: true}
	}
	schema, err := propertyToJSONSchema(*elem)
	if err != nil {
		return nil, fmt.Errorf("element type: %w", err)
	}
	return schema, nil
}

// propertyTypeSuggestions maps the spellings authors actually reach for onto
// the one this builder accepts. The JSON Schema names dominate the list on
// purpose: the builder EMITS "boolean", "integer" and "number", so an author
// who has read the emitted schema -- or who knows any other config language in
// this stack -- has every reason to type them back in.
//
// They are corrected rather than accepted (memql#2909). Aliasing would give
// the DSL two spellings for one type, which is the same "two implementations
// of one answer" this was filed about, and it would close exactly one of the
// sixteen plausible spellings this builder rejects. A message that names the
// right one closes all sixteen.
var propertyTypeSuggestions = map[string]string{
	"boolean":   "bool",
	"integer":   "int",
	"int64":     "int",
	"long":      "int",
	"number":    "float",
	"double":    "float",
	"decimal":   "float",
	"text":      "string",
	"str":       "string",
	"uuid":      "string",
	"date":      "datetime",
	"timestamp": "datetime",
	"time":      "datetime",
	"list":      "array",
	"dict":      "object",
	"json":      "object",
}

// suggestPropertyType returns a " -- did you mean ..." clause for a known
// mis-spelling, or a clause listing the accepted set for anything else. It is
// always a suffix to an "unknown type" error, never an error on its own.
//
// The vocabulary is small and closed, so listing it in full is cheaper for the
// reader than making them find the docs -- and this error is frequently the
// FIRST thing a bundle author sees from the loader.
func suggestPropertyType(name string) string {
	if want, ok := propertyTypeSuggestions[strings.ToLower(strings.TrimSpace(name))]; ok {
		return fmt.Sprintf(" -- did you mean %q?", want)
	}
	// The author-facing spellings, which are NOT the same as this switch's
	// case labels: `map` and `enum` are cases here, but neither is writable
	// bare -- they are reached only through `map[string]<type>` and
	// `enum("a", "b")`, which the parser lowers to those kinds.
	return ` -- accepted: string, bool, int, float, datetime, object, any, array, ` +
		`[]<type>, map[string]<type>, enum("a", "b")`
}
