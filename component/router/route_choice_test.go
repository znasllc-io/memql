package router

// The Ask route picker's choice, through the router (design brief section 6).
//
// The choice reaches a call on the RUN CONTEXT the executing node built from
// the run row -- the planner compiling, the agent running a step -- so every
// test here resolves through ResolveFor with nothing but that context: no
// request field says what the person chose, exactly as in production.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// chosenRun is the context of a run whose owner made choice: a STEP of it when
// stepKey is set (the agent running the reply), the run while it COMPILES when
// it is not (the planner's triage and compile).
func chosenRun(stepKey string, choice common.RouteChoice) context.Context {
	return common.ContextWithRun(context.Background(), common.RunContext{
		RunId: "v1:work:run:r1", GoalId: "v1:work:goal:g1", OwnerUserId: "alice",
		StepKey: stepKey, Mode: common.RunModeLive, Routing: choice,
	})
}

// The agent side: the reply step, pinned by its owner to Claude Code, is
// handed to a session on the owner's machine -- as THE OWNER'S pin, which is
// what the app gate admits -- and the vendor the rule would pick is never
// called.
func TestAnOwnersAppChoiceTakesTheReplyStepAsTheirPin(t *testing.T) {
	r, cloud, delegate := sessionRouter(t, []memql.AppDoor{openApp("claude-code")})
	// The rule's chain puts the vendor FIRST, so an ignored choice would show
	// as a vendor call rather than as a coincidence.
	r.policies = memql.NewPolicyRegistryForTest(map[string][]string{"defaultChain": {"streamClaudeSonnet"}})

	req := toolRequest()
	req.RunId, req.StepId = "", "" // the router fills both from the run context
	got, err := r.ResolveFor(chosenRun("reason", common.RouteChoice{Source: "app:claude-code", By: "alice"}), req)
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	if got.Resolution.Decision.Door != airoute.DoorSession || got.Resolution.Decision.Rule != "" {
		t.Fatalf("door=%q rule=%q, want a session reached through the pin", got.Resolution.Decision.Door, got.Resolution.Decision.Rule)
	}
	tools, ok := got.Client.(common.ToolCallingChatAIProvider)
	if !ok {
		t.Fatalf("client %T serves no tool turns", got.Client)
	}
	if _, err := tools.CallChatWithTools(memql.ContextWithBackgroundLane(context.Background()), []common.ChatMessage{{Role: "user", Content: "hi"}}, nil); err != nil {
		t.Fatalf("CallChatWithTools: %v", err)
	}
	if want := (memql.AppDoorPin{Pinned: true, By: "alice"}); delegate.got.Pin != want {
		t.Fatalf("the session was handed pin %+v, want %+v", delegate.got.Pin, want)
	}
	if delegate.got.StepId != "reason" {
		t.Fatalf("the session took step %q, want the reply step", delegate.got.StepId)
	}
	if cloud.calls != 0 {
		t.Fatalf("the vendor was called %d time(s) for a run pinned to Claude Code", cloud.calls)
	}
}

// The planner side, where no app door is installed yet: a PINNED source that
// cannot serve refuses, names what was pinned, and tries nothing else.
func TestAPinnedSourceTheNodeCannotReachRefusesWithoutSubstituting(t *testing.T) {
	cloud := &countingCloud{}
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(&stubFleetInference{models: []memql.FleetModel{fleetModel("qwen3.5:4b", true)}})
	providers.RegisterWithParamsForTest("vendor", "OpenAI", "gpt", map[string]any{"contextWindow": 200000}, cloud)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"defaultChain": {memql.FleetStrongest, "vendor"}})
	r := New(providers, policies, testRules(t, defaultRule("defaultChain")), nil, nil)

	triage := ResolveRequest{Level: airoute.LevelFast, Modality: airoute.ModalityStructured, PromptName: "goalComplexityTriage",
		Needs: airoute.Needs{Structured: true, MinContextTokens: 4000}}
	_, err := r.ResolveFor(chosenRun("", common.RouteChoice{Source: "app:claude-code", By: "alice"}), triage)
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) || refusal.Code != work.RefusalEveryDoorShut {
		t.Fatalf("err = %v, want an every_door_shut refusal", err)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, work.RefusalEveryDoorShut) || !strings.Contains(msg, "app:claude-code") ||
		!strings.Contains(msg, "never substituted") {
		t.Fatalf("the refusal does not say what was pinned and that nothing replaces it: %q", msg)
	}
	if len(refusal.Doors) != 1 || refusal.Doors[0].Name != "app:claude-code" {
		t.Fatalf("doors = %+v, want the pinned source alone", refusal.Doors)
	}
	if cloud.calls != 0 {
		t.Fatalf("a pinned run reached the vendor %d time(s)", cloud.calls)
	}

	// The control: the same call on a run nobody chose for is served by the
	// rule's chain, so the pin is what refused it.
	got, err := r.ResolveFor(chosenRun("", common.RouteChoice{}), triage)
	if err != nil {
		t.Fatalf("an Auto run: %v", err)
	}
	if got.Resolution.ProviderName != "fleet:qwen3.5:4b" {
		t.Fatalf("an Auto run resolved %q, want the rule's first entry", got.Resolution.ProviderName)
	}
}

// A ROUTE fails over. The same planner, the same missing app door: a route
// that names Claude Code first answers from the entry behind it.
func TestAChosenRouteFailsOverAlongItsOwnChain(t *testing.T) {
	cloud := &countingCloud{}
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(&stubFleetInference{models: []memql.FleetModel{fleetModel("qwen3.5:4b", true)}})
	providers.RegisterWithParamsForTest("vendor", "OpenAI", "gpt", map[string]any{"contextWindow": 200000}, cloud)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{
		"defaultChain": {"vendor"},
		"appFirst":     {memql.AppReferencePrefix + "claude-code", memql.FleetStrongest},
	})
	r := New(providers, policies, testRules(t, defaultRule("defaultChain")), nil, nil)

	triage := ResolveRequest{Level: airoute.LevelFast, Modality: airoute.ModalityStructured,
		Needs: airoute.Needs{Structured: true, MinContextTokens: 4000}}
	got, err := r.ResolveFor(chosenRun("", common.RouteChoice{Source: "policy:appFirst", By: "alice"}), triage)
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	d := got.Resolution.Decision
	if got.Resolution.ProviderName != "fleet:qwen3.5:4b" || d.Policy != "appFirst" || d.Rule != "" {
		t.Fatalf("resolved %q policy=%q rule=%q, want the route's second entry and no rule", got.Resolution.ProviderName, d.Policy, d.Rule)
	}
	var skipped bool
	for _, c := range d.Considered {
		if c.Entry == memql.AppReferencePrefix+"claude-code" && c.Reason != "selected" {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("considered = %+v, want the app entry recorded as passed over", d.Considered)
	}
	if cloud.calls != 0 {
		t.Fatalf("the rule's vendor was reached %d time(s) under a chosen route", cloud.calls)
	}

	// A route that disappeared after the turn chose it is a configuration
	// fault, said as one -- not a shut door that would park the run waiting
	// for a machine to wake.
	_, err = r.ResolveFor(chosenRun("", common.RouteChoice{Source: "policy:gone", By: "alice"}), triage)
	var refusal *InferenceUnavailable
	if err == nil || errors.As(err, &refusal) || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("err = %v, want a plain error naming the missing route", err)
	}
}

// The LEVEL binds the steps: the reply asks for what the person chose, and
// the rules route THAT level. Compile-time calls keep their own.
func TestTheChosenLevelIsWhatTheRulesSeeForAStep(t *testing.T) {
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(&stubFleetInference{models: []memql.FleetModel{fleetModel("qwen3.5:4b", true)}})
	strong := &countingCloud{}
	providers.RegisterWithParamsForTest("thinker", "OpenAI", "o-think", map[string]any{"contextWindow": 200000}, strong)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{
		"defaultChain": {memql.FleetReferencePrefix + "qwen3.5:4b"},
		"thinking":     {"thinker"},
	})
	r := New(providers, policies, testRules(t, defaultRule("defaultChain"), &memql.RuleConfig{
		Name: "reasoningWork", When: when("level", "reasoning"), Policy: "thinking", Locked: true, Precedence: 10,
		OnUnavailable: memql.OnUnavailableDegrade,
	}), nil, nil)
	choice := common.RouteChoice{Level: "reasoning", By: "alice"}
	req := ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityChat, Needs: airoute.Needs{MinContextTokens: 4000}}

	step, err := r.ResolveFor(chosenRun("reason", choice), req)
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if step.Resolution.ProviderName != "thinker" || step.Resolution.Decision.RequestedLevel != airoute.LevelReasoning {
		t.Fatalf("step resolved %q at %q, want the reasoning rule's chain", step.Resolution.ProviderName, step.Resolution.Decision.RequestedLevel)
	}
	compile, err := r.ResolveFor(chosenRun("", choice), req)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compile.Resolution.ProviderName != "fleet:qwen3.5:4b" || compile.Resolution.Decision.RequestedLevel != airoute.LevelStrong {
		t.Fatalf("compile resolved %q at %q, want its own level through the default rule", compile.Resolution.ProviderName, compile.Resolution.Decision.RequestedLevel)
	}
}

// The picker's vendor words are the router's own selectors.
func TestThePickersVendorWordsAreTheRoutersSelectors(t *testing.T) {
	for _, word := range []string{FederationSelectorCheapest, FederationSelectorStrongest} {
		if _, err := memql.ParseRouteChoice(FederationReferencePrefix+word, ""); err != nil {
			t.Fatalf("the picker refuses the router's own selector %q: %v", word, err)
		}
	}
	if _, err := memql.ParseRouteChoice(memql.FleetStrongest, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := memql.ParseRouteChoice(memql.FleetFastest, ""); err != nil {
		t.Fatal(err)
	}
}
