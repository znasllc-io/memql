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
	Under   string
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
	// Under is the place the row's own bytes live in, the template as
	// written (`library/{ownerUserId}`). Places and Admits read it.
	Under string
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

// attrKeyString reads a string key of a keywords annotation, trimmed; "" when
// it was not written.
func attrKeyString(attr *parser.Attribute, key string) string {
	if attr == nil || attr.Args == nil {
		return ""
	}
	v, _ := attr.Args[key].(string)
	return strings.TrimSpace(v)
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
				Tree    bool   `json:"tree"`
				Counted bool   `json:"counted"`
				Under   string `json:"under"`
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
		out = append(out, BytesField{Name: name, Tree: prop.Bytes.Tree, Counted: prop.Bytes.Counted, Under: prop.Bytes.Under})
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

// CodeBytesUnder is the stable rule id of a @bytes whose `under` names no
// place, or a place that is not built from the row's own values.
const CodeBytesUnder = "bytes_under"

// bytesUnderReservedPrefix is the first segment an account export writes its
// own files under. No row's bytes may claim it: a declaration naming it would
// let a row point the export at another account's file.
const bytesUnderReservedPrefix = "exports"

// BytesUnderError refuses a @bytes whose `under` does not name the row's own
// place.
type BytesUnderError struct {
	Field string
	Under string
	Msg   string
}

func (e *BytesUnderError) Error() string { return e.Msg + " [" + CodeBytesUnder + "]" }

// RuleCode is the refusal's stable rule id (baseloader.CodedRefusal).
func (e *BytesUnderError) RuleCode() string { return CodeBytesUnder }

// validateBytesUnder checks every top-level @bytes field's place against the
// concept's own fields (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, section C).
//
// WHY THE PLACE IS REQUIRED. A row names its own bytes, and a row's writer
// can name anything. Each of today's readers holds its own kind to a place (a
// Library file's key under its owner's prefix, a site to its own bundle); the
// declaration says that place once, so the export reads a key only when it
// lies strictly inside the place the row's own values build. A field with no
// place has no bytes the export may read, so it is refused rather than read
// without one.
func validateBytesUnder(props []parsedProperty) error {
	stringFields := map[string]bool{}
	for _, p := range props {
		if p.typeName == "string" {
			stringFields[p.name] = true
		}
	}
	for _, p := range props {
		if p.bytes == nil {
			continue
		}
		if reason := bytesUnderProblem(p.bytes, stringFields); reason != "" {
			return &BytesUnderError{
				Field: p.name,
				Under: p.bytes.Under,
				Msg: fmt.Sprintf("@bytes on `%s` %s. Write the place the row's own bytes live in, as path segments: "+
					"{id} for the row's id, {<field>} for one of the concept's top-level string fields, anything else "+
					"literal -- `@bytes(under=\"library/{ownerUserId}\")`, `@bytes(tree, under=\"sites/{id}\")`",
					p.name, reason),
			}
		}
	}
	return nil
}

// bytesUnderProblem says what is wrong with one place, or "" when nothing is.
func bytesUnderProblem(d *bytesDecl, stringFields map[string]bool) string {
	under := d.Under
	if under == "" {
		return "names no place (`under` is missing or empty)"
	}
	segments := strings.Split(under, "/")
	for _, seg := range segments {
		switch {
		case seg == "":
			return fmt.Sprintf("has an empty segment in under=%q: no leading, trailing or doubled \"/\"", under)
		case seg == "." || seg == "..":
			return fmt.Sprintf("has a relative segment %q in under=%q", seg, under)
		}
		name, isPlaceholder := bytesPlaceholder(seg)
		if !isPlaceholder {
			if strings.ContainsAny(seg, "{}") {
				return fmt.Sprintf("has the segment %q in under=%q: a placeholder is a whole segment, `{id}` or `{<field>}`", seg, under)
			}
			continue
		}
		if name != "id" && !stringFields[name] {
			return fmt.Sprintf("writes under=%q, and `{%s}` names neither the row's id nor a string field the concept declares at its top level", under, name)
		}
	}
	if segments[0] == bytesUnderReservedPrefix {
		return fmt.Sprintf("names a place under `%s` in under=%q, where an account export writes its own files", bytesUnderReservedPrefix, under)
	}
	// A tree is every object under a prefix, so a one-segment place would let
	// a row name a whole top-level namespace (every Library file) as its own.
	if d.Tree && len(segments) < 2 {
		return fmt.Sprintf("is a tree under the one-segment place %q: a tree's place has at least two segments, so no row names a whole top-level prefix", under)
	}
	return ""
}

// bytesPlaceholder reports whether a segment is written `{name}` and returns
// the name.
func bytesPlaceholder(seg string) (string, bool) {
	if len(seg) < 3 || seg[0] != '{' || seg[len(seg)-1] != '}' {
		return "", false
	}
	return seg[1 : len(seg)-1], true
}

// bareIdForm is the bare spelling of an id: the part after the last ":". A
// value with no ":" is its own bare form.
func bareIdForm(v string) string {
	if i := strings.LastIndex(v, ":"); i >= 0 {
		return v[i+1:]
	}
	return v
}

// Places answers the key prefixes the row's own bytes may live under: Under
// with `{id}` replaced by the row's id and each `{field}` by the payload's
// value, segments joined by "/", no trailing slash. An id is stored in two
// spellings, bare and canonical, and a key may hold either, so a placeholder
// whose value has a canonical form contributes both, one prefix per
// combination, deduplicated and sorted. A placeholder with no value (missing,
// empty or not a string) gives an empty slice: a row that does not say whose
// bytes these are owns none.
func (f BytesField) Places(rowID string, payload map[string]any) []string {
	out := []string{}
	if strings.TrimSpace(f.Under) == "" {
		return out
	}
	prefixes := []string{""}
	for i, seg := range strings.Split(f.Under, "/") {
		choices := []string{seg}
		if name, ok := bytesPlaceholder(seg); ok {
			var value string
			if name == "id" {
				value = rowID
			} else if v, isString := payload[name].(string); isString {
				value = v
			}
			value = strings.TrimSpace(value)
			if value == "" {
				return out
			}
			choices = []string{value}
			if bare := bareIdForm(value); bare != value {
				if bare == "" {
					return out
				}
				choices = append(choices, bare)
			}
		}
		next := make([]string, 0, len(prefixes)*len(choices))
		for _, p := range prefixes {
			for _, c := range choices {
				if i == 0 {
					next = append(next, c)
				} else {
					next = append(next, p+"/"+c)
				}
			}
		}
		prefixes = next
	}
	seen := map[string]bool{}
	for _, p := range prefixes {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// Admits reports whether key lies STRICTLY inside one of the row's places:
// split on "/", every segment of the place equal to the key's, and at least
// one more segment after it. A placeholder segment is compared in bare form on
// both sides, so `library/u1/...` and `library/v1:identity:user:u1/...` are
// the same owner's. A key with a relative segment anywhere is refused, since
// storage could resolve it outside the place.
func (f BytesField) Admits(rowID string, payload map[string]any, key string) bool {
	if key == "" || len(f.Places(rowID, payload)) == 0 {
		return false
	}
	// A backslash is split on too for this one check, as core/librarykey
	// does: storage may read one as a slash, and only a relative segment can
	// climb out of the place.
	for _, s := range strings.Split(strings.ReplaceAll(key, "\\", "/"), "/") {
		if s == "." || s == ".." {
			return false
		}
	}
	keySegs := strings.Split(key, "/")
	tmpl := strings.Split(f.Under, "/")
	// The key needs one segment past the place, and that segment must name
	// something: `library/u1/` is the place itself, not a key inside it.
	if len(keySegs) <= len(tmpl) || strings.Join(keySegs[len(tmpl):], "") == "" {
		return false
	}
	for _, place := range f.Places(rowID, payload) {
		placeSegs := strings.Split(place, "/")
		if len(placeSegs) != len(tmpl) {
			continue
		}
		match := true
		for i, ps := range placeSegs {
			if _, isPlaceholder := bytesPlaceholder(tmpl[i]); isPlaceholder {
				if bareIdForm(ps) != bareIdForm(keySegs[i]) {
					match = false
					break
				}
				continue
			}
			if ps != keySegs[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
