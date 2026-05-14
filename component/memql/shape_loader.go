package memql

import (
	"fmt"
	"log/slog"

	memoryNodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	"github.com/visionarys-io/memql/component/memql/baseregistry"
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

	// KindRow / KindCaller declare the shape's source universe.
	KindRow    bool `json:"kindRow,omitempty"`
	KindCaller bool `json:"kindCaller,omitempty"`

	// UseConcepts holds the bare concept names declared via
	// `@useConcept(name, ...)`.
	UseConcepts []string `json:"useConcepts,omitempty"`
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
