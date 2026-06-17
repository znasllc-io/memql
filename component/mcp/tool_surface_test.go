package mcp

// Phase 1 (#1531) tool-surface tests: role-gated reflection into tools/list,
// the per-tool gate on tools/call, and the run_query / run_mutation /
// run_automation dispatchers. They mirror the allow/deny structure of
// component/auth/rbac_test.go using a fake engine, so no live DB/DSL is needed.

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// --- fakes ------------------------------------------------------------------

type fakeRegistry struct{ tools []*memql.Tool }

func (f *fakeRegistry) List() []*memql.Tool { return f.tools }
func (f *fakeRegistry) Get(name string) (*memql.Tool, error) {
	for _, t := range f.tools {
		if t.Name == name {
			return t, nil
		}
	}
	return nil, errNotFound{name}
}

type errNotFound struct{ name string }

func (e errNotFound) Error() string { return "tool " + e.name + " not found" }

type fakeEngine struct {
	reg *fakeRegistry

	// recorded for assertions
	toolName      string
	toolArgs      map[string]any
	roleSeen      string
	query         string
	authoredOwner string
	authoredReg   *memql.AuthoredRuntimeRegistry
	promoted      *memql.AuthoredConstruct
	durableOwner  string // owner threaded into PromoteConstructDurable (memql#1557)
	inlineCalled  bool

	// @mcp promotion fakes (Phase 4 #1534)
	promotedFnTools []map[string]any
	promotedFns     map[string]string // name -> kind
}

func (e *fakeEngine) Tools() ToolLister { return e.reg }

func (e *fakeEngine) ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error) {
	e.toolName, e.toolArgs = name, args
	e.roleSeen = memql.ActingAgentRoleFromContext(ctx)
	tool, err := e.reg.Get(name)
	if err != nil {
		return "", err
	}
	// Simulate the engine's per-tool role gate (tool_execution.go:319).
	if !tool.IsAllowedForRole(e.roleSeen) {
		return "", errNotFound{name + " (role " + e.roleSeen + ")"}
	}
	return `{"content":[{"type":"text","text":"ok"}],"isError":false}`, nil
}

func (e *fakeEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	e.query = query
	e.roleSeen = memql.ActingAgentRoleFromContext(ctx)
	return &memql.ExecuteResult{}, nil
}

func (e *fakeEngine) ExecuteAuthored(ctx context.Context, query, owner string, reg *memql.AuthoredRuntimeRegistry) (*memql.ExecuteResult, error) {
	e.query = query
	e.authoredOwner = owner
	e.authoredReg = reg
	e.roleSeen = memql.ActingAgentRoleFromContext(ctx)
	return &memql.ExecuteResult{}, nil
}

func (e *fakeEngine) PromoteAuthoredConstruct(ctx context.Context, c *memql.AuthoredConstruct) error {
	e.promoted = c
	return nil
}

func (e *fakeEngine) PromoteConstructDurable(ctx context.Context, owner string, c *memql.AuthoredConstruct) error {
	e.promoted = c
	e.durableOwner = owner
	return nil
}

func (e *fakeEngine) ExecuteInline(ctx context.Context, query, owner string, reg *memql.AuthoredRuntimeRegistry) (*memql.ExecuteResult, error) {
	e.query = query
	e.inlineCalled = true
	e.authoredOwner = owner
	e.authoredReg = reg
	e.roleSeen = memql.ActingAgentRoleFromContext(ctx)
	return &memql.ExecuteResult{}, nil
}

func (e *fakeEngine) MCPPromotedFunctionTools() []map[string]any { return e.promotedFnTools }

func (e *fakeEngine) MCPPromotedFunctionKind(name string) (string, bool) {
	kind, ok := e.promotedFns[name]
	return kind, ok
}

func tool(name string, roles []string, clientExec bool) *memql.Tool {
	return &memql.Tool{Name: name, Description: name + " desc", AllowedRoles: roles, ClientExecution: clientExec}
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{reg: &fakeRegistry{tools: []*memql.Tool{
		tool("openTool", nil, false),                 // unrestricted
		tool("gaTool", []string{"assistant"}, false), // restricted to assistant
		tool("uiClick", nil, true),                   // client-execution: skipped
	}}}
}

func toolNames(list []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, t := range list {
		if n, ok := t["name"].(string); ok {
			out[n] = true
		}
	}
	return out
}

// --- tests ------------------------------------------------------------------

func TestListMCPTools_RoleFilterAndMetaTools(t *testing.T) {
	eng := newFakeEngine()

	cases := []struct {
		role       string
		wantGATool bool
		wantOpen   bool
		wantClient bool
		wantNumDSL int // reflected DSL tools (excludes the 3 meta-tools)
	}{
		{role: "", wantGATool: false, wantOpen: true, wantClient: false, wantNumDSL: 1}, // -> specialist
		{role: "specialist", wantGATool: false, wantOpen: true, wantClient: false, wantNumDSL: 1},
		{role: "assistant", wantGATool: true, wantOpen: true, wantClient: false, wantNumDSL: 2},
	}
	for _, tc := range cases {
		t.Run("role="+tc.role, func(t *testing.T) {
			names := toolNames(listMCPTools(eng, tc.role, TierAuthoring))
			if names["openTool"] != tc.wantOpen {
				t.Errorf("openTool visible=%v, want %v", names["openTool"], tc.wantOpen)
			}
			if names["gaTool"] != tc.wantGATool {
				t.Errorf("gaTool visible=%v, want %v (role gate)", names["gaTool"], tc.wantGATool)
			}
			if names["uiClick"] != tc.wantClient {
				t.Errorf("client-execution tool visible=%v, want %v", names["uiClick"], tc.wantClient)
			}
			// Meta-tools are always present.
			for _, mt := range []string{toolRunQuery, toolRunMutation, toolRunAutomation} {
				if !names[mt] {
					t.Errorf("missing meta-tool %q", mt)
				}
			}
		})
	}
}

func TestCallMCPTool_ReflectedTool(t *testing.T) {
	eng := newFakeEngine()
	res := callMCPTool(context.Background(), eng, "assistant", TierAuthoring, "gaTool", map[string]any{"x": 1})
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("expected success, got error result: %v", res)
	}
	if eng.toolName != "gaTool" {
		t.Errorf("ExecuteToolByName name = %q, want gaTool", eng.toolName)
	}
	if eng.roleSeen != "assistant" {
		t.Errorf("acting role threaded to ctx = %q, want assistant", eng.roleSeen)
	}
}

func TestCallMCPTool_RoleGateRejects(t *testing.T) {
	eng := newFakeEngine()
	// "specialist" (default) calling an assistant-only tool -> engine gate
	// rejects -> surfaced as an isError result.
	res := callMCPTool(context.Background(), eng, "", TierAuthoring, "gaTool", nil)
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("expected role-gate rejection (isError), got %v", res)
	}
}

func TestCallMCPTool_RunQueryBuildsInvocation(t *testing.T) {
	eng := newFakeEngine()
	res := callMCPTool(context.Background(), eng, "reader", TierAuthoring, toolRunQuery,
		map[string]any{"name": "activeSpaces", "args": map[string]any{"limit": 5}})
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("run_query unexpected error: %v", res)
	}
	if !strings.HasPrefix(eng.query, "activeSpaces(") || !strings.Contains(eng.query, `"limit":5`) {
		t.Errorf("run_query built %q, want activeSpaces({\"limit\":5})", eng.query)
	}
	if eng.roleSeen != "reader" {
		t.Errorf("run_query acting role = %q, want reader", eng.roleSeen)
	}
}

func TestCallMCPTool_RunQueryNoArgs(t *testing.T) {
	eng := newFakeEngine()
	callMCPTool(context.Background(), eng, "reader", TierAuthoring, toolRunMutation, map[string]any{"name": "ping"})
	if eng.query != "ping()" {
		t.Errorf("no-arg invocation = %q, want ping()", eng.query)
	}
}

func TestCallMCPTool_RunQueryRequiresName(t *testing.T) {
	eng := newFakeEngine()
	res := callMCPTool(context.Background(), eng, "reader", TierAuthoring, toolRunQuery, map[string]any{})
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("run_query without name should be an error result, got %v", res)
	}
	if eng.query != "" {
		t.Errorf("engine should not be called when name missing, got query %q", eng.query)
	}
}

func TestCallMCPTool_RunAutomationNotEnabled(t *testing.T) {
	eng := newFakeEngine()
	res := callMCPTool(context.Background(), eng, "owner", TierAuthoring, toolRunAutomation, map[string]any{"name": "x"})
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("run_automation should be a not-enabled error in Phase 1, got %v", res)
	}
}

func TestCallMCPTool_NilEngine(t *testing.T) {
	res := callMCPTool(context.Background(), nil, "owner", TierAuthoring, "anything", nil)
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("nil engine should yield an error result, got %v", res)
	}
}

func TestNamedCallQuery(t *testing.T) {
	if q, _ := namedCallQuery("foo", nil); q != "foo()" {
		t.Errorf("no args = %q, want foo()", q)
	}
	q, err := namedCallQuery("foo", map[string]any{"a": "b"})
	if err != nil || q != `foo({"a":"b"})` {
		t.Errorf("with args = %q (err %v), want foo({\"a\":\"b\"})", q, err)
	}
	if _, err := namedCallQuery("", nil); err == nil {
		t.Error("empty name should error")
	}
}
