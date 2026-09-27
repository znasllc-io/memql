package router

// A person's override for one step, through the router (epic memql#5414,
// design D20): a model pin is walked as a ONE-ENTRY chain, and an effort is a
// knob bound on the door already chosen -- never a reason to choose it.

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// overriddenStep is the context of a step a person re-ran with ov.
func overriddenStep(ov *common.StepOverride) context.Context {
	return common.ContextWithRun(context.Background(), common.RunContext{
		RunId: "v1:work:run:r1", GoalId: "v1:work:goal:g1", StepKey: "draft", OwnerUserId: "alice",
		Mode: common.RunModeLive, Override: ov,
	})
}

func TestAModelOverrideIsAOneEntryChain(t *testing.T) {
	ruled := &countingCloud{}
	pinned := &countingCloud{}
	providers := memql.NewProviderRegistryForTest()
	providers.RegisterWithParamsForTest("ruleWouldPick", "AnthropicStream", "claude-sonnet",
		map[string]any{"contextWindow": 200000}, ruled)
	providers.RegisterWithParamsForTest("personPinned", "OpenAI", "gpt",
		map[string]any{"contextWindow": 200000}, pinned)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"p": {"ruleWouldPick"}})
	r := New(providers, policies, testRules(t, defaultRule("p")), nil, nil)

	base := ResolveRequest{Level: airoute.LevelStrong, UserId: "alice"}
	req, err := memql.ApplyStepOverride(overriddenStep(&common.StepOverride{Model: "personPinned"}), base)
	if err != nil {
		t.Fatalf("ApplyStepOverride: %v", err)
	}
	client, resolved, err := r.ResolveChat(req)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ProviderName != "personPinned" {
		t.Fatalf("resolved %q, want the model the person pinned", resolved.ProviderName)
	}
	if resolved.Decision.Rule != "" {
		t.Fatalf("rule = %q: a pinned model consults no rule", resolved.Decision.Rule)
	}
	if len(resolved.Decision.Considered) != 1 || resolved.Decision.Considered[0].Entry != "personPinned" {
		t.Fatalf("considered = %+v, want the one pinned entry and nothing else", resolved.Decision.Considered)
	}
	if _, err := client.CallChat(context.Background(), []common.ChatMessage{{Role: "user", Content: "draft it"}}); err != nil {
		t.Fatalf("CallChat: %v", err)
	}
	if pinned.calls != 1 || ruled.calls != 0 {
		t.Fatalf("calls pinned=%d ruled=%d, want only the pin to answer", pinned.calls, ruled.calls)
	}

	// A PIN THAT CANNOT SERVE REFUSES. Falling through to the rule's chain
	// would answer the person's "run it on X" with a model they did not name.
	req, err = memql.ApplyStepOverride(overriddenStep(&common.StepOverride{Model: "nobodyRegisteredThis"}), base)
	if err != nil {
		t.Fatalf("ApplyStepOverride: %v", err)
	}
	if _, _, err := r.ResolveChat(req); err == nil {
		t.Fatal("an unavailable pin resolved; it must refuse rather than walk the rule's chain")
	}
	if ruled.calls != 0 {
		t.Fatalf("the rule's provider was called %d time(s) for a pinned step", ruled.calls)
	}

	// The control: the same request on a step nobody overrode is routed by
	// the rule, so the pin above is what made the difference.
	_, byRule, err := r.ResolveChat(base)
	if err != nil {
		t.Fatalf("resolve by rule: %v", err)
	}
	if byRule.ProviderName != "ruleWouldPick" {
		t.Fatalf("without an override the rule must decide, got %q", byRule.ProviderName)
	}
}

// effortFleet is a fleet door that records what it was asked to run.
type effortFleet struct {
	stubFleetInference
	got memql.FleetCallRequest
}

func (f *effortFleet) Call(ctx context.Context, req memql.FleetCallRequest) (memql.FleetCallResult, error) {
	f.got = req
	return f.stubFleetInference.Call(ctx, req)
}

// effortApps is an app door that records what it was asked to run.
type effortApps struct {
	stubAppInference
	got memql.AppCallRequest
}

func (a *effortApps) Call(ctx context.Context, req memql.AppCallRequest) (memql.AppCallResult, error) {
	a.got = req
	return a.stubAppInference.Call(ctx, req)
}

func TestEffortReachesOnlyTheDoorsThatHaveTheKnob(t *testing.T) {
	fleet := &effortFleet{stubFleetInference: stubFleetInference{models: []memql.FleetModel{fleetModel("llama3.1:8b", true)}}}
	apps := &effortApps{stubAppInference: stubAppInference{doors: []memql.AppDoor{openApp("claude-code")}}}
	delegate := &recordingDelegate{}
	cloud := &countingCloud{}
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(fleet)
	providers.SetAppInference(apps)
	providers.SetAppSessionDelegate(delegate)
	providers.RegisterWithParamsForTest("vendorModel", "OpenAI", "gpt", map[string]any{"contextWindow": 200000}, cloud)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"p": {"vendorModel"}})
	r := New(providers, policies, testRules(t, defaultRule("p")), nil, nil)
	messages := []common.ChatMessage{{Role: "user", Content: "draft it"}}

	chatOn := func(pin string) {
		t.Helper()
		req, err := memql.ApplyStepOverride(overriddenStep(&common.StepOverride{Model: pin, Effort: "high"}),
			ResolveRequest{Level: airoute.LevelStrong, UserId: "alice"})
		if err != nil {
			t.Fatalf("ApplyStepOverride: %v", err)
		}
		if req.Effort != "high" {
			t.Fatalf("request effort = %q, want the person's", req.Effort)
		}
		client, _, err := r.ResolveChat(req)
		if err != nil {
			t.Fatalf("resolve %s: %v", pin, err)
		}
		if _, err := client.CallChat(context.Background(), messages); err != nil {
			t.Fatalf("CallChat on %s: %v", pin, err)
		}
	}

	// The APP door carries it to the session it opens (AppSessionStart.effort).
	chatOn(memql.AppReferencePrefix + "claude-code")
	if apps.got.Effort != "high" {
		t.Fatalf("app door request effort = %q, want the person's effort", apps.got.Effort)
	}
	if apps.got.Level != string(airoute.LevelStrong) {
		t.Fatalf("binding the effort must not drop the level, got %q", apps.got.Level)
	}

	// The FLEET door carries it to ModelCallStart.
	chatOn(memql.FleetReferencePrefix + "llama3.1:8b")
	if fleet.got.Effort != "high" {
		t.Fatalf("fleet door request effort = %q, want the person's effort", fleet.got.Effort)
	}

	// A VENDOR record has no such knob and serves the call at its own
	// settings; the effort is not a reason to refuse it.
	chatOn("vendorModel")
	if cloud.calls != 1 {
		t.Fatalf("the vendor door must still serve a call carrying an effort, calls=%d", cloud.calls)
	}

	// The SESSION door hands it over with the step.
	req := toolRequest()
	req.Effort = "max"
	req.ExplicitProvider = memql.AppReferencePrefix + "claude-code"
	tools, _, err := r.resolveWithTools(context.Background(), req)
	if err != nil {
		t.Fatalf("resolveWithTools: %v", err)
	}
	if _, err := tools.CallChatWithTools(context.Background(), messages, nil); err != nil {
		t.Fatalf("CallChatWithTools: %v", err)
	}
	if delegate.got.Effort != "max" {
		t.Fatalf("session handover effort = %q, want the request's", delegate.got.Effort)
	}

	// The control: a call nobody asked an effort of carries none, so the
	// value above came from the override rather than from a default.
	client, _, err := r.ResolveChat(ResolveRequest{Level: airoute.LevelStrong, UserId: "alice",
		ExplicitProvider: memql.AppReferencePrefix + "claude-code"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := client.CallChat(context.Background(), messages); err != nil {
		t.Fatalf("CallChat: %v", err)
	}
	if apps.got.Effort != "" {
		t.Fatalf("a call with no effort asked of it carried %q", apps.got.Effort)
	}
}
