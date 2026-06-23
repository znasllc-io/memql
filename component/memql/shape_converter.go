package memql

// shape_converter.go bridges the langparser's *ast.ShapeDecl AST node
// (introduced by memql#315 / sub-epic #309 / #306 child C) to the
// memql package's *ShapeDefinition registry type. The conversion is
// the load-time path the unified-kinds loader uses; the hand-rolled
// parseShapeMemQL (shape_parser.go) is unreferenced from production
// after this child lands but kept for its tests until sub-epic #306
// child D's final deletion.
//
// Semantics mirror parseShapeMemQL one-for-one:
//
//   * Annotation surface: @description, @row, @actor, @useConcept(...).
//     Unknown / retired (@concepts, @caller) annotations hard-reject.
//   * Body path translation: row.X -> X, row.payload.X -> payload.X,
//     <conceptBareName>.X -> payload.X, actor.X stays.
//   * Template key: path's terminal identifier segment.
//   * Validation: every @useConcept(name) entry must be referenced by
//     at least one body path (signature-bound concepts are exempt).

import (
	"fmt"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// shapeDeclToShapeDefinition converts a langparser ShapeDecl into the
// engine's ShapeDefinition registry type. Returns an error matching
// parseShapeMemQL's surface on annotation / body issues so the
// loader's diagnostic messages stay identical across the migration.
func shapeDeclToShapeDefinition(decl *languageParser.ShapeDecl, origin string) (*ShapeDefinition, error) {
	if decl == nil {
		return nil, fmt.Errorf("shape decl is nil")
	}
	if strings.TrimSpace(decl.Name) == "" {
		return nil, fmt.Errorf("%s: shape name is required", origin)
	}

	var description string
	var kindRow, kindActor bool
	var useConcepts []string

	// Process annotations. The order in decl.Attributes is the
	// author-side declaration order (each entry preserves source
	// position via the parser's append pass).
	for _, attr := range decl.Attributes {
		switch attr.Name {
		case "description":
			val, ok := attr.Value.(string)
			if !ok {
				return nil, fmt.Errorf("%s: @description expects a string value", origin)
			}
			description = val

		case "row":
			kindRow = true

		case "actor":
			kindActor = true

		case "useConcept":
			// `@useConcept(name1, name2, ...)` -- the langparser stores
			// each bare identifier as a key in Args with value `true`.
			// UseTargets() returns the keys alphabetically sorted; we
			// preserve that ordering on the ShapeDefinition (the
			// downstream registry doesn't depend on declaration order
			// and a stable sort makes diagnostic output deterministic).
			useConcepts = append(useConcepts, attr.UseTargets()...)

		case "concepts":
			// Retired in Phase G.3.g; mirror the migration hint
			// parseShapeMemQL emits.
			return nil, fmt.Errorf("%s: `@concepts(\"v1:...\")` is retired -- bind the shape via `@useConcept(<bareName>)` (e.g. `@useConcept(space)`); the loader resolves bare names against the registry", origin)

		case "caller":
			// Retired in #221; mirror the same migration hint.
			return nil, fmt.Errorf("%s: @caller is retired (#221) -- use @actor; the field accessor renamed at the same time (caller.X -> actor.X)", origin)

		default:
			return nil, fmt.Errorf("%s: unknown shape annotation @%s -- the supported surface is @description, @row, @actor, @useConcept", origin, attr.Name)
		}
	}

	// Signature-bound concept (`shape <Concept> <name> { ... }`) is
	// appended to the canonical useConcepts list so downstream
	// consumers see one place for "concepts this shape binds." It is
	// tracked separately for the must-be-referenced-in-body validator
	// below -- signature-bound concepts are exempt from that check
	// because the body uses `payload.X` directly post-migration.
	signatureConcept := decl.SignatureConcept
	if signatureConcept != "" {
		// Prepend so the signature concept reads first; mirrors the
		// hand-rolled parser's ordering.
		useConcepts = append([]string{signatureConcept}, useConcepts...)
	}

	// Empty body. C5 (memql#2035): a signature-bound shape with no body
	// defaults to projecting every projectable field of its concept --
	// "concept is the single source of truth for fields." We can't
	// enumerate the concept here (the converter has no registry); emit a
	// DefaultProjection marker with an empty Template and let
	// expandDefaultShapeProjections fill it once concepts are loaded.
	// An empty body WITHOUT a signature concept stays an error (there's
	// no concept to derive fields from -- e.g. an @actor-only shape).
	if len(decl.Paths) == 0 {
		if signatureConcept == "" {
			return nil, fmt.Errorf("%s: shape %q has no body fields and no signature concept -- an empty-body shape must bind a concept (`shape <Concept> %s { }`) so the default projection can be derived", origin, decl.Name, decl.Name)
		}
		// Reject stray @useConcept on a default-projection shape: with no
		// body, those names can never be referenced (the unused-concept
		// rule below would reject them anyway, but a targeted message is
		// clearer).
		for _, name := range useConcepts {
			if name != signatureConcept {
				return nil, fmt.Errorf("%s: shape %q: @useConcept(%s) declared on an empty-body (default-projection) shape, but an empty body references nothing -- drop the @useConcept or add an explicit body", origin, decl.Name, name)
			}
		}
		return &ShapeDefinition{
			Name:              decl.Name,
			Description:       description,
			Template:          map[string]any{},
			Origin:            origin,
			KindRow:           true,
			KindActor:         kindActor,
			UseConcepts:       useConcepts,
			DefaultProjection: true,
		}, nil
	}
	template := make(map[string]any, len(decl.Paths))
	usedConcepts := make(map[string]bool, len(useConcepts))

	for _, path := range decl.Paths {
		storedPath := translateShapeBodyPath(path, useConcepts)
		// Record which @useConcept name(s) got referenced via the
		// `<name>.X` prefix form. Signature-bound concepts don't go
		// through this surface (the body uses `payload.X` directly),
		// so this only fires for annotation-declared concepts.
		if idx := strings.Index(path, "."); idx > 0 {
			head := path[:idx]
			for _, c := range useConcepts {
				if head == c {
					usedConcepts[c] = true
					break
				}
			}
		}
		key := pathTerminalKey(storedPath)
		if key == "" {
			return nil, fmt.Errorf("%s: shape field path %q has no usable terminal key (must end in a simple identifier)", origin, path)
		}
		template[key] = `node(\"` + storedPath + `\")`
	}

	// Validate: every annotation-declared @useConcept name must be
	// referenced by at least one body path. The signature-bound
	// concept is exempt (handled above).
	for _, name := range useConcepts {
		if name == signatureConcept {
			continue
		}
		if !usedConcepts[name] {
			return nil, fmt.Errorf("%s: shape %q: @useConcept(%s) declared but %s is never referenced in the body", origin, decl.Name, name, name)
		}
	}

	return &ShapeDefinition{
		Name:        decl.Name,
		Description: description,
		Template:    template,
		Origin:      origin,
		KindRow:     kindRow,
		KindActor:   kindActor,
		UseConcepts: useConcepts,
	}, nil
}
