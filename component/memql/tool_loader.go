package memql

import (
	"encoding/json"
	"log/slog"
)

// Pass 3 of the DSL restructure migration: the legacy walk over the
// dsl/v1/tools/ embed FS is retired. loadToolRegistry now returns
// an empty registry; LoadUnifiedTools (in unified_kinds_loader.go)
// fills it from the new dsl/<domain>/tools.memql tree.

// loadToolRegistry returns an empty ToolRegistry. The actual tool
// loading happens via LoadUnifiedTools called from engine.go.
func loadToolRegistry(logger *slog.Logger) (*ToolRegistry, error) {
	_ = logger
	return newToolRegistry(), nil
}

// ToolDefinitionToMCP converts an internal Tool to MCP-compatible format.
// This is used when returning tools via the ListTools gRPC call.
func ToolDefinitionToMCP(tool *Tool) map[string]any {
	if tool == nil {
		return nil
	}

	mcp := map[string]any{
		"name":        tool.Name,
		"description": tool.Description,
	}

	// Parse inputSchema from raw JSON to include in the response
	if len(tool.InputSchema) > 0 {
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err == nil {
			mcp["inputSchema"] = schema
		}
	}

	return mcp
}

// ToolListToMCP converts a list of tools to MCP-compatible format.
func ToolListToMCP(tools []*Tool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}

	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if mcp := ToolDefinitionToMCP(tool); mcp != nil {
			result = append(result, mcp)
		}
	}

	return result
}
