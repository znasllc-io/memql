package mcp

// Phase 1 (memql#1531): the Tier-1 read/exec tool surface. It reflects the
// engine's DSL `tool` constructs 1:1 into MCP `tools/list` (role-gated) and
// adds the generic `run_query` / `run_mutation` / `run_automation` dispatchers
// (design §6.1-6.2). Reflected-tool calls go through the engine's existing
// authorized path (ExecuteTool enforces the per-tool role gate internally);
// named queries/mutations run through the engine's normal Execute path, where
// the per-row authz model applies. `run_automation` is registered but deferred
// to Phase 4 (#1534).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

// ToolLister is the slice of the tool registry the surface reads. The concrete
// *memql.ToolRegistry satisfies it (List/Get are inherited from its embedded
// baseregistry), and tests fake it with hand-built tools.
type ToolLister interface {
	List() []*memql.Tool
	Get(name string) (*memql.Tool, error)
}

// Engine is the narrow slice of the in-process memQL engine the MCP tool
// surface needs. *memql.MemQLEngine is bridged onto it by engineAdapter (its
// Tools() returns the concrete *ToolRegistry, which satisfies ToolLister). A
// nil Engine yields an empty surface so the protocol head still answers (the
// Phase 0 behaviour + the no-engine tests).
type Engine interface {
	Tools() ToolLister
	ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error)
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
}

// Meta-tool names — the generic dispatchers (design §6.2).
const (
	toolRunQuery      = "run_query"
	toolRunMutation   = "run_mutation"
	toolRunAutomation = "run_automation"
)

// listMCPTools reflects the engine's DSL tools 1:1 into MCP tool descriptors,
// filtered by the acting role's per-tool gate, then appends the meta-tools.
// Client-execution tools (browser-driven, e.g. the ui* operator primitives)
// are skipped: they cannot run server-side over MCP.
func listMCPTools(eng Engine, role string) []map[string]any {
	out := make([]map[string]any, 0)
	if eng != nil {
		if reg := eng.Tools(); reg != nil {
			for _, t := range reg.List() {
				if t == nil || t.ClientExecution {
					continue
				}
				if !t.IsAllowedForRole(role) {
					continue
				}
				if m := memql.ToolDefinitionToMCP(t); m != nil {
					out = append(out, m)
				}
			}
		}
	}
	return append(out, metaToolDefs()...)
}

// metaToolDefs are the generic dispatchers added to tools/list alongside the
// reflected DSL tools. Each takes a construct `name` + `args`; the per-construct
// authz is enforced by the engine on execution.
func metaToolDefs() []map[string]any {
	schema := func(what string) map[string]any {
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "Name of the " + what + " to run."},
				"args": map[string]any{"type": "object", "description": "Arguments passed to the " + what + "."},
			},
			"required": []any{"name"},
		}
	}
	return []map[string]any{
		{"name": toolRunQuery, "description": "Run a named memQL query by name with args.", "inputSchema": schema("query")},
		{"name": toolRunMutation, "description": "Run a named memQL mutation by name with args.", "inputSchema": schema("mutation")},
		{"name": toolRunAutomation, "description": "Run a named memQL automation by name (enabled in Phase 4, memql#1534).", "inputSchema": schema("automation")},
	}
}

// callMCPTool dispatches a tools/call. name is either a reflected DSL tool
// (executed by name -- ExecuteTool enforces the per-tool role gate internally)
// or a meta-tool. It returns an MCP tools/call result object (content + isError)
// -- tool failures are reported as isError results, not protocol errors, per the
// MCP convention. The acting role is threaded onto the context for the gate.
func callMCPTool(ctx context.Context, eng Engine, role, name string, args map[string]any) map[string]any {
	if eng == nil {
		return errorResult("mcp tool surface unavailable: engine not connected")
	}
	ctx = memql.WithActingAgentRole(ctx, role)

	switch name {
	case toolRunQuery, toolRunMutation:
		return runNamedConstruct(ctx, eng, name, args)
	case toolRunAutomation:
		return errorResult("run_automation is not enabled until Phase 4 (memql#1534)")
	default:
		raw, err := eng.ExecuteToolByName(ctx, name, args)
		if err != nil {
			return errorResult(fmt.Sprintf("tool %q failed: %v", name, err))
		}
		return toolResultFromJSON(raw)
	}
}

// runNamedConstruct executes a named query/mutation via the engine's normal
// Execute path. Args are passed as the json-encoded function-call form the
// engine itself uses for tool function handlers (buildFunctionCallQuery), so
// escaping is the engine's, not hand-rolled string interpolation.
func runNamedConstruct(ctx context.Context, eng Engine, kind string, args map[string]any) map[string]any {
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return errorResult(kind + " requires a 'name'")
	}
	callArgs, _ := args["args"].(map[string]any)
	query, err := namedCallQuery(name, callArgs)
	if err != nil {
		return errorResult(err.Error())
	}
	res, err := eng.Execute(ctx, query)
	if err != nil {
		return errorResult(fmt.Sprintf("%s %q failed: %v", kind, name, err))
	}
	payload, err := json.Marshal(res.OutputPayload())
	if err != nil {
		return errorResult(fmt.Sprintf("%s %q: encode result: %v", kind, name, err))
	}
	return textResult(string(payload))
}

// namedCallQuery builds a `name(<json-args>)` invocation -- the same json-
// encoded function-call form the engine uses for tool function handlers
// (component/memql buildFunctionCallQuery), so the engine's parser handles
// escaping of the JSON-encoded values.
func namedCallQuery(name string, args map[string]any) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if len(args) == 0 {
		return name + "()", nil
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("args encode: %w", err)
	}
	return name + "(" + string(b) + ")", nil
}

func textResult(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": false}
}

func errorResult(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": true}
}

// toolResultFromJSON parses the engine's marshalled ToolCallResult (already
// {content,isError}-shaped) into the MCP result map; falls back to wrapping the
// raw string as text if it is not the expected shape.
func toolResultFromJSON(raw string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err == nil && m["content"] != nil {
		return m
	}
	return textResult(raw)
}

// engineAdapter bridges the concrete in-process engine (held on Server as
// `any`) onto the testable Engine interface. The concrete Tools() returns
// *memql.ToolRegistry, which satisfies ToolLister.
type engineAdapter struct{ c concreteEngine }

// concreteEngine is the method set of *memql.MemQLEngine the surface uses.
type concreteEngine interface {
	Tools() *memql.ToolRegistry
	ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error)
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
}

func (a engineAdapter) Tools() ToolLister { return a.c.Tools() }
func (a engineAdapter) ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error) {
	return a.c.ExecuteToolByName(ctx, name, args)
}
func (a engineAdapter) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	return a.c.Execute(ctx, query)
}

// asEngine adapts an arbitrary engine handle to the Engine interface, returning
// nil when the handle does not provide the concrete engine's method set (e.g.
// the nil handle used in protocol-only tests).
func asEngine(engine any) Engine {
	if engine == nil {
		return nil
	}
	if c, ok := engine.(concreteEngine); ok {
		return engineAdapter{c}
	}
	return nil
}
