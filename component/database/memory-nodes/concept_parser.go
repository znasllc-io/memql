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

	return &Concept{
		Name:          conceptName,
		SchemaId:      conceptName,
		Schemas:       schemas,
		NodeType:      nodeType,
		Description:   parsed.description,
		Version:       parsed.version,
		Relationships: parsed.relationships,
	}, nil
}

// parsedConcept is the intermediate representation feeding into the
// JSON-Schema builder. Kept around so the builder doesn't need to know
// about the shared AST's ConceptDecl shape.
type parsedConcept struct {
	description   string
	conceptType   string
	version       string // @version("vN") override; empty means derived from path
	properties    []parsedProperty
	required      []string
	relationships []RelationshipDefinition
	noAdditional  bool // default true for concept.memql
}

// parsedProperty mirrors the legacy internal type so the JSON-Schema
// builder can stay unchanged. Phase 3 adds fields for the expanded
// property annotation vocabulary (@unique, @pattern, @minLength,
// @maxLength, @minimum, @maximum, @immutable, @secret) and for
// discriminated-union variants (@variant).
type parsedProperty struct {
	name          string
	typeName      string
	description   string
	defaultValue  any
	required      bool
	enumValues    []string
	arrayItemType string
	nested        []parsedProperty
	format        string
	// Phase 3 constraints
	unique    bool
	pattern   string
	minLength *int64
	maxLength *int64
	minimum   *float64
	maximum   *float64
	immutable bool
	secret    bool
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
		if prop.Type.Kind == "array" && prop.Type.ArrayItem != nil {
			out.arrayItemType = prop.Type.ArrayItem.Kind
		}
		if prop.Type.Kind == "map" && prop.Type.ArrayItem != nil {
			// Reuse arrayItemType for the value-type of a map since
			// the JSON-Schema builder treats both array items and
			// map values as a single "element type". The "map"
			// typeName differentiates at build time.
			out.arrayItemType = prop.Type.ArrayItem.Kind
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
	return out, nil
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
		// @version("MAJOR.MINOR.PATCH") -- strict semver. Only the
		// major segment flows into the concept ID prefix
		// (v<MAJOR>:...); minor + patch document additive schema
		// evolution within a major version.
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
	default:
		return fmt.Errorf("unknown concept annotation @%s", attr.Name)
	}
	return nil
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
		schema["type"] = "string"
		schema["format"] = "date-time"
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
		itemType := prop.arrayItemType
		if itemType == "" {
			itemType = "string"
		}
		schema["items"] = map[string]any{"type": memqlTypeToJSONType(itemType)}
	case "map":
		// map[string]T -> {type: object, additionalProperties: {type: T}}.
		// Lets authors express typed string-keyed maps without falling
		// back to unconstrained `object`.
		schema["type"] = "object"
		valueType := prop.arrayItemType
		if valueType == "" {
			valueType = "string"
		}
		schema["additionalProperties"] = map[string]any{"type": memqlTypeToJSONType(valueType)}
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
		return nil, fmt.Errorf("unknown type %q", prop.typeName)
	}

	return schema, nil
}

func memqlTypeToJSONType(t string) string {
	switch t {
	case "string", "datetime":
		return "string"
	case "bool":
		return "boolean"
	case "int":
		return "integer"
	case "float":
		return "number"
	case "object":
		return "object"
	case "array":
		return "array"
	default:
		return "string"
	}
}
