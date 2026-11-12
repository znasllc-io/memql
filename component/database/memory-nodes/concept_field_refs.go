package memoryNodes

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/parser"
)

// @bytes and @secretName -- a field that NAMES something kept outside the row
// (design record docs/superpowers/specs/2026-11-12-account-export-design.md,
// section C). @bytes names bytes in blob storage, @secretName the name of a
// secret. An account export reads both: it copies the bytes into the file and
// lists each secret name as a connection to set up again, never its value.
//
// Both travel the route @secret takes: the annotation becomes a custom keyword
// on the field's definition schema (x-bytes, x-secret-name), and the accessors
// below read the keyword back, so a stored schema answers the same question a
// freshly built concept does.

// CodeFieldAnnotationType is the stable rule id of a @bytes or @secretName on a
// field that cannot hold what the annotation says it holds.
const CodeFieldAnnotationType = "field_annotation_type"

// bytesDecl is a field's @bytes declaration as the concept parser folds it.
type bytesDecl struct {
	Tree    bool
	Counted bool
}

// BytesField is one top-level field a concept declares @bytes on.
type BytesField struct {
	Name string
	// Tree: @bytes(tree) -- the field names a prefix, and the bytes are every
	// object under it, not one object.
	Tree bool
	// Counted: @bytes(counted) -- an account export counts the bytes in the
	// file's report and never carries them.
	Counted bool
}

// FieldAnnotationTypeError refuses a @bytes or @secretName the field's type
// cannot carry.
type FieldAnnotationTypeError struct {
	Annotation string
	Field      string
	Msg        string
}

func (e *FieldAnnotationTypeError) Error() string {
	return e.Msg + " [" + CodeFieldAnnotationType + "]"
}

// RuleCode is the refusal's stable rule id (baseloader.CodedRefusal).
func (e *FieldAnnotationTypeError) RuleCode() string { return CodeFieldAnnotationType }

// fieldReferenceAnnotations lists the reference annotations set on prop, as an
// author wrote them.
func fieldReferenceAnnotations(prop parsedProperty) []string {
	var names []string
	if prop.bytes != nil {
		names = append(names, "@bytes")
	}
	if prop.secretName {
		names = append(names, "@secretName")
	}
	return names
}

// checkFieldReferenceType refuses @bytes or @secretName on a field that is not
// a string or a list of strings. On any other type the declaration would be one
// nothing can read: an export looks for a key or a name and finds a number.
// `enum` and `datetime` are strings to JSON Schema but are left out on purpose:
// neither can hold a storage key or a secret's name.
func checkFieldReferenceType(out parsedProperty, prop *parser.PropertyDecl) error {
	names := fieldReferenceAnnotations(out)
	if len(names) == 0 {
		return nil
	}
	if out.typeName == "string" {
		return nil
	}
	if out.typeName == "array" && out.element != nil && out.element.typeName == "string" {
		return nil
	}
	return &FieldAnnotationTypeError{
		Annotation: names[0],
		Field:      out.name,
		Msg: fmt.Sprintf("%s cannot be declared on `%s`, which is %s: it names something kept outside the row, "+
			"so the field is a string or a list of strings (`%s string %s`, `%s []string %s`)",
			strings.Join(names, " and "), out.name, renderTypeRef(prop.Type),
			out.name, names[0], out.name, names[0]),
	}
}

// refuseNestedFieldReference refuses @bytes or @secretName inside a nested
// block or a variant branch. The accessors read top-level fields only, as
// SecretFields does, so a nested declaration would load and then be read by
// nobody -- an export would carry the row and silently leave its bytes behind.
func refuseNestedFieldReference(pp parsedProperty) error {
	names := fieldReferenceAnnotations(pp)
	if len(names) == 0 {
		return nil
	}
	return &FieldAnnotationTypeError{
		Annotation: names[0],
		Field:      pp.name,
		Msg: fmt.Sprintf("%s cannot be declared on the nested field `%s`: it is read on a concept's top-level fields only. "+
			"Move the field to the top level of the concept", strings.Join(names, " and "), pp.name),
	}
}

// attrFlag reads a bare key of a flag-or-keywords annotation. The parser
// stores a bare key as `true`; the registry has already refused a valued one.
func attrFlag(attr *parser.Attribute, key string) bool {
	if attr == nil || attr.Args == nil {
		return false
	}
	v, ok := attr.Args[key].(bool)
	return ok && v
}

// BytesFields returns every top-level field the concept's definition schema
// marks with x-bytes (emitted from @bytes), sorted by name. Nil when there is
// none, and for a nil concept.
func (c *Concept) BytesFields() []BytesField {
	if c == nil {
		return nil
	}
	raw, ok := c.Schemas[definitionSchemaKey]
	if !ok || len(raw) == 0 {
		return nil
	}
	var schema struct {
		Properties map[string]struct {
			Bytes *struct {
				Tree    bool `json:"tree"`
				Counted bool `json:"counted"`
			} `json:"x-bytes"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil
	}
	var out []BytesField
	for name, prop := range schema.Properties {
		if prop.Bytes == nil {
			continue
		}
		out = append(out, BytesField{Name: name, Tree: prop.Bytes.Tree, Counted: prop.Bytes.Counted})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SecretNameFields returns every top-level field the concept's definition
// schema marks with x-secret-name (emitted from @secretName), sorted. Nil when
// there is none, and for a nil concept.
func (c *Concept) SecretNameFields() []string {
	if c == nil {
		return nil
	}
	raw, ok := c.Schemas[definitionSchemaKey]
	if !ok || len(raw) == 0 {
		return nil
	}
	var schema struct {
		Properties map[string]struct {
			SecretName bool `json:"x-secret-name"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil
	}
	var out []string
	for name, prop := range schema.Properties {
		if prop.SecretName {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
