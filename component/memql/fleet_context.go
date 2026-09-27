package memql

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/airoute"
)

// WithMinContextTokens binds a resolution's floor without changing the registry
// client or another resolution. Constructing a fresh provider also keeps its
// per-call bookkeeping and mutex independent.
func (p *fleetProvider) WithMinContextTokens(tokens int) any {
	return &fleetProvider{
		registry: p.registry, modelId: p.modelId, actingUserId: p.actingUserId,
		selector: p.selector, attributes: p.attributes,
		minContextTokens: max(p.minContextTokens, tokens),
		effort:           p.effort,
	}
}

// WithEffort binds a person's explicit effort for one call (epic memql#5414,
// design D20) the same way, carrying the floor forward so the order the router
// applies the two bindings in cannot drop one.
func (p *fleetProvider) WithEffort(effort string) any {
	return &fleetProvider{
		registry: p.registry, modelId: p.modelId, actingUserId: p.actingUserId,
		selector: p.selector, attributes: p.attributes,
		minContextTokens: p.minContextTokens,
		effort:           strings.TrimSpace(effort),
	}
}

// fleetWorkingContext is recalculated for each model step: tool results and
// assistant tool arguments grow after resolution, and schemas also occupy the
// runtime's window. Use the common estimate plus completion headroom, never the
// model's advertised maximum (which can reserve far more memory than needed).
func fleetWorkingContext(req FleetCallRequest) (int, error) {
	parts := make([]string, 0, len(req.Messages)*4+len(req.Tools)*3+3)
	for _, m := range req.Messages {
		parts = append(parts, m.Role, m.Content, m.Name, m.ToolCallId)
		for _, tc := range m.ToolCalls {
			parts = append(parts, tc.ID, tc.Name, tc.Arguments)
		}
	}
	for _, tool := range req.Tools {
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return 0, fmt.Errorf("fleet tool %s schema: %w", tool.Name, err)
		}
		parts = append(parts, tool.Name, tool.Description, string(schema))
	}
	if req.Schema != nil {
		parts = append(parts, req.Schema.Name, req.Schema.Description, string(req.Schema.Schema))
	}
	return airoute.EstimateMinContextTokensFor(0, parts...), nil
}
