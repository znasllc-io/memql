package memoryNodes

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/visionarys-io/memql/component/provenance"
)

// provenanceJSONFromContext pulls the provenance.Provenance value
// out of ctx, validates it, and marshals it to JSON for storage on
// the MemoryNode row.
//
// Returns an error when no provenance was set or when the value
// fails validation -- the engine enforces NOT NULL provenance at
// the row-construction layer so writes from un-wired call paths
// fail loudly instead of stamping garbage. Genesis-mode: no silent
// default, every originating context must declare its intent via
// provenance.ContextWithProvenance before the write.
func provenanceJSONFromContext(ctx context.Context) (json.RawMessage, error) {
	prov := provenance.FromContext(ctx)
	if prov.IsZero() {
		return nil, fmt.Errorf("no provenance in context (every write must call provenance.ContextWithProvenance before reaching the insert path)")
	}
	if err := prov.Validate(); err != nil {
		return nil, err
	}
	out, err := json.Marshal(prov)
	if err != nil {
		return nil, fmt.Errorf("marshal provenance: %w", err)
	}
	return out, nil
}
