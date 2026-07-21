package memql

// mcp_promote.go -- engine support for the @mcp promotion annotation (MCP epic
// memql#1529 Phase 4 #1534). A query / mutation carrying @mcp is promoted into
// its own first-class MCP tool; the MCP protocol head reflects these and routes
// a call by name to the named-construct execution path. The automation half of
// @mcp lives on the automations Loader (the engine does not own automations),
// wired from the MCP node's app bootstrap.

import (
	"log/slog"
	"sort"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// MCPPromotedFunctionTools returns MCP tool descriptors (name / description /
// inputSchema) for every @mcp-promoted query + mutation in the function
// registry, sorted by name for deterministic listing. Logic functions are not
// promoted (they are an internal dispatch shape, not a model-facing surface).
func (e *MemQLEngine) MCPPromotedFunctionTools() []map[string]any {
	fns := e.mcpPromotedFunctions()
	out := make([]map[string]any, 0, len(fns))
	for _, fn := range fns {
		desc := fn.Description
		if strings.TrimSpace(desc) == "" {
			desc = "Run the " + fn.FunctionKind + " " + fn.Name + "."
		}
		out = append(out, map[string]any{
			"name":        fn.Name,
			"description": desc,
			"inputSchema": FunctionInputJSONSchema(fn),
		})
	}
	return out
}

// MCPPromotedFunctionKind reports the kind ("query" / "mutation") of a
// @mcp-promoted function by name, so the protocol head can route a first-class
// tool call to the named-construct executor. Non-promoted or unknown names
// return false.
func (e *MemQLEngine) MCPPromotedFunctionKind(name string) (string, bool) {
	if e.functions == nil {
		return "", false
	}
	fn, err := e.functions.Get(name)
	if err != nil || fn == nil || !fn.MCPPromoted {
		return "", false
	}
	if fn.FunctionKind == "query" || fn.FunctionKind == "mutation" {
		return fn.FunctionKind, true
	}
	return "", false
}

// mcpPromotedFunctions returns the @mcp-promoted query/mutation functions sorted
// by name.
func (e *MemQLEngine) mcpPromotedFunctions() []*Function {
	out := make([]*Function, 0)
	if e.functions == nil {
		return out
	}
	for _, fn := range e.functions.Snapshot() {
		if fn == nil || !fn.MCPPromoted {
			continue
		}
		// @disabled drops the function from the ADVERTISED surface, matching
		// the Enabled check on the function-backed tool registration path
		// (function_tools.go) -- the #2605 execution gate already refuses it
		// at call time, and an advertised-but-refused tool is a dishonest
		// surface (memql#2647). The router (MCPPromotedFunctionKind) keeps
		// routing the name so a direct call gets that refusal, not an
		// unknown-tool error.
		if !fn.Enabled {
			continue
		}
		// @internal means exactly "hide from external API discovery"
		// (annotations/registry.go), and the promoted MCP tool surface IS
		// external discovery -- so an @internal @mcp function is advertised
		// nowhere, matching the function-backed REGISTRATION path which has
		// always skipped `!fn.Enabled || fn.Internal` (function_tools.go).
		// The two paths disagreeing was the same dishonest-surface class
		// #2647 closed for @disabled (memql#2682). Routing is deliberately
		// NOT filtered: unlike @disabled, an @internal function is still
		// callable -- it is hidden, not off -- so an internal caller that
		// knows the name still resolves it.
		if fn.Internal {
			continue
		}
		if fn.FunctionKind == "query" || fn.FunctionKind == "mutation" {
			out = append(out, fn)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FunctionInputJSONSchema renders a function's args-block schema as a JSON-Schema
// object -- THE single source of truth for a function's MCP input contract
// (memql#1713). Both the @mcp typed-wrapper surface (MCPPromotedFunctionTools)
// and the describeFunction/help introspection payload render through this one
// function, so the two contracts can never diverge: there is exactly one
// renderer over fn.ArgsSchema. A function with no args block yields an empty
// object schema {"type":"object"}.
//
// Completeness: every constraint the args-block declares (type, enum, format,
// minimum/maximum, maxLength, pattern, array items, nested object
// properties+required, additionalProperties) is carried into the schema, so a
// connector consumer sees the full, correct contract rather than a bare type.
func FunctionInputJSONSchema(fn *Function) map[string]any {
	schema := map[string]any{"type": "object"}
	if fn == nil || fn.ArgsSchema == nil || len(fn.ArgsSchema.Fields) == 0 {
		return schema
	}
	props := make(map[string]any, len(fn.ArgsSchema.Fields))
	required := make([]any, 0)
	for _, f := range fn.ArgsSchema.Fields {
		if f == nil || f.Name == "" {
			continue
		}
		props[f.Name] = argsFieldToJSONSchema(f)
		if !f.Optional {
			required = append(required, f.Name)
		}
	}
	schema["properties"] = props
	if len(required) > 0 {
		schema["required"] = required
	}
	if fn.ArgsSchema.AdditionalProperties != nil {
		schema["additionalProperties"] = *fn.ArgsSchema.AdditionalProperties
	}
	return schema
}

// argsFieldToJSONSchema maps one args field to a JSON-schema property. memQL
// scalar type names map to JSON-schema types; every declared constraint
// (enum / format / numeric bounds / maxLength / pattern / array items / nested
// object fields) carries through so the rendered schema is complete.
func argsFieldToJSONSchema(f *FunctionArgsField) map[string]any {
	prop := map[string]any{}
	if f == nil {
		return prop
	}
	if f.Description != "" {
		prop["description"] = f.Description
	}
	jsType := jsonSchemaType(f.Type)
	if jsType != "" {
		prop["type"] = jsType
	}
	if len(f.Enum) > 0 {
		prop["enum"] = append([]any(nil), f.Enum...)
	}
	if f.Format != "" {
		prop["format"] = f.Format
	}
	if f.Minimum != nil {
		prop["minimum"] = *f.Minimum
	}
	if f.Maximum != nil {
		prop["maximum"] = *f.Maximum
	}
	if f.MaxLength > 0 {
		prop["maxLength"] = f.MaxLength
	}
	if f.Pattern != "" {
		prop["pattern"] = f.Pattern
	}
	if f.Items != nil {
		prop["items"] = argsFieldToJSONSchema(f.Items)
	}
	// Nested object fields project into properties + required, mirroring the
	// top-level rendering so a structured object arg advertises its full shape.
	if jsType == "object" && len(f.Nested) > 0 {
		nprops := make(map[string]any, len(f.Nested))
		nrequired := make([]any, 0)
		for _, nf := range f.Nested {
			if nf == nil || nf.Name == "" {
				continue
			}
			nprops[nf.Name] = argsFieldToJSONSchema(nf)
			if !nf.Optional {
				nrequired = append(nrequired, nf.Name)
			}
		}
		prop["properties"] = nprops
		if len(nrequired) > 0 {
			prop["required"] = nrequired
		}
		if f.AdditionalProperties != nil {
			prop["additionalProperties"] = *f.AdditionalProperties
		}
	}
	return prop
}

// jsonSchemaType maps a memQL args type name to its JSON-schema type. "any" maps
// to "" (no type constraint -- any value is permitted).
func jsonSchemaType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "string":
		return "string"
	case "number", "int", "integer", "float":
		return "number"
	case "bool", "boolean":
		return "boolean"
	case "object":
		return "object"
	case "array":
		return "array"
	default: // "any" / unknown -> unconstrained
		return ""
	}
}

// DSLConstructSource returns the raw `.memql` source slice for a named
// construct (query / mutation / logic / automation) by scanning the
// embedded/override DSL tree. Used by the MCP run_automation dry-run path,
// which feeds the source into the Gate-2 behavioral sandbox.
// Returns false when no slice with that (kind, name) is found.
//
// Automations are NOT function-registry constructs, so ExtractFunctionSlices
// deliberately skips them (the function-parse pipeline has no automation case).
// They are sliced by the dedicated ExtractAutomationSlices instead, so a
// kind=="automation" lookup actually resolves -- the dry-run gap reported in
// memql#1663, where the kind was hardcoded to "automation" against an extractor
// that never emits automation slices. The lookup is now keyed on the ACTUAL
// construct kind the caller asks for.
func DSLConstructSource(logger *slog.Logger, kind, name string) (string, bool) {
	for _, raw := range baseloader.ReadAll(logger) {
		var slices []FunctionSlice
		if kind == string(languageParser.FunctionTypeAutomation) {
			slices = ExtractAutomationSlices(raw.Content)
		} else {
			slices = ExtractFunctionSlices(raw.Content)
		}
		for _, slice := range slices {
			if string(slice.Kind) == kind && slice.Name == name {
				return slice.Source, true
			}
		}
	}
	return "", false
}
