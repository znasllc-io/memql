package memql

// A person's override for ONE step, at the model seam (epic memql#5414,
// design D20). Every assertion here is about what the call that leaves the
// seam carries -- the level asked for, the pin, the effort, the conversation --
// because the answer that comes back looks the same whether or not the
// override reached the model.

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"text/template"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// capturingModel answers every surface an override can reach and records the
// conversation it was handed.
type capturingModel struct {
	calls    int
	prompt   string
	messages []common.ChatMessage
}

func (c *capturingModel) Call(_ context.Context, prompt string) (any, error) {
	c.calls++
	c.prompt = prompt
	return "the draft", nil
}

func (c *capturingModel) CallChatStructured(_ context.Context, messages []common.ChatMessage, _ common.StructuredSchema) (string, error) {
	c.calls++
	c.messages = append([]common.ChatMessage(nil), messages...)
	return `{"draft":"the draft"}`, nil
}

// overrideSeamEngine is the smallest engine that runs a prompt through the
// seam. The prompt declares `fast` and pins its own provider, so a request that
// leaves at another level or on another pin was changed by the override and by
// nothing else. The resolver records every request it is asked to resolve.
func overrideSeamEngine(t *testing.T, cache aiCacheConfig) (*MemQLEngine, *capturingModel, *[]airoute.ResolveRequest) {
	t.Helper()
	model := &capturingModel{}
	providers := newProviderRegistry()
	providers.RegisterForTest("chat54Mini", "openai", "gpt-5.4-mini", model)
	providers.RegisterForTest("personPinned", "openai", "gpt-5.4", model)

	source := "Draft the weekly report for {{.week}}."
	tmpl, err := template.New("draftReport").Parse(source)
	if err != nil {
		t.Fatalf("compile the fixture template: %v", err)
	}
	prompts := newPromptRegistry()
	prompts.set(&PromptTemplate{
		Name: "draftReport", Level: "fast", TemplateSource: source,
		DefaultProvider: "chat54Mini", tmpl: tmpl,
	})

	e := &MemQLEngine{providers: providers, prompts: prompts, modelSeam: &modelSeam{}}
	e.aiRuntime = newTestAIRuntime(prompts, providers, cache)
	e.aiRuntime.seam = e.modelSeam
	seen := &[]airoute.ResolveRequest{}
	resolve := testRegistryResolver(providers)
	record := func(ctx context.Context, req airoute.ResolveRequest) (ResolvedProvider, error) {
		*seen = append(*seen, req)
		return resolve(ctx, req)
	}
	e.aiRuntime.resolve = record
	e.SetAIResolver(testAIResolver{fn: record})
	return e, model, seen
}

// stepCtx is the context of step `draft`, carrying ov as the executor sets it
// on the one step a re-run targets.
func stepCtx(ov *common.StepOverride) context.Context {
	return common.ContextWithRun(userCtx("alice"), common.RunContext{
		RunId: "v1:work:run:r1", GoalId: "v1:work:goal:g1", StepKey: "draft",
		OwnerUserId: "alice", Mode: common.RunModeLive, Override: ov,
	})
}

func draftPrompt() *PromptTemplate {
	return &PromptTemplate{Name: "draftReport", Level: "fast", DefaultProvider: "chat54Mini"}
}

var draftSchema = json.RawMessage(`{"type":"object"}`)

func TestALevelOverrideChangesTheRequestedLevel(t *testing.T) {
	req, err := requestForPrompt(stepCtx(&common.StepOverride{Level: "reasoning"}), draftPrompt(), nil, airoute.ModalityChat, "draft it")
	if err != nil {
		t.Fatal(err)
	}
	if req.Level != airoute.LevelReasoning {
		t.Fatalf("level = %q, want the level the person asked for", req.Level)
	}
	// A level clears no pin: the door stays the one the prompt names, and the
	// level binds on it. Clearing it is how a level could send a step pinned
	// to somebody's own app to a vendor they never named.
	if req.ExplicitProvider != "chat54Mini" {
		t.Fatalf("pin = %q, want the prompt's own pin kept", req.ExplicitProvider)
	}

	// The control: the same prompt on a step nobody overrode keeps its level.
	plain, err := requestForPrompt(stepCtx(nil), draftPrompt(), nil, airoute.ModalityChat, "draft it")
	if err != nil {
		t.Fatal(err)
	}
	if plain.Level != airoute.LevelFast {
		t.Fatalf("an unoverridden step asked for %q, want the prompt's own level", plain.Level)
	}

	// Embeddings is a level a call declares, never one a person re-runs a
	// step at, and an unknown word is refused the same way -- naming the step.
	for _, bad := range []string{"embeddings", "smart"} {
		_, err := requestForPrompt(stepCtx(&common.StepOverride{Level: bad}), draftPrompt(), nil, airoute.ModalityChat, "draft it")
		if err == nil || !strings.Contains(err.Error(), `"draft"`) || !strings.Contains(err.Error(), "fast, strong or reasoning") {
			t.Fatalf("level %q: err = %v, want a refusal naming the step and the three levels", bad, err)
		}
	}

	// And an EMBEDDING request is never overridden, whatever the step asks:
	// a vector from another embedder belongs to another space (design D10).
	embed, err := ApplyStepOverride(stepCtx(&common.StepOverride{Level: "reasoning", Model: "personPinned", Effort: "high"}),
		airoute.ResolveRequest{Level: airoute.LevelEmbeddings, Modality: airoute.ModalityEmbedding})
	if err != nil {
		t.Fatal(err)
	}
	if embed.Level != airoute.LevelEmbeddings || embed.ExplicitProvider != "" || embed.Effort != "" {
		t.Fatalf("an embedding request was overridden: %+v", embed)
	}
}

// The model a person pins outranks both pins the call already had: the
// prompt's @defaultProvider and a caller's context override. The router walks
// ExplicitProvider as a one-entry chain; component/router's
// TestAModelOverrideIsAOneEntryChain holds that half.
func TestAModelOverrideOutranksEveryOtherPin(t *testing.T) {
	askPinned := "askPinned"
	req, err := requestForPrompt(stepCtx(&common.StepOverride{Model: "app:claude-code:opus", Effort: "high"}), draftPrompt(),
		&AIInvocation{TemplateId: "draftReport", ProviderOverride: &askPinned}, airoute.ModalityChat, "draft it")
	if err != nil {
		t.Fatal(err)
	}
	if req.ExplicitProvider != "app:claude-code:opus" {
		t.Fatalf("pin = %q, want the model the person named", req.ExplicitProvider)
	}
	if req.Effort != "high" {
		t.Fatalf("effort = %q, want the person's", req.Effort)
	}
}

func TestPromptAndGuidanceRideAsTwoMessages(t *testing.T) {
	e, model, _ := overrideSeamEngine(t, aiCacheConfig{})
	ov := &common.StepOverride{
		Prompt:         "Put the totals in bold.",
		GuidanceAxes:   []string{"product", "process"},
		GuidanceReason: "the totals are missing",
	}
	callerMessages := []common.ChatMessage{{Role: "system", Content: "compose the file"}, {Role: "user", Content: "the weekly report"}}
	if _, err := e.CallAIStructured(stepCtx(ov), airoute.ResolveRequest{Level: airoute.LevelStrong, ExplicitProvider: "chat54Mini"},
		callerMessages, common.StructuredSchema{Name: "draft", Schema: draftSchema}); err != nil {
		t.Fatal(err)
	}
	want := []common.ChatMessage{
		{Role: "system", Content: "compose the file"},
		{Role: "user", Content: "the weekly report"},
		{Role: "user", Content: "Instructions for this step from its owner:\nPut the totals in bold."},
		{Role: "user", Content: "What was wrong with the previous version (product, process): the totals are missing"},
	}
	if !reflect.DeepEqual(model.messages, want) {
		t.Fatalf("the model was handed:\n%+v\nwant the caller's messages, then the instructions, then the guidance:\n%+v", model.messages, want)
	}
	if len(callerMessages) != 2 {
		t.Fatalf("the caller's own slice was changed: %+v", callerMessages)
	}
}

// Each message is absent when the override does not carry it, and a dislike
// that named axes but no reason ends at its parenthetical rather than at a
// colon with nothing after it.
func TestGuidanceSaysWhatTheDislikeSaid(t *testing.T) {
	for _, tc := range []struct {
		ov   common.StepOverride
		want string
	}{
		{common.StepOverride{GuidanceAxes: []string{"performance"}, GuidanceReason: "it took an hour"},
			"What was wrong with the previous version (performance): it took an hour"},
		{common.StepOverride{GuidanceReason: "it took an hour"}, "What was wrong with the previous version: it took an hour"},
		{common.StepOverride{GuidanceAxes: []string{"product"}}, "What was wrong with the previous version (product)."},
		{common.StepOverride{Level: "strong"}, ""},
	} {
		if got := StepOverrideGuidance(&tc.ov); got != tc.want {
			t.Errorf("guidance for %+v = %q, want %q", tc.ov, got, tc.want)
		}
	}
	if got := StepOverrideMessages(stepCtx(&common.StepOverride{Level: "strong"})); len(got) != 0 {
		t.Fatalf("an override that changes only the level adds no message, got %+v", got)
	}
}

// An ai() expression is a ONE-STRING call, so the same two messages ride its
// one prompt as paragraphs, in the same order.
func TestAnAIExpressionCarriesTheInstructionsInItsOnePrompt(t *testing.T) {
	e, model, _ := overrideSeamEngine(t, aiCacheConfig{})
	ov := &common.StepOverride{Prompt: "Put the totals in bold.", GuidanceAxes: []string{"product"}, GuidanceReason: "the totals are missing"}
	if _, err := e.InvokeAI(stepCtx(ov), "draftReport", map[string]any{"week": "38"}); err != nil {
		t.Fatal(err)
	}
	want := "Draft the weekly report for 38.\n\n" +
		"Instructions for this step from its owner:\nPut the totals in bold.\n\n" +
		"What was wrong with the previous version (product): the totals are missing"
	if model.prompt != want {
		t.Fatalf("prompt = %q\nwant %q", model.prompt, want)
	}
}

// Only the context's own override is read. Every step a re-run does not target
// carries a run context with none -- which is how the executor keeps an
// override from leaking into the next step -- and such a context must leave the
// call exactly as a call outside any run would be.
func TestAnOverrideOnAnotherStepIsIgnored(t *testing.T) {
	next := common.ContextWithRun(userCtx("alice"), common.RunContext{
		RunId: "v1:work:run:r1", GoalId: "v1:work:goal:g1", StepKey: "publish",
		OwnerUserId: "alice", Mode: common.RunModeLive,
	})
	req, err := requestForPrompt(next, draftPrompt(), nil, airoute.ModalityChat, "draft it")
	if err != nil {
		t.Fatal(err)
	}
	outside, err := requestForPrompt(userCtx("alice"), draftPrompt(), nil, airoute.ModalityChat, "draft it")
	if err != nil {
		t.Fatal(err)
	}
	if req.Level != outside.Level || req.ExplicitProvider != outside.ExplicitProvider || req.Effort != outside.Effort {
		t.Fatalf("a step with no override was changed: %+v vs %+v", req, outside)
	}
	if msgs := StepOverrideMessages(next); len(msgs) != 0 {
		t.Fatalf("a step with no override was handed %d extra message(s)", len(msgs))
	}

	e, model, _ := overrideSeamEngine(t, aiCacheConfig{})
	if _, err := e.InvokeAIStructured(next, "draftReport", map[string]any{"week": "38"}, "draft", draftSchema, true); err != nil {
		t.Fatal(err)
	}
	if len(model.messages) != 2 {
		t.Fatalf("a step with no override was handed %d message(s), want the prompt and its request only: %+v",
			len(model.messages), model.messages)
	}
}

func TestInvokeAIStructuredHonoursTheOverride(t *testing.T) {
	e, model, seen := overrideSeamEngine(t, aiCacheConfig{})
	ov := &common.StepOverride{
		Level: "reasoning", Model: "personPinned", Effort: "high",
		Prompt: "Put the totals in bold.", GuidanceAxes: []string{"process"}, GuidanceReason: "it guessed the week",
	}
	if _, err := e.InvokeAIStructured(stepCtx(ov), "draftReport", map[string]any{"week": "38"}, "draft", draftSchema, true); err != nil {
		t.Fatal(err)
	}
	if len(*seen) == 0 {
		t.Fatal("the resolver was never asked, so this test measures nothing")
	}
	got := (*seen)[len(*seen)-1]
	if got.Level != airoute.LevelReasoning || got.ExplicitProvider != "personPinned" || got.Effort != "high" {
		t.Fatalf("the structured call was resolved with %+v, want the person's level, model and effort", got)
	}
	if n := len(model.messages); n != 4 ||
		model.messages[2].Content != "Instructions for this step from its owner:\nPut the totals in bold." ||
		model.messages[3].Content != "What was wrong with the previous version (process): it guessed the week" {
		t.Fatalf("the structured call was handed %+v, want the prompt, its request, the instructions and the guidance", model.messages)
	}

	// The control: the same call on a step nobody overrode resolves at the
	// prompt's own level on its own pin, with nothing appended.
	*seen = nil
	if _, err := e.InvokeAIStructured(stepCtx(nil), "draftReport", map[string]any{"week": "38"}, "draft", draftSchema, true); err != nil {
		t.Fatal(err)
	}
	plain := (*seen)[len(*seen)-1]
	if plain.Level != airoute.LevelFast || plain.ExplicitProvider != "chat54Mini" || plain.Effort != "" {
		t.Fatalf("an unoverridden call was resolved with %+v", plain)
	}
	if len(model.messages) != 2 {
		t.Fatalf("an unoverridden call was handed %d message(s)", len(model.messages))
	}
}

// A step a person asked to run again is never answered from the cache, and its
// answer is not cached: a warm answer is the previous version's, and the key
// cannot see the override. "Run it again" with nothing changed counts too.
func TestARerunStepIsNeverServedFromTheCache(t *testing.T) {
	e, model, _ := overrideSeamEngine(t, aiCacheConfig{DefaultEnabled: true, MaxTTLSeconds: 300})
	data := map[string]any{"week": "38"}

	// The control: the cache is live -- an identical call is served warm.
	for i := 0; i < 2; i++ {
		if _, err := e.InvokeAI(stepCtx(nil), "draftReport", data); err != nil {
			t.Fatal(err)
		}
	}
	if model.calls != 1 {
		t.Fatalf("the cache did not serve the repeat (calls=%d), so the assertion below would prove nothing", model.calls)
	}

	rerun := &common.StepOverride{RequestedBy: "alice"}
	for i := 0; i < 2; i++ {
		if _, err := e.InvokeAI(stepCtx(rerun), "draftReport", data); err != nil {
			t.Fatal(err)
		}
	}
	if model.calls != 3 {
		t.Fatalf("calls = %d after two re-run calls, want both to reach the model", model.calls)
	}

	for i := 0; i < 2; i++ {
		if _, err := e.InvokeAIStructured(stepCtx(rerun), "draftReport", data, "draft", draftSchema, true); err != nil {
			t.Fatal(err)
		}
	}
	if model.calls != 5 {
		t.Fatalf("calls = %d after two structured re-run calls, want both to reach the model", model.calls)
	}
}
