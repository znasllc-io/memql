package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"google.golang.org/protobuf/types/known/structpb"
)

type delegatedEngine struct {
	*fakeEngine
	row          map[string]any
	runStatus    string
	runSeen      common.RunContext
	defaultsSeen map[string]any
}

func delegatedFixture() *delegatedEngine {
	scope := common.DelegatedTools{
		AgentID: "agent-1", AgentRole: "assistant",
		Run:      common.RunContext{RunId: "run-1", StepKey: "step-1", OwnerUserId: "v1:identity:user:mcp-person", Routing: common.RouteChoice{Source: "app:*"}},
		Tools:    []common.ToolDefinition{{Name: "gaTool", InputSchema: map[string]any{"type": "object"}}},
		Defaults: map[string]map[string]any{"gaTool": {"runId": "run-1", "data": map[string]any{"producedByPlanId": "run-1"}}},
	}
	raw, _ := json.Marshal(scope)
	var stored map[string]any
	_ = json.Unmarshal(raw, &stored)
	return &delegatedEngine{fakeEngine: newFakeEngine(), runStatus: "running", row: map[string]any{"ownerUserId": "v1:identity:user:mcp-person", "runId": "run-1", "stepId": "step-1", "status": "running", "toolScope": stored, "prompt": "original input"}}
}

func (e *delegatedEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	e.query = query
	var row map[string]any
	switch {
	case strings.HasPrefix(query, "appSessionById("):
		row = e.row
	case strings.HasPrefix(query, "workRunForOwner("):
		row = map[string]any{"status": e.runStatus}
	default:
		return e.fakeEngine.Execute(ctx, query)
	}
	if row == nil {
		return &memql.ExecuteResult{}, nil
	}
	payload, err := structpb.NewStruct(row)
	if err != nil {
		return nil, err
	}
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{Payload: payload}}}}, nil
}

func (e *delegatedEngine) ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error) {
	e.runSeen, _ = common.RunFromContext(ctx)
	e.defaultsSeen = common.ToolDefaultsFromContext(ctx)
	return e.fakeEngine.ExecuteToolByName(ctx, name, args)
}

func TestDelegatedMCPUsesStoredScopeAcrossServers(t *testing.T) {
	eng := delegatedFixture()
	// Independent protocol heads share only the durable engine projection.
	for range 2 {
		srv := NewServer(nil, "test", "1", eng, Config{ActingUser: "v1:identity:user:mcp-person", ActingRole: "owner", AppSessionId: "session-1", Tier: TierInline})
		resp := srv.handleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if resp.Error != nil {
			t.Fatal(resp.Error)
		}
		names := toolNames(resp.Result.(map[string]any)["tools"].([]map[string]any))
		if !names["gaTool"] || !names["next_task"] || names["openTool"] || names["run_query"] || names["define"] {
			t.Fatal(names)
		}
		res := callMCPTool(asPerson("owner"), eng, "owner", TierInline, "session-1", "gaTool", map[string]any{"runId": "forged", "data": map[string]any{"producedByPlanId": "forged", "text": "keep"}})
		if isError(res) {
			t.Fatal(resultText(res))
		}
		if eng.roleSeen != "assistant" || eng.runSeen.RunId != "run-1" || eng.runSeen.Routing.Source != "app:*" || eng.toolArgs["runId"] != "run-1" {
			t.Fatalf("lost context: %+v %+v", eng.runSeen, eng.toolArgs)
		}
		data := eng.toolArgs["data"].(map[string]any)
		if data["text"] != "keep" || data["producedByPlanId"] != "run-1" {
			t.Fatal(data)
		}
	}
	// A live connection must observe closure rather than cache the old grant.
	eng.row["status"] = "ended"
	if !isError(callMCPTool(asPerson("owner"), eng, "owner", TierInline, "session-1", "gaTool", nil)) {
		t.Fatal("ended session retained authority")
	}
}

func TestDelegatedMCPRefusesScopeExpansionAndMissingTools(t *testing.T) {
	for _, name := range []string{"openTool", "run_query", "run_mutation", "define", "query"} {
		eng := delegatedFixture()
		if !isError(callMCPTool(asPerson("owner"), eng, "owner", TierInline, "s", name, nil)) || eng.toolName != "" {
			t.Fatalf("unoffered tool %s reached execution", name)
		}
	}
	for _, mutate := range []func(*delegatedEngine){
		func(e *delegatedEngine) { e.row["ownerUserId"] = "another-user" },
		func(e *delegatedEngine) { e.row["runId"] = "another-run" },
		func(e *delegatedEngine) { e.runStatus = "cancelled" },
		func(e *delegatedEngine) { e.reg = &fakeRegistry{} },
	} {
		eng := delegatedFixture()
		mutate(eng)
		if _, err := delegatedMCPTools(asPerson("owner"), eng, "s"); err == nil {
			t.Fatal("invalid scope passed readiness")
		}
	}
}

func TestDelegatedSessionProtocolIsActuallyDispatchable(t *testing.T) {
	eng := delegatedFixture()
	srv := NewServer(nil, "test", "1", eng, Config{ActingUser: "v1:identity:user:mcp-person", ActingRole: "owner", AppSessionId: "s"})
	for _, name := range []string{toolNextTask, toolSubmit} {
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": map[string]any{"result": map[string]any{"ok": true}}}})
		resp := srv.handleMessage(context.Background(), data)
		if resp.Error != nil || isError(resp.Result.(map[string]any)) {
			t.Fatalf("%s refused: %+v", name, resp)
		}
	}
}
