package memql

// mcp_resources.go -- engine support for the MCP resources + prompts surface
// (MCP epic memql#1529 Phase 6 #1536, design §6.4). Concept schemas are exposed
// as MCP resources (uri memql://concept/<id>); reading one runs a rows query.
// resources/subscribe rides the engine event bus (graph.node.*.<id>) so a
// concept's changes push update notifications. DSL prompts map onto MCP prompts.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/events"
)

// mcpConceptURIPrefix is the MCP resource uri scheme for concept resources.
const mcpConceptURIPrefix = "memql://concept/"

// MCPConceptResources lists every loaded concept as an MCP resource descriptor.
// The uri encodes the concept id; name/description come from the concept; the
// payload is JSON (the concept's rows on read).
func (e *MemQLEngine) MCPConceptResources() []map[string]any {
	if e.concepts == nil {
		return nil
	}
	concepts := e.concepts.List()
	out := make([]map[string]any, 0, len(concepts))
	for _, c := range concepts {
		if c == nil || c.Name == "" {
			continue
		}
		desc := c.Description
		if strings.TrimSpace(desc) == "" {
			desc = "Rows of the " + c.Name + " concept."
		}
		out = append(out, map[string]any{
			"uri":         mcpConceptURIPrefix + c.Name,
			"name":        c.Name,
			"description": desc,
			"mimeType":    "application/json",
		})
	}
	return out
}

// mcpConceptIDFromURI extracts the concept id from a concept resource uri, or
// "" if the uri is not a concept resource.
func mcpConceptIDFromURI(uri string) string {
	if !strings.HasPrefix(uri, mcpConceptURIPrefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(uri, mcpConceptURIPrefix))
}

// MCPReadConceptResource reads a concept resource: it runs a rows query for the
// concept the uri names and returns the result payload as JSON text. The read
// runs under whatever auth envelope ctx carries (the per-row authz model
// applies), so a caller only sees rows it is allowed to.
func (e *MemQLEngine) MCPReadConceptResource(ctx context.Context, uri string) (string, error) {
	id := mcpConceptIDFromURI(uri)
	if id == "" {
		return "", fmt.Errorf("not a concept resource uri: %q", uri)
	}
	if e.concepts != nil {
		if _, err := e.concepts.Get(id); err != nil {
			return "", fmt.Errorf("unknown concept %q", id)
		}
	}
	res, err := e.Execute(ctx, "concept=="+id)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(res.OutputPayload())
	if err != nil {
		return "", fmt.Errorf("encode concept rows: %w", err)
	}
	return string(payload), nil
}

// MCPSubscribeConceptResource subscribes to a concept resource's changes and
// invokes onUpdate on every create/update/delete of a row of that concept. It
// returns an unsubscribe func; a non-concept uri is an error. The subscription
// rides the engine event bus, kept alive by the MCP session until it
// unsubscribes (or the connection closes).
func (e *MemQLEngine) MCPSubscribeConceptResource(uri string, onUpdate func()) (func(), error) {
	id := mcpConceptIDFromURI(uri)
	if id == "" {
		return nil, fmt.Errorf("not a concept resource uri: %q", uri)
	}
	bus := e.EventBus()
	if bus == nil {
		return nil, fmt.Errorf("event bus not available: resource subscriptions are unsupported in this deployment")
	}
	// graph.node.<action>.<conceptId>: "*" matches the action segment, so this
	// one pattern covers created / updated / deleted for the concept.
	pattern := "graph.node.*." + id
	unsubscribe := bus.Subscribe(pattern, func(_ events.Event) {
		if onUpdate != nil {
			onUpdate()
		}
	})
	return unsubscribe, nil
}

// MCPPromptDefinitions lists every DSL prompt as an MCP prompt descriptor
// ({name, description, arguments}). (Phase 6 #1536)
func (e *MemQLEngine) MCPPromptDefinitions() []map[string]any {
	if e.prompts == nil {
		return nil
	}
	prompts := e.prompts.List()
	out := make([]map[string]any, 0, len(prompts))
	for _, p := range prompts {
		if p == nil || p.Name == "" {
			continue
		}
		desc := p.Description
		if strings.TrimSpace(desc) == "" {
			desc = "The " + p.Name + " prompt."
		}
		out = append(out, map[string]any{
			"name":        p.Name,
			"description": desc,
			"arguments":   p.Arguments(),
		})
	}
	return out
}

// MCPGetPrompt renders a DSL prompt with the given arguments and returns the
// rendered text (the MCP prompts/get message body). An unknown prompt is an
// error.
func (e *MemQLEngine) MCPGetPrompt(name string, args map[string]any) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("prompt name is required")
	}
	if e.prompts != nil {
		if _, ok := e.prompts.Get(name); !ok {
			return "", fmt.Errorf("unknown prompt %q", name)
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	return e.RenderPrompt(name, args)
}
