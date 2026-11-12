package memoryNodes

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
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
	// Concept is the concept that declares the field: the concept a `{id}`
	// placeholder's id must be of.
	Concept string
	// OwnerField is the field the concept's @rowAuthz(owner=...) names, or
	// "" when it names none. Its placeholder's id must be a user's.
	OwnerField string
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
		out = append(out, BytesField{
			Name: name, Tree: prop.Bytes.Tree, Counted: prop.Bytes.Counted, Under: prop.Bytes.Under,
			Concept: c.Name, OwnerField: rowAuthzOwnerField(c.RowAuthz),
		})
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
func validateBytesUnder(props []parsedProperty, rowAuthz *parser.RowAuthzDecl) error {
	owner := rowAuthzOwnerField(rowAuthz)
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
		if reason := bytesUnderProblem(p.bytes, stringFields, owner); reason != "" {
			return &BytesUnderError{
				Field: p.name,
				Under: p.bytes.Under,
				Msg: fmt.Sprintf("@bytes on `%s` %s. Write the place the row's own bytes live in, as path segments: "+
					"{id} for the row's id, {<owner>} for the field @rowAuthz(owner=...) names, anything else literal "+
					"-- `@bytes(under=\"library/{ownerUserId}\")`, `@bytes(tree, under=\"sites/{id}\")`; a `counted` "+
					"field may also name a literal place or any top-level string field",
					p.name, reason),
			}
		}
	}
	return nil
}

// bytesUnderProblem says what is wrong with one place, or "" when nothing is.
//
// A CARRIED field (not `counted`) is read, so its place must be the row's own
// in a way the row's writer cannot choose: the row's id, or the owner field
// the write guard holds to the writer's own user id. A literal-only place
// would let any row of the kind name any key under it, and any other field is
// one the writer sets to whatever it likes. A COUNTED field's bytes are only
// sized, never read, so it may name a literal place or any string field.
func bytesUnderProblem(d *bytesDecl, stringFields map[string]bool, owner string) string {
	under := d.Under
	if under == "" {
		return "names no place (`under` is missing or empty)"
	}
	segments := strings.Split(under, "/")
	placeholders := 0
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
		if d.Counted || name == "id" || name == owner {
			placeholders++
			continue
		}
		if owner == "" {
			return fmt.Sprintf("writes under=%q, and the concept declares no @rowAuthz(owner=...), so a carried field's place may use only `{id}`: `{%s}` is a field the row's writer chooses", under, name)
		}
		return fmt.Sprintf("writes under=%q, and `{%s}` is neither the row's id, `{id}`, nor the owner field `{%s}` its @rowAuthz names: a carried field may name only bytes that are its own, and `{%s}` is a field the row's writer chooses", under, name, owner, name)
	}
	if !d.Counted && placeholders == 0 {
		return fmt.Sprintf("names the place %q, which has no placeholder: every row of the kind could name any key under it. A carried field's place names the row's id, `{id}`, or the owner field its @rowAuthz names; a field whose keys no row value ties to the row is `counted`", under)
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

// rowAuthzOwnerField is the field an owned @rowAuthz names, or "" when the
// concept declares none. `owner="id"` (a self-owned concept) is the row's id,
// which `{id}` already names, so it names no field here.
func rowAuthzOwnerField(d *parser.RowAuthzDecl) string {
	if d == nil || d.Tier != parser.RowAuthzOwned || d.Owner == "id" {
		return ""
	}
	return d.Owner
}

// userConcept is the concept an owner field's id is of.
const userConcept = "v1:identity:user"

// idSpelling is one id read the way core/librarykey reads it (id.ParseNodeId):
// its concept, empty for a bare id, and its short id. ok is false for a value
// that is not exactly one of the two spellings `<short>` and
// `<concept>:<short>` -- a value carrying anything ahead of its concept, or
// none of a short id, is compared only as written.
func idSpelling(v string) (concept, short string, ok bool) {
	c, sh, err := id.ParseNodeId(v)
	if err != nil || sh == "" {
		return "", "", false
	}
	if c == "" {
		return "", sh, sh == v
	}
	return c, sh, c+":"+sh == v
}

// sameId reports whether a key segment names the same id as a row's stored
// value, the way core/librarykey's sameUser does: equal as written, or both
// the same short id with each side's concept absent or the expected one. So a
// segment `v1:agents:agent:u1` never passes for user `u1`.
func sameId(value, segment, expected string) bool {
	if value == segment {
		return true
	}
	vc, vs, vok := idSpelling(value)
	sc, ss, sok := idSpelling(segment)
	if !vok || !sok || vs != ss {
		return false
	}
	return (vc == "" || vc == expected) && (sc == "" || sc == expected)
}

// placeholderValue is the stored value one placeholder stands for and the
// concept its id must be of: the row's own concept for `{id}`, a user for the
// owner field, and for any other field the concept the stored value itself
// names, when it names one.
//
// Any other field is a COUNTED field's only. On a carried field the load
// refuses such a placeholder, and this answers "" (no place) as well, so a
// BytesField built by hand -- one the load never judged -- cannot widen a
// carried place to a field the row's writer chooses.
//
// The value is returned as stored, never trimmed: a value with surrounding
// whitespace is not written the same as one without, and usableSegment turns
// it into no place at all.
func (f BytesField) placeholderValue(name, rowID string, payload map[string]any) (value, expected string) {
	switch {
	case name == "id":
		return rowID, f.Concept
	case name == f.OwnerField && f.OwnerField != "":
		value, _ = payload[name].(string)
		return value, userConcept
	case !f.Counted:
		return "", ""
	}
	value, _ = payload[name].(string)
	if c, _, ok := idSpelling(value); ok {
		expected = c
	}
	return value, expected
}

// spellings is every way a key segment may write the same id as value: the
// value as stored, and -- when the value is itself one of the two spellings
// of an id of the expected concept, or bare -- its short id and the short id
// under the expected concept. It is exactly the set sameId admits.
//
// Each derived spelling is added only when it reads back as itself: a short id
// that looks like a version or a concept (`v2`, `v2:a:b`) is not a bare id to
// idSpelling, so sameId would refuse it, and Places must not offer it.
func spellings(value, expected string) []string {
	out := []string{value}
	c, short, ok := idSpelling(value)
	if !ok || (c != "" && c != expected) {
		return out
	}
	if bc, bs, bok := idSpelling(short); bok && bc == "" && bs == short {
		out = append(out, short)
	}
	if expected != "" {
		full := expected + ":" + short
		if fc, fs, fok := idSpelling(full); fok && fc == expected && fs == short {
			out = append(out, full)
		}
	}
	return out
}

// usableSegment reports whether a stored value can stand as one key segment.
// A value holding a separator or naming a relative step would build a place
// of another depth, or one that climbs out, so it builds none.
func usableSegment(v string) bool {
	return v != "" && v == strings.TrimSpace(v) && !relativeOrSeparated(v)
}

// relativeOrSeparated reports whether a segment is, or percent-decodes to, a
// relative step or something holding a separator. Today's storage client
// escapes a name, so an encoded `..` stays literal there; this refuses it
// anyway, so no reader that decodes once can be walked out of a place.
func relativeOrSeparated(seg string) bool {
	for _, v := range []string{seg, percentDecoded(seg)} {
		if v == "." || v == ".." || strings.ContainsAny(v, "/\\") {
			return true
		}
	}
	return false
}

// percentDecoded is seg with its percent escapes decoded once, or seg itself
// when it holds none that decode.
func percentDecoded(seg string) string {
	if d, err := url.PathUnescape(seg); err == nil {
		return d
	}
	return seg
}

// Places answers the key prefixes the row's own bytes may live under: Under
// with each placeholder replaced by every spelling of the row's value
// (spellings), segments joined by "/", no trailing slash, one prefix per
// combination, deduplicated and sorted. The blob reader compares these
// prefixes exactly, so the list is every spelling a real key can have and no
// other -- the same set Admits accepts. A placeholder with no usable value
// (missing, empty, not a string, with surrounding whitespace, holding a
// separator, or with a spelling that percent-decodes to a relative step or a
// separator) gives an empty slice: a row that does not say whose bytes these
// are owns none.
//
// A `counted` field's places are for SIZING only: a reader checks Counted
// before it opens anything, because a counted place may be literal and so
// hold every row's bytes.
func (f BytesField) Places(rowID string, payload map[string]any) []string {
	out := []string{}
	if strings.TrimSpace(f.Under) == "" {
		return out
	}
	prefixes := []string{""}
	for i, seg := range strings.Split(f.Under, "/") {
		choices := []string{seg}
		if name, ok := bytesPlaceholder(seg); ok {
			value, expected := f.placeholderValue(name, rowID, payload)
			if !usableSegment(value) {
				return out
			}
			choices = spellings(value, expected)
			// Every spelling must stand as a segment too: an id whose short
			// form decodes to a relative step (`v1:platform:site:%2e`) owns
			// no bytes, rather than a place that offers `sites/%2e`, which
			// Admits refuses.
			for _, c := range choices {
				if !usableSegment(c) {
					return out
				}
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

// Admits reports whether key lies STRICTLY inside the row's place: split on
// "/", every literal segment equal, every placeholder segment the same id as
// the row's value (sameId, with the concept that placeholder expects), and at
// least one more non-empty segment after the place. A key with a relative
// segment anywhere -- after either slash, and once percent-decoded -- is
// refused, since storage could resolve it outside.
//
// For a `counted` field an admission is for SIZING only: a reader checks
// Counted before it opens anything, because a counted place may be literal
// and so admit every row's bytes.
func (f BytesField) Admits(rowID string, payload map[string]any, key string) bool {
	if key == "" || len(f.Places(rowID, payload)) == 0 {
		return false
	}
	// A backslash is split on too for this one check, as core/librarykey
	// does: storage may read one as a slash, and only a relative segment can
	// climb out of the place.
	for _, s := range strings.Split(strings.ReplaceAll(key, "\\", "/"), "/") {
		if relativeOrSeparated(s) {
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
	for i, seg := range tmpl {
		name, isPlaceholder := bytesPlaceholder(seg)
		if !isPlaceholder {
			if seg != keySegs[i] {
				return false
			}
			continue
		}
		value, expected := f.placeholderValue(name, rowID, payload)
		if !sameId(value, keySegs[i], expected) {
			return false
		}
	}
	return true
}
