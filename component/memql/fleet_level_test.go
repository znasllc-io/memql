package memql

// The LEVEL a call was resolved at has to reach the machine that runs it.
//
// A local runtime has one knob the level reaches: on Ollama a `fast` call runs
// with the model's hidden thinking off (memql-cockpit ollamaThink). A
// goalComplexityTriage on qwen3.5:4b spent 2,052-5,636 hidden tokens --
// 26 s to 2 m 51 s -- on a ~50-token answer, because the level never left the
// engine: the fleet provider had no level binding, the fleet request no level
// field, and an ai() expression reached the provider past the router's
// binding altogether. Every assertion here is on the request that LEAVES the
// provider, because the answer that comes back looks the same either way --
// only slower.

import (
	"context"
	"testing"
	"text/template"
	"time"

	"github.com/znasllc-io/memql/core/common"
)

// offTheSharedRateCeiling runs a test's fleet calls past a guard with every
// layer off, restoring the shared one after. A fleet call spends the
// process-wide local rate ceiling (20 calls in 10 s) that the rest of this
// package's fleet tests share, and nothing here is about the guard --
// TestTheFleetProviderItselfPassesTheGuard swaps it the same way.
func offTheSharedRateCeiling(t *testing.T) {
	t.Helper()
	prev := sharedLLMGuard
	t.Cleanup(func() { sharedLLMGuard = prev })
	sharedLLMGuard = &llmGuard{now: time.Now, logger: testGuardLogger()}
}

// The level is stamped in call(), which every surface goes through; one
// surface proves it, and the bindings are read off the provider.
func TestAFleetCallCarriesTheLevelItWasBoundAt(t *testing.T) {
	offTheSharedRateCeiling(t)
	fleet := &stubFleet{answer: "{}", models: []FleetModel{onlineModel("qwen3.5:4b", true)}}
	r := newProviderRegistry()
	r.SetFleetInference(fleet)
	p := &fleetProvider{registry: r, modelId: "qwen3.5:4b", actingUserId: "alice"}
	messages := []common.ChatMessage{{Role: "user", Content: "classify"}}

	if _, err := p.WithLevel("fast").(common.ChatAIProvider).CallChat(userCtx("alice"), messages); err != nil {
		t.Fatal(err)
	}
	if fleet.lastReq.Level != "fast" {
		t.Fatalf("a call bound at fast left with level %q", fleet.lastReq.Level)
	}
	// The registry entry is shared, so a binding is one call's: the unbound
	// provider still names no level, which the cockpit reads as "run at the
	// runtime's own defaults".
	if _, err := p.CallChat(userCtx("alice"), messages); err != nil {
		t.Fatal(err)
	}
	if fleet.lastReq.Level != "" {
		t.Fatalf("binding a level changed the shared provider: an unbound call left with %q", fleet.lastReq.Level)
	}
}

// The router binds the floor, the level and the effort one after another, in
// an order nothing promises. Each binding has to carry the others forward, or
// whichever runs last silently drops the rest.
func TestFleetBindingsCarryEachOtherForward(t *testing.T) {
	p := &fleetProvider{modelId: "qwen3.5:4b", actingUserId: "alice", selector: FleetSelectorFastest}
	orders := map[string]any{
		"floor, level, effort": p.WithMinContextTokens(40000).(*fleetProvider).WithLevel("fast").(*fleetProvider).WithEffort("low"),
		"effort, level, floor": p.WithEffort("low").(*fleetProvider).WithLevel("fast").(*fleetProvider).WithMinContextTokens(40000),
		"level, effort, floor": p.WithLevel("fast").(*fleetProvider).WithEffort("low").(*fleetProvider).WithMinContextTokens(40000),
	}
	for order, bound := range orders {
		got := bound.(*fleetProvider)
		if got.level != "fast" || got.effort != "low" || got.minContextTokens != 40000 {
			t.Errorf("%s: level=%q effort=%q floor=%d, want fast/low/40000", order, got.level, got.effort, got.minContextTokens)
		}
		if got.modelId != p.modelId || got.actingUserId != p.actingUserId || got.selector != p.selector {
			t.Errorf("%s: the binding lost the provider's identity: %+v", order, got)
		}
	}
	if p.level != "" || p.effort != "" || p.minContextTokens != 0 {
		t.Fatalf("binding mutated the shared provider: %+v", p)
	}
}

// An ai() expression -- how goalComplexityTriage reaches a model -- calls the
// provider RECORD the router resolved, not the router's bound client. The
// level the prompt declares has to reach that call too.
func TestAnAIExpressionCarriesItsLevelToTheFleet(t *testing.T) {
	offTheSharedRateCeiling(t)
	fleet := &stubFleet{answer: `{"complexity":"trivial"}`, models: []FleetModel{onlineModel("qwen3.5:4b", true)}}
	providers := newProviderRegistry()
	providers.SetFleetInference(fleet)

	source := "Classify {{.goal}}."
	tmpl, err := template.New("triage").Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	prompts := newPromptRegistry()
	prompts.set(&PromptTemplate{
		Name: "triage", Level: "fast", TemplateSource: source,
		DefaultProvider: "fleet:qwen3.5:4b", tmpl: tmpl,
	})
	e := &MemQLEngine{providers: providers, prompts: prompts, modelSeam: &modelSeam{}}
	e.aiRuntime = newTestAIRuntime(prompts, providers, aiCacheConfig{})
	e.aiRuntime.seam = e.modelSeam

	if _, err := e.InvokeAI(userCtx("alice"), "triage", map[string]any{"goal": "hi"}); err != nil {
		t.Fatal(err)
	}
	if fleet.lastReq.Level != "fast" {
		t.Fatalf("an ai() call to a fast prompt reached the machine with level %q", fleet.lastReq.Level)
	}

	// A person's re-run at another level is the level that leaves, not the
	// prompt's own: the binding reads the RESOLVED request.
	ctx := common.ContextWithRun(userCtx("alice"), common.RunContext{
		RunId: "v1:work:run:r1", GoalId: "v1:work:goal:g1", StepKey: "triage",
		OwnerUserId: "alice", Mode: common.RunModeLive, Override: &common.StepOverride{Level: "strong"},
	})
	if _, err := e.InvokeAI(ctx, "triage", map[string]any{"goal": "hi again"}); err != nil {
		t.Fatal(err)
	}
	if fleet.lastReq.Level != "strong" {
		t.Fatalf("a step re-run at strong reached the machine with level %q", fleet.lastReq.Level)
	}
}

// A provider with no level knob -- a vendor record -- is called exactly as it
// was: binding is offered, never required.
func TestAnAIExpressionOnAProviderWithoutALevelKnobIsUnchanged(t *testing.T) {
	model := &capturingModel{}
	providers := newProviderRegistry()
	providers.RegisterForTest("chat54Mini", "openai", "gpt-5.4-mini", model)
	source := "Classify {{.goal}}."
	tmpl, err := template.New("triage").Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	prompts := newPromptRegistry()
	prompts.set(&PromptTemplate{Name: "triage", Level: "fast", TemplateSource: source, DefaultProvider: "chat54Mini", tmpl: tmpl})
	e := &MemQLEngine{providers: providers, prompts: prompts, modelSeam: &modelSeam{}}
	e.aiRuntime = newTestAIRuntime(prompts, providers, aiCacheConfig{})
	e.aiRuntime.seam = e.modelSeam

	if _, err := e.InvokeAI(context.Background(), "triage", map[string]any{"goal": "hi"}); err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 || model.prompt != "Classify hi." {
		t.Fatalf("the vendor call changed: calls=%d prompt=%q", model.calls, model.prompt)
	}
}
