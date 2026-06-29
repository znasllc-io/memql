package memql

// spec_binding_resolver.go is the post-load pass that finalizes every
// spec + trait under the spec/shape binding redesign (epic #2281). It
// runs at engine bootstrap AFTER concepts, shapes, and the raw spec
// slices are loaded, so it has the registries the per-slice converter
// (specDeclToSpec) lacks.
//
// The per-slice converter parses the new-form body -- bare field names,
// no payload./actor./row. prefix -- and stores the signature binding
// name (Spec.BoundName) verbatim, leaving Spec.Kind empty. This pass:
//
//  1. Resolves the binding. A spec binds exactly one shape XOR concept;
//     resolution tries the shape registry first, then the concept
//     registry (by trailing-segment match, mirroring shape default
//     projection). A trait is deliberately UNBOUND (BoundName empty).
//  2. Rewrites the body's bare field references to their underlying
//     access form so the existing SQL compiler / in-process evaluator
//     consume an unchanged Expr shape:
//       - shape binding: each bare field must be a projected key of the
//         shape; it rewrites to the shape's stored path (payload.X /
//         actor.X / a bare intrinsic).
//       - concept binding: a bare intrinsic stays bare; any other bare
//         field must be a declared payload field and rewrites to
//         payload.X.
//       - trait: a bare intrinsic stays bare; any other bare field
//         rewrites to payload.X (existence is validated at the call
//         site against the concrete concept, as before).
//  3. Classifies the spec: an @actor shape -> context-spec; a concept
//     or @row (or mixed) shape, or a trait -> row-spec.
//
// The "mixed body" rejection of the old derive-from-body model is
// obsolete: one binding = one surface, so a mix is unrepresentable.

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// resolveSpecBindings finalizes every registered spec + trait. Per-spec
// failures are accumulated and returned as one joined error (the
// bootstrap logs it); a misbinding is a contract violation surfaced
// loudly rather than silently dropped. The migrated tree resolves
// cleanly, so this returns nil in the steady state.
func resolveSpecBindings(logger *slog.Logger, specs *SpecRegistry, shapes *ShapeRegistry, concepts memoryNodes.Registry) error {
	if specs == nil {
		return nil
	}
	var errs []error
	resolved := 0
	for _, spec := range specs.List() {
		if spec == nil {
			continue
		}
		if err := resolveOneSpecBinding(spec, shapes, concepts); err != nil {
			errs = append(errs, fmt.Errorf("%s: spec %q: %w", spec.Origin, spec.Name, err))
			continue
		}
		if err := specs.Upsert(spec.Name, spec); err != nil {
			errs = append(errs, fmt.Errorf("%s: re-register spec %q: %w", spec.Origin, spec.Name, err))
			continue
		}
		resolved++
	}
	if logger != nil {
		logger.Info("memql.specBindingResolver: resolved spec/trait bindings",
			"component", "memql.engine", "count", resolved, "errors", len(errs))
	}
	return errors.Join(errs...)
}

// resolveOneSpecBinding mutates spec in place: rewrites spec.Expr to the
// underlying access form, sets spec.Kind, and refreshes spec.ExprSource.
func resolveOneSpecBinding(spec *Spec, shapes *ShapeRegistry, concepts memoryNodes.Registry) error {
	var (
		kind    SpecKind
		mapper  func(FieldReference) (FieldReference, error)
		bindErr error
	)

	switch {
	case spec.IsTrait:
		// Deliberately unbound row predicate: bare intrinsic stays bare,
		// any other bare field -> payload.X (validated at the call site).
		kind = SpecKindRow
		mapper = conceptFieldMapper(nil)

	case spec.BoundName == "":
		return fmt.Errorf("non-trait spec has no signature binding -- declare `spec <boundName> %s { ... }`", spec.Name)

	default:
		// Shape binding takes precedence (the import path disambiguates
		// shapes vs concepts at authoring time; here a name lookup against
		// the shape registry first, then concepts, is sufficient for the
		// functional rewrite).
		if shape, ok := shapeLookup(shapes, spec.BoundName); ok {
			kind = shapeSpecKind(shape)
			mapper, bindErr = shapeFieldMapper(spec.BoundName, shape)
			if bindErr != nil {
				return bindErr
			}
		} else if concept, err := resolveConceptByTrailingSegment(concepts, spec.BoundName); err == nil && concept != nil {
			kind = SpecKindRow
			mapper = conceptFieldMapper(concept)
		} else {
			return fmt.Errorf("binding %q resolves to neither an imported shape nor a concept -- check the file-top `use` import (use ...shapes.{ %s } for a shape, use ...concepts.{ %s } for a concept)", spec.BoundName, spec.BoundName, spec.BoundName)
		}
	}

	if err := rewriteSpecFields(spec.Expr, mapper); err != nil {
		return err
	}
	spec.Kind = kind
	spec.ExprSource = canonicalExpression(spec.Expr)
	return nil
}

// shapeSpecKind picks the eval strategy for a shape-bound spec: a pure
// @actor shape evaluates in-process (context-spec); a concept-projecting
// @row shape (or a mixed shape) compiles to SQL (row-spec).
func shapeSpecKind(shape *ShapeDefinition) SpecKind {
	if shape.KindActor && !shape.KindRow {
		return SpecKindContext
	}
	return SpecKindRow
}

// shapeLookup resolves a shape by name against the registry.
func shapeLookup(shapes *ShapeRegistry, name string) (*ShapeDefinition, bool) {
	if shapes == nil {
		return nil, false
	}
	return shapes.Get(name)
}

// shapeFieldMapper builds the bare-field -> underlying-path rewriter for
// a shape binding. Every bare field in the spec body must be a projected
// key of the shape; it rewrites to the shape's stored path.
func shapeFieldMapper(boundName string, shape *ShapeDefinition) (func(FieldReference) (FieldReference, error), error) {
	keys := make(map[string]string, len(shape.Template))
	for key, raw := range shape.Template {
		s, ok := raw.(string)
		if !ok {
			continue
		}
		keys[key] = extractNodePath(s)
	}
	return func(ref FieldReference) (FieldReference, error) {
		if len(ref.Parts) != 1 {
			return ref, fmt.Errorf("field %q reads through a shape binding by bare name only (no dotted path)", joinParts(ref.Parts))
		}
		field := ref.Parts[0]
		path, ok := keys[field]
		if !ok {
			return ref, fmt.Errorf("field %q is not a projected key of bound shape %q (keys: %s)", field, boundName, sortedShapeKeys(keys))
		}
		return fieldReferenceFromPath(path), nil
	}, nil
}

// conceptFieldMapper builds the bare-field -> underlying-path rewriter
// for a concept binding (concept non-nil) or a trait (concept nil). A
// bare intrinsic stays bare; any other bare field rewrites to payload.X.
// When concept is non-nil the field must be a declared payload field.
func conceptFieldMapper(concept *memoryNodes.Concept) func(FieldReference) (FieldReference, error) {
	var declared map[string]bool
	if concept != nil {
		fields := concept.DeclaredFields()
		declared = make(map[string]bool, len(fields))
		for _, f := range fields {
			declared[f] = true
		}
	}
	return func(ref FieldReference) (FieldReference, error) {
		if len(ref.Parts) != 1 {
			return ref, fmt.Errorf("field %q reads the bound concept by bare name only (no dotted path)", joinParts(ref.Parts))
		}
		field := ref.Parts[0]
		if isSpecIntrinsicField(field) {
			return fieldReferenceFromPath(canonicalSpecIntrinsic(field)), nil
		}
		if declared != nil && !declared[field] {
			return ref, fmt.Errorf("field %q is not a declared payload field of bound concept %q", field, concept.Name)
		}
		return fieldReferenceFromPath("payload." + field), nil
	}
}

// rewriteSpecFields walks the expression tree and rewrites every field
// reference via mapper. Spec references (specName()) and comparison
// values are left untouched.
func rewriteSpecFields(expr ExpressionNode, mapper func(FieldReference) (FieldReference, error)) error {
	if expr == nil || mapper == nil {
		return nil
	}
	switch node := expr.(type) {
	case *ComparisonExpression:
		newRef, err := mapper(node.Field)
		if err != nil {
			return err
		}
		node.Field = newRef
		for i := range node.FieldSelections {
			rewritten, selErr := mapper(node.FieldSelections[i])
			if selErr != nil {
				return selErr
			}
			node.FieldSelections[i] = rewritten
		}
		return nil
	case *LogicalExpression:
		if err := rewriteSpecFields(node.Left, mapper); err != nil {
			return err
		}
		return rewriteSpecFields(node.Right, mapper)
	case *RelationshipExpression:
		return rewriteSpecFields(node.Target, mapper)
	default:
		return nil
	}
}

// extractNodePath unwraps the shape Template storage form
// `node(\"<path>\")` back to the bare `<path>`. The stored string
// carries literal backslash-quotes (see shapeDeclToShapeDefinition /
// expandDefaultShapeProjections), so the affixes include the backslash.
func extractNodePath(stored string) string {
	s := strings.TrimSpace(stored)
	s = strings.TrimPrefix(s, `node(\"`)
	s = strings.TrimSuffix(s, `\")`)
	return s
}

// fieldReferenceFromPath builds a FieldReference from a dotted path.
func fieldReferenceFromPath(path string) FieldReference {
	return FieldReference{Raw: path, Parts: strings.Split(path, ".")}
}

// isSpecIntrinsicField reports whether a bare field name is a row
// intrinsic (kept bare on rewrite) rather than a payload field.
func isSpecIntrinsicField(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "schema", "partition":
		return true
	}
	_, ok := resolveIntrinsicField(name)
	return ok
}

// canonicalSpecIntrinsic returns the canonical casing for a bare
// intrinsic field (e.g. createdat -> createdAt) so the rewritten Expr
// matches what the SQL compiler / evaluator expect.
func canonicalSpecIntrinsic(name string) string {
	if c, ok := canonicalIntrinsicFieldName(name); ok {
		return c
	}
	return name
}

// sortedKeys renders a key set deterministically for diagnostics.
func sortedShapeKeys(m map[string]string) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
