package memql

import (
	"encoding/json"
	"log/slog"
	"strings"
)

// registerFunctionTools generates MCP tools for enabled, non-internal functions and
// registers them into the provided ToolRegistry.
//
// Behavior:
// - Skips disabled functions
// - Skips internal functions
// - Skips if an explicit tool with the same name already exists (JSON-defined tools win)
// - Input schema is derived from the function's args block (ArgsSchemaConfig).
func registerFunctionTools(logger *slog.Logger, functions *FunctionRegistry, tools *ToolRegistry) {
	if functions == nil || tools == nil {
		return
	}

	snapshot := functions.Snapshot()
	if snapshot == nil {
		return
	}

	for _, fn := range snapshot {
		if fn == nil {
			continue
		}
		if !fn.Enabled || fn.Internal {
			continue
		}

		name := strings.TrimSpace(fn.Name)
		if name == "" {
			continue
		}

		// If a tool already exists with this name, do not override it.
		// A name marked @disabled at load is treated as occupied: generating
		// a tool from the backing function would resurrect the disabled tool
		// without its governance attributes (#2606).
		if tools.Has(name) || tools.IsDisabled(name) {
			continue
		}

		inputSchema, err := toolInputSchemaFromArgs(fn.ArgsSchema)
		if err != nil {
			if logger != nil {
				logger.Warn("failed to derive tool input schema from function args block; skipping function tool",
					"component", ComponentName,
					"function", name,
					"error", err.Error())
			}
			continue
		}

		tool := &Tool{
			Name:        name,
			Description: strings.TrimSpace(fn.Description),
			InputSchema: inputSchema,
			Handler: &ToolHandler{
				Type:         "function",
				FunctionName: name,
			},
			Annotations: &ToolAnnotations{
				Destructive:   strings.EqualFold(fn.FunctionKind, "mutation"),
				ExecutionTime: "fast",
			},
			Origin: "generated:function:" + name,
		}

		if err := ValidateTool(tool); err != nil {
			if logger != nil {
				logger.Warn("generated function tool failed validation; skipping",
					"component", ComponentName,
					"function", name,
					"error", err.Error())
			}
			continue
		}

		if err := tools.add(tool); err != nil {
			if logger != nil {
				logger.Warn("failed to register generated function tool; skipping",
					"component", ComponentName,
					"function", name,
					"error", err.Error())
			}
			continue
		}
	}
}

func toolInputSchemaFromArgs(assert *ArgsSchemaConfig) (json.RawMessage, error) {
	additionalProperties := false
	if assert != nil && assert.AdditionalProperties != nil {
		additionalProperties = *assert.AdditionalProperties
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": additionalProperties,
	}

	if assert == nil || len(assert.Fields) == 0 {
		return json.Marshal(schema)
	}

	props := make(map[string]any, len(assert.Fields))
	required := make([]string, 0, len(assert.Fields))

	for _, f := range assert.Fields {
		if f == nil || strings.TrimSpace(f.Name) == "" {
			continue
		}
		name := strings.TrimSpace(f.Name)
		props[name] = jsonSchemaForArgsField(f)
		if !f.Optional {
			required = append(required, name)
		}
	}

	schema["properties"] = props
	if len(required) > 0 {
		schema["required"] = required
	}

	return json.Marshal(schema)
}

func jsonSchemaForArgsField(f *FunctionArgsField) map[string]any {
	if f == nil {
		return map[string]any{}
	}

	typ := strings.TrimSpace(strings.ToLower(f.Type))
	switch typ {
	case "string":
		schema := map[string]any{"type": "string"}
		if len(f.Enum) > 0 {
			schema["enum"] = append([]any(nil), f.Enum...)
		}
		if strings.TrimSpace(f.Format) != "" {
			schema["format"] = strings.TrimSpace(f.Format)
		}
		return schema
	case "number":
		schema := map[string]any{"type": "number"}
		if len(f.Enum) > 0 {
			schema["enum"] = append([]any(nil), f.Enum...)
		}
		if f.Minimum != nil {
			schema["minimum"] = *f.Minimum
		}
		if f.Maximum != nil {
			schema["maximum"] = *f.Maximum
		}
		return schema
	case "int", "integer":
		schema := map[string]any{"type": "integer"}
		if len(f.Enum) > 0 {
			schema["enum"] = append([]any(nil), f.Enum...)
		}
		if f.Minimum != nil {
			schema["minimum"] = *f.Minimum
		}
		if f.Maximum != nil {
			schema["maximum"] = *f.Maximum
		}
		return schema
	case "bool", "boolean":
		schema := map[string]any{"type": "boolean"}
		if len(f.Enum) > 0 {
			schema["enum"] = append([]any(nil), f.Enum...)
		}
		return schema
	case "array":
		// OpenAI tool schema validation requires array schemas to specify "items".
		// If the assertion includes nested fields, treat array items as objects with that schema.
		items := map[string]any{}
		if f.Items != nil {
			items = jsonSchemaForArgsField(f.Items)
		} else if len(f.Nested) > 0 {
			items = jsonSchemaForArgsField(&FunctionArgsField{Type: "object", Nested: f.Nested})
		}
		schema := map[string]any{
			"type":  "array",
			"items": items,
		}
		if len(f.Enum) > 0 {
			schema["enum"] = append([]any(nil), f.Enum...)
		}
		return schema
	case "object":
		// Nested object schema
		props := map[string]any{}
		required := []string{}
		for _, n := range f.Nested {
			if n == nil || strings.TrimSpace(n.Name) == "" {
				continue
			}
			nm := strings.TrimSpace(n.Name)
			props[nm] = jsonSchemaForArgsField(n)
			if !n.Optional {
				required = append(required, nm)
			}
		}
		obj := map[string]any{
			"type":       "object",
			"properties": props,
		}
		// additionalProperties resolution:
		//   1. explicit @additionalProperties wins.
		//   2. a free-form object (no declared sub-fields) defaults to
		//      OPEN -- otherwise additionalProperties:false rejects
		//      every key, making the whole partial-update family
		//      (updateTodo.payload, notesUpdate, ...) uncallable
		//      even though the payload is documented to carry the
		//      changed fields (#1606).
		//   3. an object WITH declared sub-fields stays CLOSED, so the
		//      schema is descriptive and typo args are rejected.
		switch {
		case f.AdditionalProperties != nil:
			obj["additionalProperties"] = *f.AdditionalProperties
		case len(props) == 0:
			obj["additionalProperties"] = true
		default:
			obj["additionalProperties"] = false
		}
		if len(required) > 0 {
			obj["required"] = required
		}
		if len(f.Enum) > 0 {
			obj["enum"] = append([]any(nil), f.Enum...)
		}
		return obj
	case "any", "":
		if len(f.Enum) > 0 {
			return map[string]any{"enum": append([]any(nil), f.Enum...)}
		}
		return map[string]any{}
	default:
		// Unknown assertion type: treat as any.
		if len(f.Enum) > 0 {
			return map[string]any{"enum": append([]any(nil), f.Enum...)}
		}
		return map[string]any{}
	}
}
