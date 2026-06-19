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
			"inputSchema": fn.mcpInputSchema(),
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
		if fn.FunctionKind == "query" || fn.FunctionKind == "mutation" {
			out = append(out, fn)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// mcpInputSchema renders a function's args-block schema as a JSON-schema object
// (the shape MCP tools advertise). A function with no args block yields an empty
// object schema.
func (fn *Function) mcpInputSchema() map[string]any {
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
	return schema
}

// argsFieldToJSONSchema maps one args field to a JSON-schema property. memQL
// scalar type names are mapped to JSON-schema types; enum / format constraints
// carry through.
func argsFieldToJSONSchema(f *FunctionArgsField) map[string]any {
	prop := map[string]any{}
	if t := jsonSchemaType(f.Type); t != "" {
		prop["type"] = t
	}
	if len(f.Enum) > 0 {
		prop["enum"] = append([]any(nil), f.Enum...)
	}
	if f.Format != "" {
		prop["format"] = f.Format
	}
	if f.Items != nil {
		prop["items"] = argsFieldToJSONSchema(f.Items)
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
	// An automation lookup that misses the authored tree may be a synthetic
	// entry-point-logic wrapper (memql#1707) -- generated, not on disk. The
	// dry-run run_automation path resolves the wrapper name via LoadByName and
	// then fetches its source here, so return the generated source.
	if kind == string(languageParser.FunctionTypeAutomation) {
		if src, ok := EntryPointWrapperSource(logger, name); ok {
			return src, true
		}
	}
	return "", false
}
