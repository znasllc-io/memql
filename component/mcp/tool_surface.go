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
//
// ExecuteAuthored (Phase 3 #1533) runs a query with the session's authored
// constructs resolvable by name, core-first; PromoteAuthoredConstruct registers
// a validated session construct into the engine's durable/shared registries.
type Engine interface {
	Tools() ToolLister
	ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error)
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
	ExecuteAuthored(ctx context.Context, query, owner string, reg *memql.AuthoredRuntimeRegistry) (*memql.ExecuteResult, error)
	PromoteAuthoredConstruct(ctx context.Context, c *memql.AuthoredConstruct) error
}

// Meta-tool names — the generic dispatchers (design §6.2) + the tier-gated
// authoring / inline ops (define lands in Phase 3 #1533; inline query in
// Phase 5 #1535).
const (
	toolRunQuery      = "run_query"
	toolRunMutation   = "run_mutation"
	toolRunAutomation = "run_automation"
	toolDefine        = "define"
	toolPromote       = "promote"
	toolQuery         = "query"
)

// mcpSession carries the per-connection authoring state: the owner (the acting
// user every authored construct is scoped to) and the session-scoped,
// non-durable authored-construct registry. It rides on the context so the tool
// dispatch + the existing test call-sites stay signature-stable.
type mcpSession struct {
	owner    string
	registry *memql.AuthoredRuntimeRegistry
}

type mcpSessionKey struct{}

// withMCPSession attaches the session authoring state to ctx. A nil registry or
// empty owner yields a no-op session (define/promote then report unavailable),
// so callers can always attach unconditionally.
func withMCPSession(ctx context.Context, owner string, reg *memql.AuthoredRuntimeRegistry) context.Context {
	return context.WithValue(ctx, mcpSessionKey{}, mcpSession{owner: strings.TrimSpace(owner), registry: reg})
}

// mcpSessionFromContext returns the attached session, or a zero session (no
// owner, no registry) when none was set.
func mcpSessionFromContext(ctx context.Context) mcpSession {
	if s, ok := ctx.Value(mcpSessionKey{}).(mcpSession); ok {
		return s
	}
	return mcpSession{}
}

func (s mcpSession) available() bool { return s.registry != nil && s.owner != "" }

// listMCPTools reflects the engine's DSL tools 1:1 into MCP tool descriptors,
// filtered by the acting role's per-tool gate, then appends the meta-tools.
// Client-execution tools (browser-driven, e.g. the ui* operator primitives)
// are skipped: they cannot run server-side over MCP. The tier-gated `define`
// (Tier 2) and `query` (Tier 3) tools are listed only when BOTH the deployment
// tier (Gate A) and the acting role (Gate B) permit them.
func listMCPTools(eng Engine, role string, tier Tier) []map[string]any {
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
	out = append(out, metaToolDefs()...)
	if tierAllows(tier, classAuthor) && roleCanAuthor(role) {
		out = append(out, map[string]any{
			"name":        toolDefine,
			"description": "Author a memQL .memql bundle: validate + register session-scoped (non-durable) constructs callable by name within this session. Requires the authoring tier + owner/developer role.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"bundle": map[string]any{"type": "string", "description": "The .memql source to author."}},
				"required":   []any{"bundle"},
			},
		})
	}
	// promote is owner-only (a developer authors; only an owner makes a
	// construct durable), so it lists strictly tighter than define.
	if tierAllows(tier, classAuthor) && roleCanPromote(role) {
		out = append(out, map[string]any{
			"name":        toolPromote,
			"description": "Promote a session-authored construct into the durable, shared schema so it is callable by every session. Owner only.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string", "description": "Name of the session-authored construct to promote."},
					"kind": map[string]any{"type": "string", "description": "Construct kind (query / mutation / logic / spec). Optional; resolved by name when omitted."},
				},
				"required": []any{"name"},
			},
		})
	}
	if tierAllows(tier, classInline) && roleCanRunInline(role) {
		out = append(out, map[string]any{
			"name":        toolQuery,
			"description": "Run ad-hoc inline memQL query text. Requires the inline tier + owner/developer role.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"query": map[string]any{"type": "string", "description": "Inline memQL query text."}},
				"required":   []any{"query"},
			},
		})
	}
	return out
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
func callMCPTool(ctx context.Context, eng Engine, role string, tier Tier, name string, args map[string]any) map[string]any {
	if eng == nil {
		return errorResult("mcp tool surface unavailable: engine not connected")
	}
	ctx = memql.WithActingAgentRole(ctx, role)

	switch name {
	case toolRunQuery, toolRunMutation:
		return runNamedConstruct(ctx, eng, name, args)
	case toolRunAutomation:
		return errorResult("run_automation is not enabled until Phase 4 (memql#1534)")
	case toolDefine:
		return handleDefine(ctx, eng, role, tier, args)
	case toolPromote:
		return handlePromote(ctx, eng, role, tier, args)
	case toolQuery:
		return gateInline(role, tier)
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
	// Resolve session-authored constructs by name (core-first) when a session is
	// active; otherwise the plain core execution path.
	res, err := executeForSession(ctx, eng, query)
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

// executeForSession runs a query, resolving the caller's session-authored
// constructs by name (core-first) when a session is active. With no session it
// is the plain Execute path, so callers route every named-construct run through
// it unconditionally.
func executeForSession(ctx context.Context, eng Engine, query string) (*memql.ExecuteResult, error) {
	if s := mcpSessionFromContext(ctx); s.available() {
		return eng.ExecuteAuthored(ctx, query, s.owner, s.registry)
	}
	return eng.Execute(ctx, query)
}

// handleDefine implements the Tier-2 `define` op (design §3 Tier 2, §5): both
// gates first (server-side), then validate + register the bundle into the
// caller's session-scoped, owner-scoped, non-durable registry. New
// function-family constructs become callable by name within the session; they
// do NOT touch the shared/durable schema until an owner promotes them.
func handleDefine(ctx context.Context, eng Engine, role string, tier Tier, args map[string]any) map[string]any {
	if ok, deny := authoringGate(role, tier); !ok {
		return errorResult(deny)
	}
	s := mcpSessionFromContext(ctx)
	if !s.available() {
		return errorResult("define is unavailable: this MCP session has no authoring identity (set MEMQL_MCP_USER)")
	}
	bundle, _ := args["bundle"].(string)
	if strings.TrimSpace(bundle) == "" {
		return errorResult("define requires a 'bundle' (.memql source)")
	}
	res, err := memql.AuthorSessionBundle(s.registry, s.owner, bundle)
	if err != nil {
		// A validation failure carries per-construct diagnostics; surface them.
		payload, _ := json.Marshal(res)
		return errorResult(fmt.Sprintf("define failed: %v\n%s", err, string(payload)))
	}
	payload, mErr := json.Marshal(res)
	if mErr != nil {
		return errorResult(fmt.Sprintf("define: encode result: %v", mErr))
	}
	return textResult(string(payload))
}

// handlePromote implements the owner-gated promotion of a session-authored
// construct into the durable/shared schema (design §5). Both gates first
// (authoring tier + OWNER role -- stricter than define), then look the construct
// up in the caller's session registry and register it into the engine's shared
// registries so every session can call it. A non-owner is refused by the gate;
// a construct the caller never defined is not found.
func handlePromote(ctx context.Context, eng Engine, role string, tier Tier, args map[string]any) map[string]any {
	if ok, deny := promoteGate(role, tier); !ok {
		return errorResult(deny)
	}
	s := mcpSessionFromContext(ctx)
	if !s.available() {
		return errorResult("promote is unavailable: this MCP session has no authoring identity (set MEMQL_MCP_USER)")
	}
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return errorResult("promote requires the 'name' of a session-authored construct")
	}
	c, err := resolveSessionConstruct(s, name, args)
	if err != nil {
		return errorResult(err.Error())
	}
	if err := eng.PromoteAuthoredConstruct(ctx, c); err != nil {
		return errorResult(fmt.Sprintf("promote %s %q failed: %v", c.Kind, c.Name, err))
	}
	return textResult(fmt.Sprintf("promoted %s %q into the durable shared schema; it is now callable by every session", c.Kind, name))
}

// resolveSessionConstruct finds an active session-authored construct by name.
// When `kind` is supplied it pins the lookup; otherwise it scans the owner's
// constructs for a unique name match and errors on an ambiguous one.
func resolveSessionConstruct(s mcpSession, name string, args map[string]any) (*memql.AuthoredConstruct, error) {
	if kind, _ := args["kind"].(string); strings.TrimSpace(kind) != "" {
		if c, ok := s.registry.Lookup(s.owner, strings.TrimSpace(kind), name); ok {
			return c, nil
		}
		return nil, fmt.Errorf("no session-authored %s %q to promote (define it first)", kind, name)
	}
	var match *memql.AuthoredConstruct
	for _, c := range s.registry.ListForOwner(s.owner) {
		if c.Name == name {
			if match != nil {
				return nil, fmt.Errorf("construct name %q is ambiguous; pass 'kind' to disambiguate", name)
			}
			match = c
		}
	}
	if match == nil {
		return nil, fmt.Errorf("no session-authored construct %q to promote (define it first)", name)
	}
	return match, nil
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
	ExecuteAuthored(ctx context.Context, query, owner string, reg *memql.AuthoredRuntimeRegistry) (*memql.ExecuteResult, error)
	PromoteAuthoredConstruct(ctx context.Context, c *memql.AuthoredConstruct) error
}

func (a engineAdapter) Tools() ToolLister { return a.c.Tools() }
func (a engineAdapter) ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error) {
	return a.c.ExecuteToolByName(ctx, name, args)
}
func (a engineAdapter) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	return a.c.Execute(ctx, query)
}
func (a engineAdapter) ExecuteAuthored(ctx context.Context, query, owner string, reg *memql.AuthoredRuntimeRegistry) (*memql.ExecuteResult, error) {
	return a.c.ExecuteAuthored(ctx, query, owner, reg)
}
func (a engineAdapter) PromoteAuthoredConstruct(ctx context.Context, c *memql.AuthoredConstruct) error {
	return a.c.PromoteAuthoredConstruct(ctx, c)
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
