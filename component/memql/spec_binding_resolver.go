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
	languageParser "github.com/znasllc-io/memql/component/language/parser"
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
		// The QUALIFIED key (memql#3897). Writing the resolved spec back under
		// its bare name would leave the namespaced entry unresolved and add a
		// second, shadowing one -- so every binding would silently fail to
		// stick while the registry reported two specs where there is one.
		key := QualifyConstruct(ConstructNamespaceForOrigin(spec.Origin), spec.Name)
		if err := specs.Upsert(key, spec); err != nil {
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
		if shape, ok := specBindingShape(shapes, spec); ok {
			kind = shapeSpecKind(shape)
			mapper, bindErr = shapeFieldMapper(spec.BoundName, shape)
			if bindErr != nil {
				return bindErr
			}
		} else if concept, err := specBindingConcept(concepts, spec); err == nil && concept != nil {
			kind = SpecKindRow
			mapper = conceptFieldMapper(concept)
		} else {
			return specBindingRefusal(spec, shapes, concepts)
		}
	}

	// An edition-2026 body (memql#5366) is NOT rewritten here: it has no Expr
	// yet, and when it does it will not need one -- Lower emits the final
	// access forms (`payload.<f>`, the canonical intrinsic, the shape's stored
	// path) directly, and its fields are read through the lambda parameter,
	// never bare, so there is no bare field for a mapper to find. Its binding
	// is resolved (the mapper above refuses a binding that does not resolve)
	// and its kind is set, which is what the Init pass lowers it against.
	spec.Kind = kind
	if spec.Lambda != nil {
		return nil
	}
	if err := rewriteSpecFields(spec.Expr, mapper); err != nil {
		return err
	}
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

// specBindingShape and specBindingConcept resolve a spec's bound name the way
// a query's signature concept resolves (memql#5366):
//
//  1. through the spec's file-top `use` imports -- an import that names the
//     bound name decides what it is and where, as it does for a query
//     (resolveUseDeclarations: the import's first segment is the namespace
//     hint). This is the only step an AUTHORED spec's stored row can repeat at
//     re-hydration, because its source is all that is stored: a bundle's spec
//     slice carries the bundle's import preamble, so the row does too, and the
//     boot recompile binds exactly as the author's define did;
//  2. in the spec's OWN domain -- a shape the domain declares, a concept of
//     the domain's namespace, the way a query's signature concept resolves
//     ambiently (resolveBareConceptNameWithNamespace). Without it a bound name
//     two domains both declare resolved to neither (memql#5369, found by the
//     conformance corpus). An authored spec has no own domain -- its origin
//     names no directory -- so it never depends on this step to bind;
//  3. across the whole tree, by a unique name.
//
// A name several domains declare, with no import to choose, is refused naming
// the imports that would (specBindingRefusal) -- at define and at promote,
// never passed there to be quarantined at the next boot.
func specBindingShape(shapes *ShapeRegistry, spec *Spec) (*ShapeDefinition, bool) {
	if shapes == nil || spec == nil {
		return nil, false
	}
	if use, source, ok := specBindingImport(spec); ok {
		if importKind(use) != "shapes" {
			// Imported as something else -- a concept: not a shape, whatever
			// shape happens to share the name.
			return nil, false
		}
		if shape, ok := shapes.Get(QualifyConstruct(importNamespace(use), source)); ok {
			return shape, true
		}
		return shapeLookup(shapes, source)
	}
	if ns := ConstructNamespaceForOrigin(spec.Origin); ns != "" {
		if shape, ok := shapes.Get(QualifyConstruct(ns, spec.BoundName)); ok {
			return shape, true
		}
	}
	return shapeLookup(shapes, spec.BoundName)
}

// specBindingConcept is specBindingShape's concept half.
func specBindingConcept(concepts memoryNodes.Registry, spec *Spec) (*memoryNodes.Concept, error) {
	if spec == nil {
		return nil, fmt.Errorf("no spec to resolve a binding for")
	}
	if concepts == nil {
		return nil, fmt.Errorf("no concept registry to resolve binding %q in", spec.BoundName)
	}
	if use, source, ok := specBindingImport(spec); ok {
		if importKind(use) != "concepts" {
			return nil, fmt.Errorf("binding %q is imported from %s, which is not a concepts module", spec.BoundName, use.Path)
		}
		nsHint := ""
		if len(use.Parts) > 0 {
			nsHint = use.Parts[0]
		}
		id, err := NewConceptResolver(concepts).resolveBareConceptNameWithNamespace(source, nsHint)
		if err != nil {
			return nil, fmt.Errorf("binding %q imported from %s: %w", spec.BoundName, use.Path, err)
		}
		return concepts.Get(id)
	}
	if ns := strings.ReplaceAll(ConstructNamespaceForOrigin(spec.Origin), "/", ":"); ns != "" {
		var own *memoryNodes.Concept
		found := 0
		for _, c := range concepts.List() {
			if c == nil || idNamespace(c.Name) != ns {
				continue
			}
			if i := strings.LastIndex(c.Name, ":"); i >= 0 && c.Name[i+1:] == spec.BoundName {
				own = c
				found++
			}
		}
		if found == 1 {
			return own, nil
		}
	}
	return resolveConceptByTrailingSegment(concepts, spec.BoundName)
}

// specBindingImport finds the file-top import that brings a spec's bound name
// into scope -- `use lowertwin.concepts.{ ticket }` for `spec ticket ...`, or
// `{ card as ticket }` -- returning the import and the name it imports under
// (the SOURCE name, for an aliased one). ok is false when no import names it.
func specBindingImport(spec *Spec) (*languageParser.UseDeclaration, string, bool) {
	if spec == nil || strings.TrimSpace(spec.BoundName) == "" {
		return nil, "", false
	}
	for _, use := range spec.Uses {
		if use == nil {
			continue
		}
		for _, source := range use.Names {
			if use.LocalNameFor(source) == spec.BoundName {
				return use, source, true
			}
		}
	}
	return nil, "", false
}

// importKind is the construct kind a Form B import names -- the last segment
// of its path: "concepts", "shapes", ...
func importKind(use *languageParser.UseDeclaration) string {
	if use == nil || len(use.Parts) == 0 {
		return ""
	}
	return use.Parts[len(use.Parts)-1]
}

// importNamespace is the namespace a Form B import names its constructs in:
// the path before the kind segment, as a namespace ("agents.tools.shapes" ->
// "agents/tools").
func importNamespace(use *languageParser.UseDeclaration) string {
	if use == nil || len(use.Parts) < 2 {
		return ""
	}
	return strings.Join(use.Parts[:len(use.Parts)-1], "/")
}

// specBindingRefusal explains a binding that resolved to nothing. When no
// import names the bound name and more than one domain declares it -- a
// concept of that name in several namespaces, or a shape -- the refusal says so
// and names the imports that would choose, rather than claiming the name
// resolves to nothing: it resolves to too much.
func specBindingRefusal(spec *Spec, shapes *ShapeRegistry, concepts memoryNodes.Registry) error {
	name := spec.BoundName
	if _, _, imported := specBindingImport(spec); !imported {
		seen := map[string]bool{}
		var imports []string
		add := func(path, kind string) {
			line := "`use " + path + "." + kind + ".{ " + name + " }`"
			if path != "" && !seen[line] {
				seen[line] = true
				imports = append(imports, line)
			}
		}
		if concepts != nil {
			for _, c := range concepts.List() {
				if c == nil {
					continue
				}
				if i := strings.LastIndex(c.Name, ":"); i >= 0 && c.Name[i+1:] == name {
					add(strings.ReplaceAll(idNamespace(c.Name), ":", "."), "concepts")
				}
			}
		}
		if shapes != nil {
			for _, sh := range shapes.List() {
				if sh != nil && sh.Name == name {
					add(strings.ReplaceAll(ConstructNamespaceForOrigin(sh.Origin), "/", "."), "shapes")
				}
			}
		}
		if len(imports) > 1 {
			sort.Strings(imports)
			return fmt.Errorf("binding %q is declared by more than one domain, and no file-top `use` import says which -- import the one you mean: %s", name, strings.Join(imports, " or "))
		}
	}
	return fmt.Errorf("binding %q resolves to neither an imported shape nor a concept -- check the file-top `use` import (use ...shapes.{ %s } for a shape, use ...concepts.{ %s } for a concept)", name, name, name)
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
		// A comparison on a collection ELEMENT reads the element, not the
		// bound surface: `$elem.qty` is not a field of the bound concept or
		// shape, and mapping it would either refuse a valid body or prefix it
		// into a payload path no row has.
		if isArrayElementField(node.Field) {
			return nil
		}
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
	case *NotExpression:
		return rewriteSpecFields(node.Target, mapper)
	case *ArrayPredicateExpression:
		// The ARRAY is a field of the bound surface and maps like any other;
		// a nested predicate's array is under the enclosing element and does
		// not.
		if !isArrayElementField(node.Field) {
			newRef, err := mapper(node.Field)
			if err != nil {
				return err
			}
			node.Field = newRef
		}
		return rewriteSpecFields(node.Pred, mapper)
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
