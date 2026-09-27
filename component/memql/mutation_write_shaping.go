package memql

import (
	"fmt"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
)

// mutation_write_shaping.go -- the write-shaping annotations name fields the
// write can shape (memql#5426).
//
// @createOnly, @noUnset, @mergeFields, @appendFields, @addToSet and
// @removeFromSet each change how one WRITTEN top-level field meets the stored
// row. Their extractors (mutation_templates.go) held the annotation to the
// mutation's kind and to a non-empty list, and nothing else: a name the concept
// does not declare, a field the mutation does not write, or a field of a type
// the annotation cannot shape all loaded, and each did nothing at all -- the
// executor keys on the name and simply never meets it -- so the author's
// intent (keep this, append to that) silently did not happen.
//
// Each named field is held to three things, in this order, so the message
// answers the first mistake an author made:
//
//   - the bound concept declares it, as a top-level field -- the executor's
//     unit is the top-level payload key (mergePayloadFields), so a dotted
//     path names nothing it will ever meet;
//   - its declared type is one the annotation shapes: a list for the three
//     list verbs, an object or map for @mergeFields;
//   - the mutation's block writes it. When the block's payload is one
//     caller-supplied object (`insert { args.payload, ... }`) the fields it
//     writes are the caller's to decide, and this part is not checked.
//
// A concept the registry cannot resolve checks nothing but the last rule:
// the unresolved binding is its own refusal.

// The rule ids of the three refusals (D24).
const (
	CodeWriteAnnotationUndeclaredField = "write_annotation_undeclared_field"
	CodeWriteAnnotationFieldType       = "write_annotation_field_type"
	CodeWriteAnnotationUnwrittenField  = "write_annotation_unwritten_field"
)

// writeShaping is one write-shaping annotation as the loader read it.
type writeShaping struct {
	annotation string
	fields     []string
	// needs is the kind of field the annotation shapes: "list", "object", or
	// "" for any type.
	needs string
}

// WriteAnnotationError is one refusal of a write-shaping annotation's field.
type WriteAnnotationError struct {
	Annotation string
	Field      string
	Code       string
	Reason     string
	Fix        string
}

// Error names the annotation and the field as written, why the field cannot be
// shaped, and the fix, with the rule id last in brackets.
func (e *WriteAnnotationError) Error() string {
	return fmt.Sprintf("@%s(%q) %s -- %s [%s]", e.Annotation, e.Field, e.Reason, e.Fix, e.Code)
}

// RuleCode is the refusal's stable rule id (baseloader.CodedRefusal).
func (e *WriteAnnotationError) RuleCode() string { return e.Code }

// validateWriteShapingFields holds every field a write-shaping annotation names
// to the concept the mutation writes and to its block (see the file comment).
func validateWriteShapingFields(registry memoryNodes.Registry, conceptName string, tmpl *FunctionMutationTemplate, verb string, shaping []writeShaping) error {
	if tmpl == nil {
		return nil
	}
	if verb == "" {
		verb = string(ast.MutationKindInsert) // an unmarked statement is an insert
	}
	var declared map[string]conceptFieldShape
	if registry != nil && strings.TrimSpace(conceptName) != "" {
		if concept, err := registry.Get(conceptName); err == nil && concept != nil {
			if fields, ferr := flattenConceptFields(concept); ferr == nil {
				declared = fields
			}
		}
	}
	written := mutationBlockFieldsV1(tmpl)
	_, blockIsFields := tmpl.PayloadTemplate.(map[string]any)
	if tmpl.PayloadTemplate == nil {
		blockIsFields = true
	}
	for _, s := range shaping {
		for _, field := range s.fields {
			if declared != nil {
				shape, ok := declared[field]
				if !ok || strings.Contains(field, ".") {
					return &WriteAnnotationError{Annotation: s.annotation, Field: field, Code: CodeWriteAnnotationUndeclaredField,
						Reason: fmt.Sprintf("names a field concept %q does not declare as a top-level field, so the write never meets it", conceptName),
						Fix:    "name one of the concept's top-level fields, or delete it from the annotation"}
				}
				if msg := writeShapingTypeMismatch(s, shape.Type); msg != "" {
					return &WriteAnnotationError{Annotation: s.annotation, Field: field, Code: CodeWriteAnnotationFieldType,
						Reason: fmt.Sprintf("names a field declared %s: %s", shape.Type, msg),
						Fix:    "name a field of that type, or delete it from the annotation"}
				}
			}
			if blockIsFields {
				if _, ok := written[field]; !ok {
					return &WriteAnnotationError{Annotation: s.annotation, Field: field, Code: CodeWriteAnnotationUnwrittenField,
						Reason: fmt.Sprintf("names a field this mutation's %s block does not write, so the annotation has nothing to shape", verb),
						Fix:    fmt.Sprintf("write `%s` in the %s block, or delete it from the annotation", field, verb)}
				}
			}
		}
	}
	return nil
}

// writeShapingTypeMismatch says why a field of the declared type cannot be
// shaped by the annotation, or "" when it can. A field declared `any` can be
// anything, and is not refused.
func writeShapingTypeMismatch(s writeShaping, declaredType string) string {
	if declaredType == "any" {
		return ""
	}
	switch s.needs {
	case "list":
		if !strings.HasPrefix(declaredType, "[]") {
			return fmt.Sprintf("@%s changes the members of a stored list, so it takes a []T field", s.annotation)
		}
	case "object":
		if declaredType != "object" && declaredType != "variant" && !strings.HasPrefix(declaredType, "map[") {
			return fmt.Sprintf("@%s deep-merges a stored object, so it takes an object or map field", s.annotation)
		}
	}
	return ""
}
