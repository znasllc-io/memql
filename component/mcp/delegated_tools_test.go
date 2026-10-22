package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestDelegatedWorkRestoresOnlyItsCapturedCurrentOwnerAuthority(t *testing.T) {
	expires := time.Now().Add(time.Hour).Truncate(time.Second)
	claims := map[string]any{"sub": "v1:identity:user:mcp-person", "class": appSessionClass, "node_id": appSessionLabelPrefix + "s", "exp": float64(expires.Unix())}
	base := auth.ContextWithClaims(context.Background(), claims)
	base = auth.ContextWithToken(base, auth.BuildTokenInfo(claims))
	h := &httpHead{base: Config{Tier: TierInline}}
	cfg := h.configForRequest(httptest.NewRequest(http.MethodPost, MCPPath, nil).WithContext(base))
	if cfg.ActingRole != "" {
		t.Fatal("test must start with the deployed roleless app credential")
	}
	for _, tc := range []struct{ name, current, ceiling, want string }{
		{"owner", "owner", "owner", "owner"},
		{"downgraded", "reader", "owner", "reader"},
		{"ceiling", "owner", "reader", "reader"},
		{"no grant", "owner", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := delegatedFixture()
			eng.ownerRole = tc.current
			if tc.ceiling != "" {
				eng.runFields["executionAuthority"] = map[string]any{"roleCeiling": tc.ceiling, "credentialClass": "user"}
			}
			srv := NewServer(nil, "test", "1", eng, cfg)
			ctx := srv.withActorEnvelope(base)
			resolved, _, err := delegatedScope(ctx, eng, "s")
			if err != nil {
				t.Fatal(err)
			}
			access, _ := auth.AccessFromContext(resolved)
			if string(access.Role) != tc.want || auth.OriginFromContext(resolved) == auth.OriginInternal {
				t.Fatalf("scope widened authority: %+v", access)
			}
			retained, _ := auth.ClaimsFromContext(resolved)
			forwarded, _ := auth.ForwardedAuthorityFromContext(resolved)
			if AppSessionFromClaims(retained) != "s" || forwarded.CredentialClass != auth.ForwardedClassAppSession || !forwarded.ExpiresAt.Equal(expires) {
				t.Fatal("delegation replaced its app credential with parent credentials")
			}
			if result := callDelegatedTool(ctx, eng, "s", "gaTool", nil); isError(result) || string(eng.accessSeen.Role) != tc.want {
				t.Fatalf("resolved actor did not reach tool execution: %v", result)
			}
			if tc.want == "owner" {
				eng.ownerRole = "reader"
				if result := callDelegatedTool(ctx, eng, "s", "gaTool", nil); isError(result) || eng.accessSeen.Role != auth.RoleReader {
					t.Fatal("live connection retained owner authority after downgrade")
				}
			}
		})
	}
	for name, mutate := range map[string]func(*delegatedEngine){
		"cancelled":            func(e *delegatedEngine) { e.runFields["cancelRequested"] = true },
		"another owner":        func(e *delegatedEngine) { e.runFields["ownerUserId"] = "v1:identity:user:other" },
		"another goal":         func(e *delegatedEngine) { e.runFields["goalId"] = "other" },
		"superseded execution": func(e *delegatedEngine) { e.runFields["rerun"] = map[string]any{"requestId": "new"} },
		"expired badge": func(e *delegatedEngine) {
			e.runFields["executionAuthority"] = map[string]any{"roleCeiling": "owner", "credentialClass": "badge", "expiresAt": time.Now().Add(-time.Minute).Format(time.RFC3339Nano)}
		},
		"malformed grant": func(e *delegatedEngine) { e.runFields["executionAuthority"] = "owner" },
	} {
		t.Run(name, func(t *testing.T) {
			eng := delegatedFixture()
			eng.runFields["executionAuthority"] = map[string]any{"roleCeiling": "owner", "credentialClass": "user"}
			mutate(eng)
			ctx := NewServer(nil, "test", "1", eng, cfg).withActorEnvelope(base)
			if result := callDelegatedTool(ctx, eng, "s", "gaTool", nil); !isError(result) || eng.accessSeen != nil {
				t.Fatalf("invalid execution reached a tool: %v", result)
			}
		})
	}
	eng := delegatedFixture()
	badgeExpiry := time.Now().Add(time.Minute).Truncate(time.Second)
	eng.runFields["executionAuthority"] = map[string]any{"roleCeiling": "reader", "credentialClass": "badge", "expiresAt": badgeExpiry.Format(time.RFC3339Nano)}
	ctx := NewServer(nil, "test", "1", eng, cfg).withActorEnvelope(base)
	resolved, _, err := delegatedScope(ctx, eng, "s")
	forwarded, _ := auth.ForwardedAuthorityFromContext(resolved)
	if err != nil || forwarded.Role != auth.RoleReader || !forwarded.ExpiresAt.Equal(badgeExpiry) {
		t.Fatalf("delegation exceeded its parent badge grant: %+v %v", forwarded, err)
	}
}

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
	for name, bad := range map[string]any{
		"nan": math.NaN(), "infinity": math.Inf(1), "negative-infinity": math.Inf(-1),
		"overflow": float64(math.MaxInt64), "underflow": -math.MaxFloat64,
		"fractional": float64(expires.Unix()) + 0.5, "missing": nil,
		"invalid-number":  json.Number("invalid"),
		"number-overflow": json.Number("9223372036854775808"),
		"number-fraction": json.Number("1800000000.5"),
	} {
		t.Run(name, func(t *testing.T) {
			changed := auth.CloneClaims(claims)
			changed["exp"] = bad
			if _, _, err := delegatedScope(auth.ContextWithClaims(asPerson("reader"), changed), delegatedFixture(), "s"); err == nil {
				t.Fatal("invalid expiry gained forwarding authority")
			}
		})
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
	row                map[string]any
	runStatus          string
	runFields          map[string]any
	ownerRole          string
	accessSeen         *auth.AccessContext
	runSeen            common.RunContext
	defaultsSeen       map[string]any
	sourceRun          common.RunContext
	sourceArgs         map[string]any
	sourceResult       string
	checkpointErr      error
	checkpointMessages []common.ChatMessage
	checkpointArgs     map[string]any
}

func (e *delegatedEngine) PrepareDelegatedFeedback(_ context.Context, _ string, messages []common.ChatMessage, _ string, args map[string]any) error {
	if e.accessSeen != nil {
		panic("question executed before its checkpoint")
	}
	e.checkpointMessages, e.checkpointArgs = messages, args
	return e.checkpointErr
}

func (e *concreteDelegatedFixture) PrepareDelegatedFeedback(ctx context.Context, session string, messages []common.ChatMessage, name string, args map[string]any) error {
	return e.delegate.PrepareDelegatedFeedback(ctx, session, messages, name, args)
}

func TestDelegatedQuestionSavesContinuationBeforeAnyEffect(t *testing.T) {
	for _, name := range []string{"requestUserFeedback", "requestComputerUseScope", "work.requestComputerUseScope"} {
		for _, fail := range []bool{false, true} {
			fixture := delegatedFixture()
			fixture.reg.tools = append(fixture.reg.tools, tool(name, []string{"assistant"}))
			scope := fixture.row["toolScope"].(map[string]any)
			scope["tools"] = []common.ToolDefinition{{Name: name}}
			scope["messages"] = []common.ChatMessage{{Role: "user", Content: "Keep working on my request"}}
			scope["defaults"] = map[string]map[string]any{name: {"runId": "run-1"}}
			raw, _ := json.Marshal(scope)
			var stored map[string]any
			_ = json.Unmarshal(raw, &stored)
			fixture.row["toolScope"] = stored
			if fail {
				fixture.checkpointErr = errors.New("storage unavailable")
			}
			result := callDelegatedTool(asPerson("reader"), asEngine(&concreteDelegatedFixture{delegate: fixture}), "s", name, map[string]any{"runId": "forged"})
			if isError(result) != fail || (fixture.accessSeen == nil) != fail || len(fixture.checkpointMessages) != 1 || fixture.checkpointArgs["runId"] != "run-1" {
				t.Fatalf("question checkpoint barrier failed: tool=%s fail=%v result=%v", name, fail, result)
			}
		}
	}
}

// Production goes through engineAdapter because MemQLEngine.Tools returns a
// concrete registry. Direct Engine fakes cannot catch a missing adapter method.
type concreteDelegatedFixture struct {
	*memql.MemQLEngine
	delegate *delegatedEngine
}

func (e *concreteDelegatedFixture) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	return e.delegate.Execute(ctx, query)
}
func (e *concreteDelegatedFixture) ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error) {
	return e.delegate.ExecuteToolByName(ctx, name, args)
}
func (e *concreteDelegatedFixture) RecordDelegatedToolSource(ctx context.Context, name string, args map[string]any, result string) error {
	return e.delegate.RecordDelegatedToolSource(ctx, name, args, result)
}

func TestDeployedEngineAdapterPreservesDelegatedSourceReceipts(t *testing.T) {
	fixture := delegatedFixture()
	eng := asEngine(&concreteDelegatedFixture{delegate: fixture})
	if _, ok := eng.(engineAdapter); !ok {
		t.Fatalf("test bypassed the deployed engine adapter: %T", eng)
	}
	result := callDelegatedTool(asPerson("reader"), eng, "s", "gaTool", map[string]any{"runId": "forged"})
	if isError(result) {
		t.Fatal(resultText(result))
	}
	if fixture.sourceRun.RunId != "run-1" || fixture.sourceArgs["runId"] != "run-1" || fixture.sourceResult == "" {
		t.Fatalf("deployed adapter dropped the executed source receipt: run=%+v args=%v result=%q", fixture.sourceRun, fixture.sourceArgs, fixture.sourceResult)
	}
}

func TestDelegatedSchemaHidesServerArgumentsAndDispatchStillStampsThem(t *testing.T) {
	eng := delegatedFixture()
	registered, _ := eng.reg.Get("gaTool")
	registered.AutoInjectedFields = []string{"runId", "agentId"}
	scope := eng.row["toolScope"].(map[string]any)
	definition := scope["tools"].([]any)[0].(map[string]any)
	definition["InputSchema"] = map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{"action": map[string]any{"type": "string"}, "runId": map[string]any{"type": "string"}, "agentId": map[string]any{"type": "string"}},
		"required":   []any{"action", "runId", "agentId"},
	}
	defaults := scope["defaults"].(map[string]any)["gaTool"].(map[string]any)
	defaults["agentId"] = "agent-1"
	before, _ := json.Marshal(scope)
	listed, err := delegatedMCPTools(asPerson("reader"), eng, "s")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	for _, tool := range listed {
		if tool["name"] == "gaTool" {
			schema = tool["inputSchema"].(map[string]any)
		}
	}
	props := schema["properties"].(map[string]any)
	if len(props) != 1 || props["action"] == nil || len(schema["required"].([]any)) != 1 || schema["additionalProperties"] != false {
		t.Fatalf("model-facing contract still requires internal IDs or lost user inputs: %v", schema)
	}
	after, _ := json.Marshal(scope)
	if string(before) != string(after) {
		t.Fatal("listing mutated the durable execution contract")
	}
	if result := callDelegatedTool(asPerson("reader"), eng, "s", "gaTool", map[string]any{"action": "exec"}); isError(result) {
		t.Fatal(resultText(result))
	}
	if eng.sourceArgs["runId"] != "run-1" || eng.sourceArgs["agentId"] != "agent-1" || eng.sourceArgs["action"] != "exec" {
		t.Fatalf("server arguments were not restored before execution: %v", eng.sourceArgs)
	}
	delete(defaults, "agentId")
	if _, err := delegatedMCPTools(asPerson("reader"), eng, "s"); err == nil {
		t.Fatal("preflight accepted an unresolved required server argument")
	}
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
	return &delegatedEngine{fakeEngine: newFakeEngine(), runStatus: "running", ownerRole: "owner", runFields: map[string]any{"ownerUserId": "v1:identity:user:mcp-person"}, row: map[string]any{"ownerUserId": "v1:identity:user:mcp-person", "runId": "run-1", "stepId": "v1:work:step:step-1", "status": "running", "toolScope": stored, "prompt": "original input"}}
}

func (e *delegatedEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	e.query = query
	var row map[string]any
	switch {
	case strings.HasPrefix(query, "appSessionById("):
		row = e.row
	case strings.HasPrefix(query, "workRunForOwner("):
		row = map[string]any{"status": e.runStatus}
		for key, value := range e.runFields {
			row[key] = value
		}
	case strings.HasPrefix(query, "query userByIdSystem("):
		if auth.OriginFromContext(ctx) != auth.OriginInternal {
			panic("identity resolution lost its server boundary")
		}
		row = map[string]any{"role": e.ownerRole}
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
	e.accessSeen, _ = auth.AccessFromContext(ctx)
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
