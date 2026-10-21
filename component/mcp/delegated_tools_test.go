package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestDelegatedMCPForwardsVerifiedCredentialWithoutWidening(t *testing.T) {
	expires := time.Now().Add(time.Minute).Truncate(time.Second)
	claims := map[string]any{"sub": "v1:identity:user:mcp-person", "class": "app_session", "node_id": "app-session:s", "exp": float64(expires.Unix())}
	ctx, _, err := delegatedScope(auth.ContextWithClaims(asPerson("reader"), claims), delegatedFixture(), "s")
	if err != nil {
		t.Fatal(err)
	}
	authority, ok := auth.ForwardedAuthorityFromContext(ctx)
	if !ok || authority.CredentialClass != auth.ForwardedClassAppSession || authority.Role != auth.RoleReader || authority.Subject != claims["sub"] || !authority.ExpiresAt.Equal(expires) {
		t.Fatalf("lost or widened credential: %+v", authority)
	}
	if _, err := auth.VerifyForwardedAuthority(authority, expires); err == nil {
		t.Fatal("receiver accepted an expired app-session credential")
	}
	if _, err := auth.VerifyForwardedAuthority(authority, time.Now()); err != nil {
		t.Fatalf("receiver refused a live app session: %v", err)
	}
	missingExpiry := authority
	missingExpiry.ExpiresAt = time.Time{}
	if _, err := auth.VerifyForwardedAuthority(missingExpiry, time.Now()); err == nil {
		t.Fatal("receiver accepted an app-session credential with no expiry")
	}
	for key, bad := range map[string]any{"sub": "another-user", "node_id": "app-session:another-session", "class": "user", "exp": float64(time.Now().Add(-time.Minute).Unix())} {
		t.Run(key, func(t *testing.T) {
			changed := auth.CloneClaims(claims)
			changed[key] = bad
			if _, _, err := delegatedScope(auth.ContextWithClaims(asPerson("reader"), changed), delegatedFixture(), "s"); err == nil {
				t.Fatal("invalid credential gained forwarding authority")
			}
		})
	}
	ctx, _, err = delegatedScope(asPerson("reader"), delegatedFixture(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := auth.ForwardedAuthorityFromContext(ctx); ok {
		t.Fatal("no-auth configuration manufactured mesh authority")
	}
}

func TestDelegatedRecordingRetainsVerifiedAuthorityWithoutExecutionScope(t *testing.T) {
	expires := time.Now().Add(time.Minute).Truncate(time.Second)
	for _, verified := range []bool{true, false} {
		eng := delegatedFixture()
		recorder := &delegatedAuthorityRecorder{}
		ctx := withMCPAppSessionRecorder(asPerson("reader"), recorder)
		if verified {
			ctx = auth.ContextWithClaims(ctx, map[string]any{
				"sub": "v1:identity:user:mcp-person", "class": "app_session",
				"node_id": "app-session:s", "exp": float64(expires.Unix()),
			})
		}
		result := callDelegatedTool(ctx, eng, "s", "gaTool", nil)
		if isError(result) || !recorder.called {
			t.Fatalf("recording did not follow execution: %v", result)
		}
		if recorder.hasAuthority != verified {
			t.Fatalf("recording authority present=%v, verified=%v", recorder.hasAuthority, verified)
		}
		if verified && (recorder.authority.CredentialClass != auth.ForwardedClassAppSession ||
			recorder.authority.Subject != "v1:identity:user:mcp-person" ||
			recorder.authority.Role != auth.RoleReader || !recorder.authority.ExpiresAt.Equal(expires)) {
			t.Fatalf("recording widened or changed credential: %+v", recorder.authority)
		}
		if recorder.hasRun || recorder.agent != "" || len(recorder.defaults) != 0 {
			t.Fatal("recording inherited the delegated tool's execution scope")
		}
	}
}

type delegatedAuthorityRecorder struct {
	called, hasAuthority, hasRun bool
	authority                    auth.ForwardedAuthority
	agent                        string
	defaults                     map[string]any
}

func (r *delegatedAuthorityRecorder) RecordToolCall(ctx context.Context, _ AppSessionToolCall) error {
	r.called = true
	r.authority, r.hasAuthority = auth.ForwardedAuthorityFromContext(ctx)
	_, r.hasRun = common.RunFromContext(ctx)
	r.agent = memql.ActingAgentIdFromContext(ctx)
	r.defaults = common.ToolDefaultsFromContext(ctx)
	return nil
}

type delegatedEngine struct {
	*fakeEngine
	row          map[string]any
	runStatus    string
	runSeen      common.RunContext
	defaultsSeen map[string]any
	sourceRun    common.RunContext
	sourceArgs   map[string]any
	sourceResult string
}

func (e *delegatedEngine) RecordDelegatedToolSource(ctx context.Context, _ string, args map[string]any, result string) error {
	e.sourceRun, _ = common.RunFromContext(ctx)
	e.sourceArgs, e.sourceResult = args, result
	return nil
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
	return &delegatedEngine{fakeEngine: newFakeEngine(), runStatus: "running", row: map[string]any{"ownerUserId": "v1:identity:user:mcp-person", "runId": "run-1", "stepId": "v1:work:step:step-1", "status": "running", "toolScope": stored, "prompt": "original input"}}
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
		if eng.sourceRun.RunId != "run-1" || eng.sourceArgs["runId"] != "run-1" || eng.sourceResult == "" {
			t.Fatal("executed result did not reach its original owned run's evidence")
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
