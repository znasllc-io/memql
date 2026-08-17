package memql

// shape_concept_validator.go is the REGISTRY-dependent half of the shape
// validation memql#3621 adds: it compares a shape body against the concept
// the shape binds. Nothing did that before -- shapeDeclToShapeDefinition has
// no registry and no later pass revisited it -- so a body could project a
// property the concept does not declare and the only symptom was a wire key
// whose value was null.
//
// Two live shapes were wrong when this landed, and they are the two shapes
// of the failure:
//
//   - dsl/workbench/shapes.memql workspaceFull wrote a bare `createdAt`. A
//     bare name is a PAYLOAD property (epic #2292), so it stored
//     `payload.createdAt` while the concept comment says the opposite in so
//     many words -- provisioning time is the row INTRINSIC. Both spellings
//     produce the wire key `createdAt`; only the value differs (nil vs the
//     real timestamp), which is why nobody noticed.
//   - dsl/identity/shapes.memql delegationFull projected `agentSubject`,
//     removed from the concept by memql#1630 but not from the shape. Four
//     delegation queries returned an always-null field, and the shape also
//     omitted the real, @required createdBySubject.
//
// The check runs at engine bootstrap (engine_bootstrap.go), after concepts
// and shapes are both loaded, and records each violation on the LoadReport
// so strict boot refuses rather than warns.

import (
	"fmt"
	"sort"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// shapeBindingViolation is one shape that does not agree with the concept it
// binds. Detail is the operator-facing sentence; Shape / Origin name the
// construct for the LoadReport entry.
type shapeBindingViolation struct {
	Shape  string
	Origin string
	Detail string
}

// resolveShapeBoundConcept resolves the concept a shape's signature binds
// (`shape <Concept> <name>`), disambiguating an ambiguous bare name with the
// shape's own domain -- the first path segment of its origin file.
//
// The hint matters, and it is the fix for the fourth hole in memql#3621. Four
// shapes in the tree bind a bare name that is ambiguous cluster-wide:
// forge/requestFull (`request`), planner/planFull (`plan`),
// telephony/callFull (`call`), worker/workerInvocationFull (`invocation`).
// With an explicit body nothing broke, because the binding was only ever
// consulted by expandDefaultShapeProjections -- so converting any of them to
// the empty-body default-projection form (memql#2035, the direction the tree
// is being pushed) yielded an EMPTY projection and a Warn line. Resolving
// through the domain is what the function loader already does for a
// signature concept (ConceptResolver.ResolveSignatureConceptInNamespace);
// shapes now get the same answer instead of a global trailing-segment scan
// that gives up on a collision.
func resolveShapeBoundConcept(concepts memoryNodes.Registry, shape *ShapeDefinition) (*memoryNodes.Concept, error) {
	if shape == nil {
		return nil, fmt.Errorf("shape is nil")
	}
	if concepts == nil {
		return nil, fmt.Errorf("concept registry not available")
	}
	if len(shape.UseConcepts) == 0 {
		return nil, fmt.Errorf("shape %q binds no concept", shape.Name)
	}
	bare := shape.UseConcepts[0]

	c, err := resolveConceptByTrailingSegment(concepts, bare)
	if err == nil {
		return c, nil
	}
	domain := NamespaceFromFilePath(shape.Origin)
	if domain == "" {
		return nil, err
	}
	id, hintErr := NewConceptResolver(concepts).resolveBareConceptNameWithNamespace(bare, domain)
	if hintErr != nil {
		return nil, err
	}
	hinted, getErr := concepts.Get(id)
	if getErr != nil || hinted == nil {
		return nil, err
	}
	return hinted, nil
}

// validateShapeConceptBindings reports every shape whose body disagrees with
// the concept it binds. Returns violations in shape-name order so the boot
// log and the strict-boot detail are deterministic.
//
// Two rules:
//
//  1. A signature-bound shape's concept MUST resolve. An unresolvable or
//     ambiguous binding was a Warn on the default-projection path and
//     nothing at all on the explicit-body path.
//  2. Every projected PAYLOAD property must be a declared field of that
//     concept. Row intrinsics (`row.X`, stored bare) and actor paths are out
//     of scope here -- the actor set is closed by validateShapeBody.
//
// An UNBOUND shape is skipped on purpose, not missed: the trait scaffolds in
// dsl/common/shapes.memql (activeRowTrait, statusRowTrait, ...) are
// deliberately concept-agnostic, which is the whole point of a trait.
func validateShapeConceptBindings(shapes *ShapeRegistry, concepts memoryNodes.Registry) []shapeBindingViolation {
	if shapes == nil || concepts == nil {
		return nil
	}
	all := shapes.List()
	sort.Slice(all, func(i, j int) bool {
		if all[i] == nil || all[j] == nil {
			return all[j] == nil
		}
		return all[i].Name < all[j].Name
	})

	var out []shapeBindingViolation
	for _, shape := range all {
		if shape == nil || len(shape.UseConcepts) == 0 {
			continue
		}
		bare := shape.UseConcepts[0]
		concept, err := resolveShapeBoundConcept(concepts, shape)
		if err != nil {
			out = append(out, shapeBindingViolation{
				Shape:  shape.Name,
				Origin: shape.Origin,
				Detail: fmt.Sprintf("binds concept %q, which does not resolve: %v", bare, err),
			})
			continue
		}

		// A default-projection shape has no authored body to check; its
		// projection IS the concept's projectable field list, so the only
		// way it can be wrong is by being empty.
		if shape.DefaultProjection {
			if len(concept.ProjectableFields()) == 0 {
				out = append(out, shapeBindingViolation{
					Shape:  shape.Name,
					Origin: shape.Origin,
					Detail: fmt.Sprintf("has an empty body (default projection) but bound concept %s declares no projectable field -- the shape would project nothing", concept.Name),
				})
			}
			continue
		}

		declared := make(map[string]bool, 32)
		for _, f := range concept.DeclaredFields() {
			declared[f] = true
		}
		for _, key := range sortedTemplateKeys(shape.Template) {
			stored, ok := shape.Template[key].(string)
			if !ok {
				continue
			}
			prop, isPayload := strings.CutPrefix(extractNodePath(stored), "payload.")
			if !isPayload {
				continue // row intrinsic (`row.X` stores bare) or actor path
			}
			head := prop
			if idx := strings.Index(prop, "."); idx > 0 {
				head = prop[:idx]
			}
			if declared[head] {
				continue
			}
			out = append(out, shapeBindingViolation{
				Shape:  shape.Name,
				Origin: shape.Origin,
				Detail: fmt.Sprintf("projects payload property %q, which concept %s does not declare.%s Declared fields: %s",
					head, concept.Name, shapeUnknownFieldHint(head), strings.Join(concept.DeclaredFields(), ", ")),
			})
		}
	}
	return out
}

// shapeUnknownFieldHint adds the one correction that is nearly always the
// right one: a bare name that is a ROW INTRINSIC was meant to carry the
// `row.` prefix. Both spellings produce the same wire key, so without the
// hint the author sees "createdAt is not declared" on a shape whose body
// plainly says `createdAt` and has to know epic #2292 to read it.
func shapeUnknownFieldHint(head string) string {
	if canonical, ok := canonicalIntrinsicFieldName(head); ok {
		return fmt.Sprintf(" %q is a ROW INTRINSIC -- a bare name in a shape body is a payload property (epic #2292), so write `row.%s`.", head, canonical)
	}
	return ""
}

// sortedTemplateKeys renders a template's keys deterministically.
func sortedTemplateKeys(template map[string]any) []string {
	out := make([]string, 0, len(template))
	for k := range template {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
