//go:build agent

package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/planner"
	procedure "github.com/znasllc-io/memql/integrations/procedure"
)

// procedure_app_fallback_test.go -- a goal a replay cannot serve goes back to
// the app through the router's session door, and ONLY there.

type fakeSessionClient struct {
	result  *common.ToolCallingChatResult
	err     error
	session memql.AppSessionOutcome
	set     bool
	calls   int
	got     []common.ChatMessage
}

func (c *fakeSessionClient) CallChatWithTools(_ context.Context, messages []common.ChatMessage, _ []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	c.calls++
	c.got = messages
	return c.result, c.err
}

func (c *fakeSessionClient) LastSession() (memql.AppSessionOutcome, bool) { return c.session, c.set }

type fakeSessionRouter struct {
	client     common.ToolCallingChatAIProvider
	resolution airoute.Resolution
	err        error
	calls      int
	got        airoute.ResolveRequest
	gotCtx     context.Context
}

func (r *fakeSessionRouter) resolve(ctx context.Context, req airoute.ResolveRequest) (common.ToolCallingChatAIProvider, airoute.Resolution, error) {
	r.calls++
	r.got, r.gotCtx = req, ctx
	return r.client, r.resolution, r.err
}

func sessionResolution() airoute.Resolution {
	return airoute.Resolution{ProviderName: "app:claude-code", Decision: airoute.Decision{Door: airoute.DoorSession}}
}

func fallbackRequest() procedure.FallbackRequest {
	return procedure.FallbackRequest{
		OwnerUserId: "v1:identity:user:owner",
		GoalId:      "v1:work:goal:g1",
		RunId:       "v1:work:run:goalrun",
		StepId:      "v1:work:step:goalrun-replayed",
		App:         "claude-code",
		Statement:   "Write the weekly report",
		Guidance: procedure.Guidance{
			Procedure: "weeklyReport",
			Prompt:    "A learned procedure already ran these steps -- do not repeat them:\n- fs_write out/report.md (idem-0)\nIt stopped at step 2 because: exit code 1 where every recording exited 0",
		},
	}
}

func TestTheFallbackHandsTheStepToTheRecordedAppThroughTheSessionDoor(t *testing.T) {
	client := &fakeSessionClient{
		result:  &common.ToolCallingChatResult{AssistantText: "transcript"},
		session: memql.AppSessionOutcome{ChildRunId: "v1:work:run:repair", SessionId: "v1:worker:appSession:s1", Content: "the report is written"},
		set:     true,
	}
	rt := &fakeSessionRouter{client: client, resolution: sessionResolution()}
	f := &procedureAppFallback{resolve: rt.resolve, agents: testAgents}

	out, err := f.Handover(context.Background(), fallbackRequest())
	if err != nil {
		t.Fatal(err)
	}
	if out.ChildRunId != "v1:work:run:repair" || out.SessionId != "v1:worker:appSession:s1" || out.Content != "the report is written" {
		t.Fatalf("outcome = %+v", out)
	}
	req := rt.got
	if req.Modality != airoute.ModalityTools || !req.Needs.Tools || req.Needs.MinContextTokens <= 0 {
		t.Fatalf("request modality/needs = %s %+v", req.Modality, req.Needs)
	}
	if req.Level != airoute.LevelReasoning {
		t.Fatalf("level = %q, want reasoning when the request names none", req.Level)
	}
	if req.ExplicitProvider != "app:claude-code" {
		t.Fatalf("explicit provider = %q, want the app the procedure was recorded from", req.ExplicitProvider)
	}
	if req.RunId != "v1:work:run:goalrun" || req.StepId != "v1:work:step:goalrun-replayed" || req.UserId != "v1:identity:user:owner" || req.AgentId != testReasoningAgent.Id {
		t.Fatalf("request identity = %+v", req)
	}
	if access, ok := auth.AccessFromContext(rt.gotCtx); !ok || access.UserId != "v1:identity:user:owner" {
		t.Fatalf("the handover was not resolved as the owner: %+v", access)
	}
	if len(client.got) != 1 || client.got[0].Role != "user" ||
		client.got[0].Content != "Write the weekly report\n\n"+fallbackRequest().Guidance.Prompt {
		t.Fatalf("messages = %+v, want the goal statement then the guidance", client.got)
	}
}

func TestTheFallbackAsksForTheRequestsLevel(t *testing.T) {
	rt := &fakeSessionRouter{client: &fakeSessionClient{result: &common.ToolCallingChatResult{}}, resolution: sessionResolution()}
	req := fallbackRequest()
	req.Level = "strong"
	if _, err := (&procedureAppFallback{resolve: rt.resolve, agents: testAgents}).Handover(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if rt.got.Level != airoute.LevelStrong {
		t.Fatalf("level = %q", rt.got.Level)
	}
	req.Level = "genius"
	rt.calls = 0
	if _, err := (&procedureAppFallback{resolve: rt.resolve, agents: testAgents}).Handover(context.Background(), req); err == nil || rt.calls != 0 {
		t.Fatalf("an unknown level = %v, router asked %d times", err, rt.calls)
	}
}

func TestAGoalWhoseAppIsUnknownGoesToAnySignedInApp(t *testing.T) {
	for _, app := range []string{"", "some-new-app"} {
		rt := &fakeSessionRouter{client: &fakeSessionClient{result: &common.ToolCallingChatResult{}}, resolution: sessionResolution()}
		req := fallbackRequest()
		req.App = app
		if _, err := (&procedureAppFallback{resolve: rt.resolve, agents: testAgents}).Handover(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if rt.got.ExplicitProvider != memql.AppWildcard {
			t.Fatalf("app %q -> %q, want %q", app, rt.got.ExplicitProvider, memql.AppWildcard)
		}
	}
}

// ANYTHING BUT AN APP SESSION IS REFUSED, BEFORE THE CALL. A goal handed
// back "to the app" that a vendor model then served would be a different
// thing from the one the ladder promised, and a paid one.
func TestTheFallbackRefusesAnythingButAnAppSession(t *testing.T) {
	for _, door := range []string{airoute.DoorFederation, airoute.DoorLocal, airoute.DoorApp} {
		client := &fakeSessionClient{result: &common.ToolCallingChatResult{}}
		rt := &fakeSessionRouter{client: client, resolution: airoute.Resolution{ProviderName: "someModel", Decision: airoute.Decision{Door: door}}}
		_, err := (&procedureAppFallback{resolve: rt.resolve, agents: testAgents}).Handover(context.Background(), fallbackRequest())
		if err == nil || !strings.Contains(err.Error(), "not an app session") || !strings.Contains(err.Error(), door) {
			t.Errorf("door %s -> %v", door, err)
		}
		if client.calls != 0 {
			t.Errorf("door %s: the client was called", door)
		}
	}
}

// The runner turns this into procedure_fallback_unavailable, so the router's
// OWN WORDS must survive -- they say which door was shut and why.
func TestTheFallbackCarriesTheRoutersOwnWords(t *testing.T) {
	rt := &fakeSessionRouter{err: errors.New("router: every door is shut: app:claude-code: no signed-in machine offers claude-code")}
	_, err := (&procedureAppFallback{resolve: rt.resolve, agents: testAgents}).Handover(context.Background(), fallbackRequest())
	if err == nil || !strings.Contains(err.Error(), "no signed-in machine offers claude-code") || !strings.Contains(err.Error(), "app:claude-code") {
		t.Fatalf("err = %v", err)
	}
}

func TestAFailedSessionIsAnErrorInItsOwnWords(t *testing.T) {
	client := &fakeSessionClient{err: errors.New("cockpit-app: denied_by_scope: action requires scope \"full\", agent has \"observe\"")}
	rt := &fakeSessionRouter{client: client, resolution: sessionResolution()}
	_, err := (&procedureAppFallback{resolve: rt.resolve, agents: testAgents}).Handover(context.Background(), fallbackRequest())
	if err == nil || !strings.Contains(err.Error(), "denied_by_scope") {
		t.Fatalf("err = %v", err)
	}
}

func TestTheFallbackRefusesWhatCannotBeHandedOver(t *testing.T) {
	for name, mutate := range map[string]func(*procedure.FallbackRequest){
		"no owner":   func(r *procedure.FallbackRequest) { r.OwnerUserId = "" },
		"no run":     func(r *procedure.FallbackRequest) { r.RunId = "" },
		"no step":    func(r *procedure.FallbackRequest) { r.StepId = " " },
		"no content": func(r *procedure.FallbackRequest) { r.Statement, r.Guidance.Prompt = "", "" },
	} {
		rt := &fakeSessionRouter{client: &fakeSessionClient{}, resolution: sessionResolution()}
		req := fallbackRequest()
		mutate(&req)
		if _, err := (&procedureAppFallback{resolve: rt.resolve, agents: testAgents}).Handover(context.Background(), req); err == nil || rt.calls != 0 {
			t.Errorf("%s: %v, router asked %d times", name, err, rt.calls)
		}
	}
	rt := &fakeSessionRouter{client: &fakeSessionClient{}, resolution: sessionResolution()}
	noAgent := &procedureAppFallback{resolve: rt.resolve, agents: func(context.Context, string) (planner.ReasoningAgent, error) {
		return planner.ReasoningAgent{}, errors.New("no active assistant or seeded planner")
	}}
	if _, err := noAgent.Handover(context.Background(), fallbackRequest()); err == nil || !strings.Contains(err.Error(), "reasoning agent") || rt.calls != 0 {
		t.Fatalf("no reasoning agent -> %v", err)
	}
}

func TestTheGoalIsToldOnce(t *testing.T) {
	if got := procedureFallbackPrompt("Write it", "Write it\n\nsteps..."); got != "Write it\n\nsteps..." {
		t.Fatalf("guidance that opens with the statement -> %q", got)
	}
	if got := procedureFallbackPrompt("Write it", ""); got != "Write it" {
		t.Fatalf("no guidance -> %q", got)
	}
	if got := procedureFallbackPrompt("", "steps"); got != "steps" {
		t.Fatalf("no statement -> %q", got)
	}
}

// END TO END THROUGH THE REAL ROUTER: the pin reaches the session door, the
// delegate runs the step, and the subrun it opened comes back through the
// router's tools client -- the wrapper that used to hide it.
func TestTheFallbackReachesTheSessionDelegateThroughTheRealRouter(t *testing.T) {
	providers := memql.NewProviderRegistryForTest()
	providers.SetAppInference(&openAppDoors{doors: []memql.AppDoor{{AppId: "claude-code", Machines: []memql.AppMachine{{
		RegistrationId: "laptop", Name: "laptop", Online: true, LocalStream: true, StructuredResult: true, FollowUps: true,
	}}}}})
	delegate := &recordingSessionDelegate{}
	providers.SetAppSessionDelegate(delegate)
	rules := memql.NewRuleRegistry()
	if err := rules.Register(&memql.RuleConfig{
		Name: memql.DefaultRuleName, When: memql.RuleWhen{Present: map[string]bool{}}, Policy: "localFirst",
		OnUnavailable: memql.OnUnavailableDegrade, Locked: true, SourceFile: "dsl/rules/rules.memql",
	}); err != nil {
		t.Fatal(err)
	}
	if err := rules.Finalize(); err != nil {
		t.Fatal(err)
	}
	rt := router.New(providers, memql.NewPolicyRegistryForTest(map[string][]string{"localFirst": {"fleet:strongest"}}), rules, nil, nil)
	f := &procedureAppFallback{
		resolve: func(ctx context.Context, req airoute.ResolveRequest) (common.ToolCallingChatAIProvider, airoute.Resolution, error) {
			got, err := rt.ResolveFor(ctx, req)
			if err != nil {
				return nil, airoute.Resolution{}, err
			}
			client, _ := got.Client.(common.ToolCallingChatAIProvider)
			return client, got.Resolution, nil
		},
		agents: testAgents,
	}

	out, err := f.Handover(context.Background(), fallbackRequest())
	if err != nil {
		t.Fatal(err)
	}
	if delegate.runs != 1 || delegate.got.AppId != "claude-code" || delegate.got.StepId != "v1:work:step:goalrun-replayed" ||
		delegate.got.AgentId != testReasoningAgent.Id || delegate.got.ActingUserId != "v1:identity:user:owner" {
		t.Fatalf("the delegate saw %d runs, last %+v", delegate.runs, delegate.got)
	}
	if !strings.HasPrefix(delegate.got.Prompt, "Write the weekly report") {
		t.Fatalf("the app was handed %q", delegate.got.Prompt)
	}
	if out.ChildRunId != "v1:work:run:repair" || out.SessionId != "v1:worker:appSession:s1" {
		t.Fatalf("outcome = %+v, want the subrun and session the delegate opened", out)
	}
}

type openAppDoors struct{ doors []memql.AppDoor }

func (o *openAppDoors) Doors(context.Context, string) ([]memql.AppDoor, error) { return o.doors, nil }
func (o *openAppDoors) AppOrder(context.Context, string) ([]string, error)     { return nil, nil }
func (o *openAppDoors) Call(context.Context, memql.AppCallRequest) (memql.AppCallResult, error) {
	return memql.AppCallResult{}, errors.New("a chat turn is not what the fallback asks for")
}

type recordingSessionDelegate struct {
	runs int
	got  memql.AppSessionHandover
}

func (d *recordingSessionDelegate) RunStep(_ context.Context, h memql.AppSessionHandover) (memql.AppSessionOutcome, error) {
	d.runs++
	d.got = h
	return memql.AppSessionOutcome{Content: "repaired", ChildRunId: "v1:work:run:repair", SessionId: "v1:worker:appSession:s1"}, nil
}
