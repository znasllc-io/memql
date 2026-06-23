package memql

import (
	"fmt"
	"log/slog"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql/baseregistry"
)

// Pass 3 of the DSL restructure migration: the legacy walk over
// dsl/v1/shapes/ is retired. loadEmbeddedShapes returns an empty
// registry; LoadUnifiedShapes (unified_kinds_loader.go) fills it
// from dsl/<domain>/<entity>.memql via slice extraction.

// ShapeDefinition represents a shape template loaded from
// shapes/v1/*.{json,memql}.
type ShapeDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Template    map[string]any `json:"template"`
	Origin      string         `json:"-"` // file path for debugging

	// KindRow / KindActor declare the shape's source universe.
	KindRow   bool `json:"kindRow,omitempty"`
	KindActor bool `json:"kindActor,omitempty"`

	// UseConcepts holds the bare concept names declared via
	// `@useConcept(name, ...)`.
	UseConcepts []string `json:"useConcepts,omitempty"`

	// DefaultProjection marks a signature-bound @row shape that was
	// authored with an EMPTY body (`shape <Concept> name { }`). Its
	// Template is filled at engine bootstrap (expandDefaultShapeProjections)
	// with every PROJECTABLE field of the bound concept -- all declared
	// fields except those marked @internal. This is the C5 (memql#2035)
	// "concept is the single source of truth for fields" path: a shape
	// no longer re-declares the concept's field list.
	DefaultProjection bool `json:"defaultProjection,omitempty"`
}

// ShapeRegistry holds all loaded shape templates. Unlike the
// Function / Tool / Spec registries this one does NOT clone its
// entries on egress -- shape definitions are read-only after load
// and consumers expect pointer identity for caching.
type ShapeRegistry struct {
	*baseregistry.Registry[ShapeDefinition]
}

// newShapeRegistry creates an empty shape registry.
func newShapeRegistry() *ShapeRegistry {
	return &ShapeRegistry{
		Registry: baseregistry.New[ShapeDefinition]("shape", nil, nil),
	}
}

// Upsert inserts or replaces a shape registration. Used by the
// unified loader so duplicate registrations don't error.
func (r *ShapeRegistry) Upsert(shape *ShapeDefinition) error {
	if shape == nil {
		return fmt.Errorf("shape is nil")
	}
	return r.Registry.Upsert(shape.Name, shape)
}

// add registers a shape definition (error on duplicate).
func (r *ShapeRegistry) add(shape *ShapeDefinition) error {
	if shape == nil {
		return fmt.Errorf("shape is nil")
	}
	return r.Registry.Add(shape.Name, shape)
}

// Get retrieves a shape by name. Returns (nil, false) when not
// found -- the canonical comma-ok form, kept on the wrapper so
// existing callers don't need to rewrite to the error form.
func (r *ShapeRegistry) Get(name string) (*ShapeDefinition, bool) {
	if r == nil {
		return nil, false
	}
	return r.Registry.Lookup(name)
}

// loadEmbeddedShapes returns an empty registry. The actual shape
// loading happens via LoadUnifiedShapes called from engine.go.
func loadEmbeddedShapes(logger *slog.Logger, conceptRegistry memoryNodes.Registry) (*ShapeRegistry, error) {
	_ = logger
	_ = conceptRegistry
	return newShapeRegistry(), nil
}

// expandDefaultShapeProjections fills the Template of every
// DefaultProjection shape (an empty-body, signature-bound `shape
// <Concept> name { }`) with one entry per PROJECTABLE field of its
// bound concept -- all declared fields except those marked @internal.
// This is the C5 (memql#2035) "concept is the single source of truth
// for fields" path: a shape no longer re-states the concept's field
// list; the empty body says "project everything safe to expose."
//
// Runs at engine bootstrap, AFTER both concepts and shapes are loaded.
// Resolution failures (unknown / ambiguous concept, no projectable
// fields) are logged and the shape is left with an empty template
// rather than aborting the whole load -- mirroring the rest of the
// unified-loader pass's degrade-don't-crash posture.
//
// The returned count is the number of shapes successfully expanded.
func expandDefaultShapeProjections(logger *slog.Logger, shapes *ShapeRegistry, concepts memoryNodes.Registry) int {
	if shapes == nil || concepts == nil {
		return 0
	}
	expanded := 0
	for _, shape := range shapes.List() {
		if shape == nil || !shape.DefaultProjection {
			continue
		}
		// The signature concept is the first UseConcepts entry (the
		// converter prepends it). Without one there is nothing to
		// derive from -- the converter should never emit a
		// DefaultProjection shape in that state, but guard anyway.
		if len(shape.UseConcepts) == 0 {
			if logger != nil {
				logger.Warn("memql.shapeDefaultProjection: default-projection shape has no signature concept; leaving empty",
					"shape", shape.Name, "origin", shape.Origin)
			}
			continue
		}
		bare := shape.UseConcepts[0]
		concept, err := resolveConceptByTrailingSegment(concepts, bare)
		if err != nil {
			if logger != nil {
				logger.Warn("memql.shapeDefaultProjection: cannot resolve bound concept; leaving shape empty",
					"shape", shape.Name, "concept", bare, "origin", shape.Origin, "error", err)
			}
			continue
		}
		fields := concept.ProjectableFields()
		if len(fields) == 0 {
			if logger != nil {
				logger.Warn("memql.shapeDefaultProjection: bound concept has no projectable fields; leaving shape empty",
					"shape", shape.Name, "concept", concept.Name, "origin", shape.Origin)
			}
			continue
		}
		template := make(map[string]any, len(fields))
		for _, field := range fields {
			// Mirror the storage form the converter writes for an
			// explicit `row.payload.<field>` path: a node("payload.X")
			// call string with backslash-escaped quotes.
			template[field] = `node(\"payload.` + field + `\")`
		}
		shape.Template = template
		if err := shapes.Upsert(shape); err != nil && logger != nil {
			logger.Warn("memql.shapeDefaultProjection: re-upsert failed",
				"shape", shape.Name, "error", err)
		}
		expanded++
		if logger != nil {
			logger.Debug("memql.shapeDefaultProjection: expanded",
				"shape", shape.Name, "concept", concept.Name, "fields", len(fields))
		}
	}
	if logger != nil && expanded > 0 {
		logger.Info("memql.shapeDefaultProjection: expanded default-projection shapes",
			"component", "memql.engine", "count", expanded)
	}
	return expanded
}

// resolveConceptByTrailingSegment resolves a bare concept name (e.g.
// "space") to its registered Concept by matching the trailing segment
// of each canonical id (the part after the last ':'). Mirrors
// ConceptResolver.resolveBareConceptName but works directly off the
// memoryNodes.Registry so the shape-expansion pass doesn't need a
// full resolver. An ambiguous trailing segment is an error (the
// signature-bound shape carries no namespace hint to disambiguate).
func resolveConceptByTrailingSegment(registry memoryNodes.Registry, bare string) (*memoryNodes.Concept, error) {
	bare = strings.TrimSpace(bare)
	if bare == "" {
		return nil, fmt.Errorf("empty concept name")
	}
	// Exact-name fast path (already-canonical ids).
	if c, err := registry.Get(bare); err == nil && c != nil {
		return c, nil
	}
	var matches []*memoryNodes.Concept
	for _, c := range registry.List() {
		if c == nil {
			continue
		}
		idx := strings.LastIndex(c.Name, ":")
		if idx < 0 {
			continue
		}
		if c.Name[idx+1:] == bare {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no registered concept has trailing segment %q", bare)
	case 1:
		return matches[0], nil
	default:
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.Name)
		}
		return nil, fmt.Errorf("ambiguous concept name %q matches %d concepts: %s", bare, len(matches), strings.Join(names, ", "))
	}
}
