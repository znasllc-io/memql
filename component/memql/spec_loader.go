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
