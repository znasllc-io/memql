package memql

import (
	"context"
	"encoding/json"
	"strconv"
)

// applyToolDefaults merges server-provided defaults into the LLM-
// supplied tool-call args map.
//
// For fields named in tool.AutoInjectedFields the server default
// ALWAYS wins, regardless of what the LLM supplied -- and if the
// server has no default for an auto-injected field, the
// LLM-supplied value is dropped entirely. This is defense in
// depth: an LLM that hallucinates or attempts to forge values for
// fields like ownerUserId / agentId / partitionId can't sneak the
// forged value past the central validator just because the
// runtime forgot to stamp the default.
//
// MCP exception (memql#1684): when ctx carries WithMCPToolExecution
// and the server has NO default for an auto-injected field, the
// caller-supplied value is preserved instead of dropped. Over MCP
// the caller is the authenticated user (not an LLM), so a
// caller-supplied partitionId on tools like recentChat is a legitimate
// input. The security invariant is maintained: when a server
// default IS available it still wins regardless of MCP context,
// so the MCP path cannot escalate beyond what the server would
// otherwise stamp.
//
// For all other fields the legacy "fill-if-missing" semantic
// applies: defaults populate fields the LLM omitted, but don't
// overwrite fields the LLM set.
//
// In addition (memql#1682), any field that declares a static
// `@default(...)` in the tool's InputSchema is materialized when the
// caller omitted it. Before this, a declared default was advisory --
// it lived in the JSON Schema shown to the model but was NEVER applied
// server-side, so an omitted optional arg reached the handler as an
// unfilled `$args.<field>` placeholder (collapsed to `null`). That
// silently dropped the value for tools that pushed it into the handler
// query (searchUsers' `paginate(..., $args.limit)` crashed on the null
// limit). Materializing the schema default closes that gap: the value
// is coerced to the field's declared type so it substitutes as a real
// literal. Server-context defaults (auto-injected + the `defaults` map)
// still win over schema defaults, and an explicit caller value always
// wins over the schema default.
//
// Returns the (possibly mutated, possibly newly-allocated) args
// map; nil + no defaults = nil. A nil tool is treated as "no
// auto-injected fields known" so unknown-tool dispatch paths
// preserve the legacy behavior; the unknown-tool branch will
// reject the call anyway, so the validator's strictness there is
// moot.
//
// Surfaced by memql#107 (centralised tool-arg validator +
// auto-injected enforcement).
func applyToolDefaults(ctx context.Context, tool *Tool, args map[string]any, defaults map[string]any) map[string]any {
	schemaDefaults := toolSchemaDefaults(tool)
	if len(defaults) == 0 && len(schemaDefaults) == 0 && (tool == nil || len(tool.AutoInjectedFields) == 0) {
		return args
	}
	if args == nil {
		args = map[string]any{}
	}

	autoInjected := map[string]struct{}{}
	if tool != nil {
		for _, field := range tool.AutoInjectedFields {
			autoInjected[field] = struct{}{}
		}
	}

	// Pass 1: auto-injected fields -- server default wins, or drop.
	// MCP exception: when no server default is available AND the call
	// originated from the MCP connector, preserve the caller-supplied
	// value instead of dropping it (memql#1684).
	isMCP := mcpToolExecution(ctx)
	for field := range autoInjected {
		if v, ok := defaults[field]; ok {
			// Server default always wins, even on the MCP path.
			args[field] = v
		} else if !isMCP {
			// Agent runtime: drop LLM-supplied value (security).
			delete(args, field)
		}
		// else MCP + no server default: preserve caller-supplied value.
	}

	// Pass 2: every other server-context default -- fill-if-missing.
	for k, v := range defaults {
		if _, isAuto := autoInjected[k]; isAuto {
			continue
		}
		if _, exists := args[k]; !exists {
			args[k] = v
		}
	}

	// Pass 3: static schema `@default(...)` -- fill-if-missing, lowest
	// priority (server-context defaults from pass 2 already ran). Never
	// touches auto-injected fields.
	for k, v := range schemaDefaults {
		if _, isAuto := autoInjected[k]; isAuto {
			continue
		}
		if _, exists := args[k]; !exists {
			args[k] = v
		}
	}

	return args
}

// toolSchemaDefaults extracts the per-field static `@default(...)`
// values declared in a tool's InputSchema, coerced to the field's
// declared JSON-Schema type so they substitute as proper literals.
// Returns nil when the tool has no schema or no field defaults.
func toolSchemaDefaults(tool *Tool) map[string]any {
	if tool == nil || len(tool.InputSchema) == 0 {
		return nil
	}
	var schema struct {
		Properties map[string]struct {
			Type    string `json:"type"`
			Default any    `json:"default"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		return nil
	}
	out := map[string]any{}
	for name, prop := range schema.Properties {
		if prop.Default == nil {
			continue
		}
		out[name] = coerceSchemaDefault(prop.Type, prop.Default)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// coerceSchemaDefault converts a JSON-Schema default value (the MemQL
// tool loader stores these as strings, e.g. "10" / "false") into the
// Go value matching the field's declared type, so downstream MemQL-arg
// substitution renders it as the right literal kind. Non-string
// defaults pass through unchanged; an uncoercible string falls back to
// the raw string.
func coerceSchemaDefault(typ string, raw any) any {
	s, ok := raw.(string)
	if !ok {
		return raw
	}
	switch typ {
	case "integer":
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
	case "number":
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	case "boolean":
		if b, err := strconv.ParseBool(s); err == nil {
			return b
		}
	}
	return s
}
