package memql

import (
	"fmt"
	"log/slog"
)

// loadEmbeddedSpecs returns an empty registry. The legacy walk over
// dsl/v1/specs/ was retired in Pass 3 of the DSL restructure, and
// the dedicated `parseSpecMemQL` + `LoadUnifiedSpecs` pair (in
// spec_parser.go + unified_spec_loader.go) is now the only path
// that populates the SpecRegistry.
func loadEmbeddedSpecs(logger *slog.Logger, schemaIdx *schemaIndex) (*SpecRegistry, error) {
	if schemaIdx == nil {
		return nil, fmt.Errorf("schema index is required to load specs")
	}
	_ = logger
	return newSpecRegistry(), nil
}

// ValidateSpecBindings walks every registered spec and verifies its
// @useShape(N) reference resolves to a shape in the shape registry.
// (@useConcept(N) is already validated against the concept registry
// at spec parse time -- the validator only needs to confirm shape
// references here.)
//
// Returns the first unresolved binding as an error. Called from the
// engine bootstrap after both the spec and shape registries are
// populated.
func ValidateSpecBindings(specs *SpecRegistry, shapes *ShapeRegistry) error {
	if specs == nil || shapes == nil {
		return nil
	}
	for _, name := range specs.Names() {
		spec, err := specs.Get(name)
		if err != nil || spec == nil {
			continue
		}
		if spec.IsTrait {
			continue
		}
		if spec.UseShapeName == "" {
			continue
		}
		if _, ok := shapes.Get(spec.UseShapeName); !ok {
			return fmt.Errorf("spec %q binds @useShape(%s) but no such shape is registered", spec.Name, spec.UseShapeName)
		}
	}
	return nil
}
