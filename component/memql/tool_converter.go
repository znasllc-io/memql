package memql

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
)

// toolDeclToTool translates the langparser-produced *ast.ToolDecl
// into the *Tool the registry stores. Mirrors what the legacy
// parseToolMemQL produced: a JSON-Schema input descriptor compiled
// from the field list, a ToolHandler dispatched by handler-type,
// and a ToolAnnotations block when any of destructive /
// requiresConfirmation / executionTime / rateLimit was declared.
//
// Returns ([]*Tool, error) -- always exactly one entry today --
// to match the legacy parseToolMemQL signature LoadUnifiedTools'
// baseloader.LoadMany pipeline already wires through.
func toolDeclToTool(decl *ast.ToolDecl, origin string) ([]*Tool, error) {
	if decl == nil {
		return nil, fmt.Errorf("tool declaration is nil")
	}

	properties := make(map[string]any, len(decl.Fields))
	var required []string
	var autoInjected []string

	for _, f := range decl.Fields {
		prop := map[string]any{}

		switch strings.ToLower(f.Type) {
		case "string":
			prop["type"] = "string"
		case "number", "float":
			prop["type"] = "number"
		case "integer", "int":
			prop["type"] = "integer"
		case "bool", "boolean":
			prop["type"] = "boolean"
		case "object":
			prop["type"] = "object"
		case "array":
			prop["type"] = "array"
			// OpenAI's function-tool schema requires array properties
			// to specify "items". A DSL tool field can't declare an
			// item type, so emit a permissive empty schema ({}) rather
			// than forcing {"type":"object"}. The old object default
			// made every scalar-array tool uncallable -- e.g.
			// notesCreate.tags (a list of string tags) rejected its
			// strings with `items/type: expected object, but got
			// string` (#1606). An empty items schema accepts scalars
			// and objects alike.
			prop["items"] = map[string]any{}
		default:
			// Unknown type -- fall back to string. Mirrors the
			// legacy parser's default; an unknown type isn't a hard
			// error so a typo in a fixture or future-only type slips
			// through as a permissive string instead of breaking the
			// loader.
			prop["type"] = "string"
		}

		if f.Description != "" {
			prop["description"] = f.Description
		}
		if len(f.EnumValues) > 0 {
			enumAny := make([]any, len(f.EnumValues))
			for i, v := range f.EnumValues {
				enumAny[i] = v
			}
			prop["enum"] = enumAny
		}
		if f.Default != "" {
			prop["default"] = f.Default
		}

		properties[f.Name] = prop

		if f.Required {
			required = append(required, f.Name)
		}
		if f.AutoInjected {
			autoInjected = append(autoInjected, f.Name)
		}
	}

	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}

	schemaJSON, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("tool %q: marshal inputSchema: %w", decl.Name, err)
	}

	tool := &Tool{
		Name:               decl.Name,
		Description:        decl.Description,
		InputSchema:        json.RawMessage(schemaJSON),
		AutoInjectedFields: autoInjected,
		ClientExecution:    decl.ClientExecution,
		Origin:             origin,
	}
	if len(decl.AllowedRoles) > 0 {
		tool.AllowedRoles = append([]string(nil), decl.AllowedRoles...)
	}
	if len(decl.Scopes) > 0 {
		tool.Scopes = append([]string(nil), decl.Scopes...)
	}

	// Handler. Match the legacy mapping of @handler args to
	// ToolHandler fields per handler type. The "delegate" handler
	// kind passes through with just a Type populated.
	if decl.HandlerType != "" {
		handler := &ToolHandler{Type: decl.HandlerType}
		switch decl.HandlerType {
		case "function":
			handler.FunctionName = decl.HandlerName
		case "query":
			handler.Query = decl.HandlerName
		case "webhook":
			handler.URL = decl.HandlerURL
			if decl.HandlerMethod != "" {
				handler.Method = decl.HandlerMethod
			} else {
				handler.Method = "POST"
			}
		}
		tool.Handler = handler
	}

	// Annotations. Materialise the block only when at least one
	// annotation actually fires; otherwise leave it nil so the JSON
	// representation stays clean (matches legacy behaviour).
	if decl.Destructive || decl.RequiresConfirmation || decl.ExecutionTime != "" || decl.RateLimitMaxCalls > 0 {
		ann := &ToolAnnotations{
			Destructive:          decl.Destructive,
			RequiresConfirmation: decl.RequiresConfirmation,
			ExecutionTime:        decl.ExecutionTime,
		}
		if decl.RateLimitMaxCalls > 0 {
			ann.RateLimit = &ToolRateLimit{
				MaxCalls:      decl.RateLimitMaxCalls,
				PeriodSeconds: decl.RateLimitPeriod,
			}
		}
		tool.Annotations = ann
	}

	return []*Tool{tool}, nil
}
