package memql

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
)

// stubFleet is an in-process FleetInference.
type stubFleet struct {
	models    []FleetModel
	lastReq   FleetCallRequest
	lastActor string
	answer    string
	err       error
}

func (s *stubFleet) Catalog(_ context.Context, actingUserId string) ([]FleetModel, error) {
	s.lastActor = actingUserId
	return s.models, nil
}

func (s *stubFleet) Call(_ context.Context, req FleetCallRequest) (FleetCallResult, error) {
	s.lastReq = req
	if s.err != nil {
		return FleetCallResult{}, s.err
	}
	if req.OnDelta != nil {
		req.OnDelta(s.answer)
	}
	return FleetCallResult{
		Content:          s.answer,
		Usage:            FleetUsage{InputTokens: 3, OutputTokens: 5, Known: true, Model: req.ModelId},
		ExecutionSurface: "fleet:laptop",
		MachineLabel:     "Laptop",
	}, nil
}

func onlineModel(id string, structured bool) FleetModel {
	return FleetModel{
		ModelId:          id,
		ContextWindow:    131072,
		StructuredOutput: structured,
		Machines:         []FleetMachine{{RegistrationId: "laptop", Name: "laptop", Online: true}},
	}
}

func userCtx(userId string) context.Context {
	return auth.ContextWithUserActor(context.Background(), userId)
}

// A `fleet:<modelId>` name resolves DYNAMICALLY through Entry. Doing it here
// rather than at load is what lets an asleep fleet boot and a waking machine
// become usable with no reload.
func TestFleetReferenceResolvesThroughTheRegistryEntry(t *testing.T) {
	r := newProviderRegistry("")
	r.SetFleetInference(&stubFleet{models: []FleetModel{onlineModel("llama3.1:8b", true)}, answer: "hello"})

	entry, ok := r.EntryForContext(userCtx("alice"), "fleet:llama3.1:8b")
	if !ok {
		t.Fatal("a fleet reference must resolve to an entry")
	}
	if !entry.Available {
		t.Fatalf("entry should be available: %v", entry.err)
	}
	if _, isChat := entry.Client.(common.ChatAIProvider); !isChat {
		t.Fatal("the fleet client must satisfy ChatAIProvider so the existing accessors reach it")
	}
	if _, isStructured := entry.Client.(common.ChatStructuredProvider); !isStructured {
		t.Fatal("the fleet client must satisfy ChatStructuredProvider")
	}
	if _, isEmbed := entry.Client.(EmbeddingAIProvider); !isEmbed {
		t.Fatal("the fleet client must satisfy EmbeddingAIProvider")
	}
}

// An unavailable fleet model behaves EXACTLY like a disabled provider: the
// entry resolves, reports unavailable, and the policy's authored fallback
// fires with no special case anywhere.
func TestAnOfflineFleetModelIsUnavailableRatherThanMissing(t *testing.T) {
	offline := onlineModel("llama3.1:8b", true)
	offline.Machines[0].Online = false
	r := newProviderRegistry("")
	r.SetFleetInference(&stubFleet{models: []FleetModel{offline}})

	entry, ok := r.EntryForContext(userCtx("alice"), "fleet:llama3.1:8b")
	if !ok {
		t.Fatal("an offline fleet model must still RESOLVE -- 'missing' and 'asleep' are " +
			"different answers and only one of them is true")
	}
	if entry.Available {
		t.Fatal("a model with no online machine must not be available")
	}
	// The existing accessors must see it as unusable, exactly as they see a
	// disabled provider.
	if p := r.ChatStructuredProviderByName("fleet:llama3.1:8b"); p != nil {
		t.Fatal("an unavailable fleet entry must not be handed out by the by-name accessor")
	}
}

// A node with no worker service has an UNAVAILABLE fleet, not a broken one:
// the same state as "no machine is awake", flowing through the same path.
func TestANodeWithNoFleetInferenceHasAnUnavailableFleet(t *testing.T) {
	r := newProviderRegistry("")
	if r.FleetInferenceInstalled() {
		t.Fatal("a fresh registry has no fleet inference")
	}
	entry, ok := r.EntryForContext(context.Background(), "fleet:llama3.1:8b")
	if !ok || entry.Available {
		t.Fatalf("expected a resolved-but-unavailable entry, got ok=%v available=%v", ok, entry.Available)
	}
	if entry.err == nil || !strings.Contains(entry.err.Error(), "fleet inference") {
		t.Fatalf("the reason must say this node has none: %v", entry.err)
	}
}

// A name that is not a fleet reference must not become one.
func TestOnlyAFleetPrefixResolvesDynamically(t *testing.T) {
	r := newProviderRegistry("")
	r.SetFleetInference(&stubFleet{models: []FleetModel{onlineModel("llama3.1:8b", true)}})
	for _, name := range []string{"streamClaudeSonnet", "fleet", "fleet:", " ", "notfleet:llama3.1:8b"} {
		if _, ok := r.EntryForContext(context.Background(), name); ok {
			t.Fatalf("%q must not resolve as a fleet reference", name)
		}
	}
}

// The acting user decides whose machines are eligible, and it comes from the
// CONTEXT because the provider interfaces have nowhere to carry it.
func TestTheActingUserComesFromTheCallContext(t *testing.T) {
	stub := &stubFleet{models: []FleetModel{onlineModel("llama3.1:8b", true)}, answer: "hi"}
	r := newProviderRegistry("")
	r.SetFleetInference(stub)

	entry, _ := r.EntryForContext(userCtx("v1:identity:user:alice"), "fleet:llama3.1:8b")
	chat := entry.Client.(common.ChatAIProvider)
	if _, err := chat.CallChat(userCtx("v1:identity:user:alice"), []common.ChatMessage{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("CallChat: %v", err)
	}
	if stub.lastReq.ActingUserId != "v1:identity:user:alice" {
		t.Fatalf("acting user = %q, want the context's", stub.lastReq.ActingUserId)
	}
}

// A context with no access carries no acting user, which is SYSTEM WORK. That
// reading narrows the fleet (opted-in machines only) rather than widening it;
// the opposite default would be the cross-user routing memql#4678 prevents.
func TestNoAccessContextMeansSystemWorkNotEveryUser(t *testing.T) {
	stub := &stubFleet{models: []FleetModel{onlineModel("llama3.1:8b", true)}, answer: "hi"}
	r := newProviderRegistry("")
	r.SetFleetInference(stub)
	if _, err := r.FleetCatalog(context.Background(), ""); err != nil {
		t.Fatalf("FleetCatalog: %v", err)
	}
	if stub.lastActor != "" {
		t.Fatalf("actor = %q, want empty (system work), never a stand-in user", stub.lastActor)
	}
}

// The structured path sends the SCHEMA to the runtime rather than appending it
// to the prompt: the router only routes here when the machine advertised the
// capability, so a runtime returning prose has broken its own advertisement.
func TestStructuredCallsCarryTheSchemaToTheRuntime(t *testing.T) {
	stub := &stubFleet{models: []FleetModel{onlineModel("llama3.1:8b", true)}, answer: `{"ok":true}`}
	r := newProviderRegistry("")
	r.SetFleetInference(stub)

	entry, _ := r.EntryForContext(userCtx("alice"), "fleet:llama3.1:8b")
	sp := entry.Client.(common.ChatStructuredProvider)
	schema := common.StructuredSchema{Name: "t", Schema: []byte(`{"type":"object"}`), Strict: true}
	out, err := sp.CallChatStructured(userCtx("alice"), []common.ChatMessage{{Role: "user", Content: "go"}}, schema)
	if err != nil {
		t.Fatalf("CallChatStructured: %v", err)
	}
	if out != `{"ok":true}` {
		t.Fatalf("out = %q", out)
	}
	if stub.lastReq.Schema == nil || string(stub.lastReq.Schema.Schema) != `{"type":"object"}` {
		t.Fatalf("the schema must reach the runtime, got %+v", stub.lastReq.Schema)
	}
}

// A vector count that does not match the input count cannot be matched back
// up, and guessing the alignment attaches the wrong meaning to a row that then
// looks correct forever.
func TestMismatchedEmbeddingCountsAreRefusedRatherThanAligned(t *testing.T) {
	r := newProviderRegistry("")
	r.SetFleetInference(&stubFleet{models: []FleetModel{{
		ModelId:  "nomic-embed-text",
		Machines: []FleetMachine{{RegistrationId: "laptop", Online: true}},
	}}})
	entry, _ := r.EntryForContext(userCtx("alice"), "fleet:nomic-embed-text")
	ep := entry.Client.(EmbeddingAIProvider)
	if _, err := ep.EmbedBatch(userCtx("alice"), []string{"a", "b"}); err == nil {
		t.Fatal("a runtime returning no vectors for two inputs must be an error, not a guess")
	}
}

// The provider surfaces return a bare string, so the machine and usage have to
// be read back -- memql#4681 stamps the ledger from this.
func TestTheLastCallReportsItsMachineAndUsage(t *testing.T) {
	r := newProviderRegistry("")
	r.SetFleetInference(&stubFleet{models: []FleetModel{onlineModel("llama3.1:8b", true)}, answer: "hey"})
	entry, _ := r.EntryForContext(userCtx("alice"), "fleet:llama3.1:8b")
	chat := entry.Client.(common.ChatAIProvider)
	if _, err := chat.CallChat(userCtx("alice"), []common.ChatMessage{{Role: "user", Content: "x"}}); err != nil {
		t.Fatalf("CallChat: %v", err)
	}
	surface, usage := entry.Client.(*fleetProvider).LastCall()
	if surface != "fleet:laptop" {
		t.Fatalf("surface = %q", surface)
	}
	if !usage.Known || usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v", usage)
	}
}

// A fleet call that fails must read as unavailable so the chain falls through.
func TestAFailedFleetCallReadsAsUnavailable(t *testing.T) {
	r := newProviderRegistry("")
	r.SetFleetInference(&stubFleet{
		models: []FleetModel{onlineModel("llama3.1:8b", true)},
		err:    ErrFleetUnavailable,
	})
	entry, _ := r.EntryForContext(userCtx("alice"), "fleet:llama3.1:8b")
	chat := entry.Client.(common.ChatAIProvider)
	_, err := chat.CallChat(userCtx("alice"), []common.ChatMessage{{Role: "user", Content: "x"}})
	if !errors.Is(err, ErrFleetUnavailable) {
		t.Fatalf("err = %v, want the unavailable sentinel", err)
	}
}

// The tree must contain no static per-model fleet providers: a fleet model
// exists while a laptop is awake, so a registry entry written at load would be
// a claim nothing can keep.
func TestTheFleetProviderHasNoStaticPerModelChildren(t *testing.T) {
	_, err := newAIProvider(ProviderConfig{Name: "fleetLlama", Type: FleetProviderType, Model: "llama3.1:8b"})
	if err == nil {
		t.Fatal("a static per-model fleet provider must be refused")
	}
	if !strings.Contains(err.Error(), "fleet:llama3.1:8b") {
		t.Fatalf("the refusal must name the syntax that works, got %v", err)
	}
}

// A prompt whose @defaultProvider is a fleet model must not refuse BOOT. A
// fleet model is not declared anywhere, because it exists only while a machine
// hosting it is awake -- which is the whole reason selection-time resolution
// exists.
func TestAFleetDefaultProviderDoesNotRefuseBoot(t *testing.T) {
	prompts := newPromptRegistryForTest(map[string]string{
		"plannerAgent": "fleet:llama3.1:8b",
		"conductor":    "streamClaudeSonnet",
	})
	providers := newProviderRegistry("")
	providers.markDeclared("streamClaudeSonnet")

	if err := ValidatePromptDefaultProviders(prompts, providers); err != nil {
		t.Fatalf("a fleet default provider must load: %v", err)
	}
}

// The exemption is for the `fleet:` prefix only. A genuine typo still refuses,
// which is what this gate exists for.
func TestATypoedDefaultProviderStillRefusesBoot(t *testing.T) {
	prompts := newPromptRegistryForTest(map[string]string{"conductor": "streamCloudeSonnet"})
	providers := newProviderRegistry("")
	providers.markDeclared("streamClaudeSonnet")

	if err := ValidatePromptDefaultProviders(prompts, providers); err == nil {
		t.Fatal("a misspelled provider name must still refuse the load")
	}
}

// THE PARK PATH ON THE PROMPT SEAM (memql#4682). An unavailable fleet model
// reached through InvokeAI must produce the TYPED refusal, because that is
// what the planner matches on: a generic error FAILS the plan where the typed
// one parks it and resumes when a machine wakes.
func TestAnUnavailableFleetPromptProviderYieldsTheTypedRefusal(t *testing.T) {
	offline := onlineModel("llama3.1:8b", true)
	offline.Machines[0].Online = false
	r := newProviderRegistry("")
	r.SetFleetInference(&stubFleet{models: []FleetModel{offline}})

	refusal := r.FleetRefusal(userCtx("alice"), "alice", "llama3.1:8b")
	if refusal == nil {
		t.Fatal("expected a refusal")
	}
	if refusal.Code() != FeedbackReasonNoLocalModel {
		t.Fatalf("code = %q", refusal.Code())
	}
	if refusal.Considered["laptop"] != "offline" {
		t.Fatalf("the refusal must name the machine and why: %v", refusal.Considered)
	}
	if !errors.Is(error(refusal), ErrFleetUnavailable) {
		t.Fatal("the refusal must read as unavailable")
	}
}

// newPromptRegistryForTest builds a registry from name -> @defaultProvider.
func newPromptRegistryForTest(defaults map[string]string) *PromptRegistry {
	r := newPromptRegistry()
	for name, provider := range defaults {
		r.byName[name] = &PromptTemplate{Name: name, DefaultProvider: provider}
	}
	return r
}
