package memql

// Strongest first, and what "strongest" means (epic memql#5096, task
// memql#5099, design D5).
//
// These are fold-style tests over a fixture table rather than assertions
// against a running fleet, because the property is a total order and the
// interesting cases are the ties and the absences -- neither of which a live
// catalog produces on demand.

import (
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

func sized(id string, params int64, ctxWindow int) FleetModel {
	m := onlineModel(id, true)
	m.Params = params
	m.ContextWindow = ctxWindow
	return m
}

func ids(models []FleetModel) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ModelId)
	}
	return out
}

func sameOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestOrderModelsRanksByParametersThenContextThenId(t *testing.T) {
	models := []FleetModel{
		sized("qwen2.5:7b", 7_000_000_000, 32768),
		sized("llama3.3:70b", 70_000_000_000, 8192),
		sized("llama3.1:8b", 8_000_000_000, 131072),
		sized("mistral:7b", 7_000_000_000, 32768),
	}
	sameOrder(t, ids(orderModels(models, nil)),
		"llama3.3:70b", // most parameters, despite the smallest window
		"llama3.1:8b",
		// 7B tie: same window, so the id breaks it.
		"mistral:7b",
		"qwen2.5:7b",
	)
}

// A model that does not say how big it is must not WIN by silence. Sorting an
// unknown size first would make a cockpit that predates the attribute the
// fleet's strongest model on every machine it runs on.
func TestAModelThatDoesNotSayItsSizeSortsLast(t *testing.T) {
	unknown := sized("mystery:latest", 0, 999999)
	known := sized("llama3.1:8b", 8_000_000_000, 8192)

	sameOrder(t, ids(orderModels([]FleetModel{unknown, known}, nil)), "llama3.1:8b", "mystery:latest")
	// ...even with an enormous context window, which is the tempting
	// tie-break to reach for and would be the wrong one: a window is not a
	// size, and a runtime that reports one and not the other has said
	// nothing about how capable the model is.
	sameOrder(t, ids(orderModels([]FleetModel{known, unknown}, nil)), "llama3.1:8b", "mystery:latest")
}

func TestAnExplicitPreferenceOutranksSize(t *testing.T) {
	models := []FleetModel{
		sized("llama3.3:70b", 70_000_000_000, 8192),
		sized("qwen2.5:7b", 7_000_000_000, 32768),
		sized("llama3.1:8b", 8_000_000_000, 131072),
	}
	// The owner said: try the 7B first. It is their hardware and their
	// judgement about it, and the size ordering exists for the users who
	// have not made one.
	sameOrder(t, ids(orderModels(models, []string{"qwen2.5:7b"})),
		"qwen2.5:7b", "llama3.3:70b", "llama3.1:8b")

	// A preference ORDERS, it does not filter: the models it does not name
	// stay eligible, ranked among themselves by the default.
	sameOrder(t, ids(orderModels(models, []string{"llama3.1:8b", "qwen2.5:7b"})),
		"llama3.1:8b", "qwen2.5:7b", "llama3.3:70b")

	// A preference naming a model the fleet does not run is inert rather
	// than an error: a laptop that has not pulled it yet must not break
	// routing for the models it has.
	sameOrder(t, ids(orderModels(models, []string{"not-installed:8b"})),
		"llama3.3:70b", "llama3.1:8b", "qwen2.5:7b")
}

func TestOrderModelsIsStableAcrossReplicas(t *testing.T) {
	// Two models identical on every signal. Every replica must order them
	// the same way or `fleet:strongest` resolves differently per node, which
	// is the property the routing strategies already depend on.
	a := sized("a:1b", 0, 0)
	b := sized("b:1b", 0, 0)
	for range 20 {
		sameOrder(t, ids(orderModels([]FleetModel{a, b}, nil)), "a:1b", "b:1b")
		sameOrder(t, ids(orderModels([]FleetModel{b, a}, nil)), "a:1b", "b:1b")
	}
}

// `fleet:*` is RECOGNISED so that it can be REFUSED by name. It resolves
// nothing: the recognizer exists only to carry the replacement into the
// message an author reads.
func TestFleetWildcardIsRecognisedSoItCanBeRefused(t *testing.T) {
	if !IsFleetWildcard(FleetWildcard) {
		t.Error("fleet:* must still be recognised, or the refusal cannot name its replacement")
	}
	for _, name := range []string{"fleet:llama3.1:8b", "fleet:", "*", "chat54Mini", "", FleetStrongest} {
		if IsFleetWildcard(name) {
			t.Errorf("%q must not be read as the wildcard", name)
		}
	}

	r := newProviderRegistry()
	r.SetFleetInference(&stubFleet{models: []FleetModel{sized("llama3.1:8b", 8_000_000_000, 8192)}})
	entry, ok := r.EntryForUser(userCtx("alice"), "alice", FleetWildcard)
	if !ok || entry == nil {
		t.Fatal("fleet:* must still resolve to an ENTRY, so the refusal reaches the door report")
	}
	if entry.Available {
		t.Fatal("fleet:* resolved as available; it is retired")
	}
	// An author who wrote fleet:* meant "the best local model". Telling them
	// only that it is invalid sends them to the source to find out what to
	// write; the message carries both replacements instead.
	msg := entry.Err().Error()
	if !strings.Contains(msg, FleetStrongest) || !strings.Contains(msg, FleetFastest) {
		t.Fatalf("the refusal must name both selectors, got %q", msg)
	}
}

// THE SELECTOR WORDS ARE RESERVED, so a model id can never be mistaken for an
// ordering and an ordering can never be mistaken for a model id.
func TestIsFleetSelectorRecognisesExactlyTheTwoWords(t *testing.T) {
	for name, want := range map[string]string{
		FleetStrongest: FleetSelectorStrongest,
		FleetFastest:   FleetSelectorFastest,
	} {
		got, ok := IsFleetSelector(name)
		if !ok || got != want {
			t.Errorf("IsFleetSelector(%q) = (%q, %v), want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{"fleet:llama3.1:8b", FleetWildcard, "fleet:", "strongest", "chat54Mini", ""} {
		if _, ok := IsFleetSelector(name); ok {
			t.Errorf("%q must not be read as a selector", name)
		}
	}
}

// FASTEST is fewest parameters, and a model that never said how big it is
// sorts LAST here too -- the same rule as strongest rather than its mirror.
// Reading an unknown size as zero would make every silent model the fastest
// thing on the fleet, which is the silence-wins failure arriving from the
// other direction.
func TestOrderModelsFastestRanksFewestParametersMissingLast(t *testing.T) {
	models := []FleetModel{
		sized("big", 70_000_000_000, 8192),
		sized("silent", 0, 131072),
		sized("small", 8_000_000_000, 8192),
	}
	sameOrder(t, ids(orderModelsFastest(models)), "small", "big", "silent")

	// Stable across replicas: two models that tie on every signal come back in
	// the same order however the catalog was handed over.
	a, b := sized("a:1b", 0, 0), sized("b:1b", 0, 0)
	for range 20 {
		sameOrder(t, ids(orderModelsFastest([]FleetModel{a, b})), "a:1b", "b:1b")
		sameOrder(t, ids(orderModelsFastest([]FleetModel{b, a})), "a:1b", "b:1b")
	}
}

// The owner's model preference is deliberately NOT consulted by `fastest`. A
// preference list answers "which model do I want", which is the question
// `strongest` asks; honouring it here would make the two selectors return the
// same model on every fleet whose owner set one -- exactly the fleets where
// the distinction was worth writing down.
func TestOrderModelsFastestIgnoresTheOwnersPreference(t *testing.T) {
	models := []FleetModel{
		sized("big", 70_000_000_000, 8192),
		sized("small", 8_000_000_000, 8192),
	}
	sameOrder(t, ids(orderModels(models, []string{"big"})), "big", "small")
	sameOrder(t, ids(orderModelsFastest(models)), "small", "big")
}

// An unknown selector is an ERROR rather than a default. A defaulted ordering
// is a routing decision nobody wrote: a typo in a policy would silently mean
// "strongest" and the chain would look correct.
func TestOrderBySelectorRefusesAnUnknownSelector(t *testing.T) {
	if _, err := orderBySelector(nil, "quickest", nil); err == nil {
		t.Fatal("an unknown selector was accepted")
	} else if !strings.Contains(err.Error(), FleetSelectorStrongest) ||
		!strings.Contains(err.Error(), FleetSelectorFastest) {
		t.Fatalf("the error must name the selectors that exist, got %q", err)
	}
}

// A SELECTOR is AVAILABLE when any model is online, because the concrete model
// depends on what the call needs and the chain walk is asking a different
// question: is there any local model at all.
func TestASelectorEntryIsAvailableWhenAnyModelIsOnline(t *testing.T) {
	r := newProviderRegistry()
	r.SetFleetInference(&stubFleet{models: []FleetModel{sized("llama3.1:8b", 8_000_000_000, 8192)}})

	entry, ok := r.EntryForUser(userCtx("alice"), "alice", FleetStrongest)
	if !ok || entry == nil {
		t.Fatal("the selector must resolve to an entry")
	}
	if !entry.Available {
		t.Fatalf("want available, got unavailable: %v", entry.Err())
	}

	offline := sized("llama3.1:8b", 8_000_000_000, 8192)
	offline.Machines[0].Online = false
	r2 := newProviderRegistry()
	r2.SetFleetInference(&stubFleet{models: []FleetModel{offline}})
	entry2, _ := r2.EntryForUser(userCtx("alice"), "alice", FleetStrongest)
	if entry2.Available {
		t.Error("a fleet with nothing online must make the selector unavailable")
	}
}

// The concrete model is chosen PER CALL, from what the call needs. A fleet
// running one structured model and one embeddings model serves both kinds of
// turn through the same `fleet:strongest` reference.
func TestTheSelectorPicksTheStrongestModelThatFitsTheCall(t *testing.T) {
	chatBig := sized("llama3.3:70b", 70_000_000_000, 8192)
	chatBig.Embeddings = false
	embed := sized("nomic-embed-text", 137_000_000, 8192)
	embed.Embeddings = true
	embed.StructuredOutput = false

	fleet := &stubFleet{models: []FleetModel{chatBig, embed}, answer: "ok"}
	r := newProviderRegistry()
	r.SetFleetInference(fleet)
	entry, _ := r.EntryForUser(userCtx("alice"), "alice", FleetStrongest)

	chat := entry.Client.(common.ChatAIProvider)
	if _, err := chat.CallChat(userCtx("alice"), []common.ChatMessage{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("chat through the selector: %v", err)
	}
	if fleet.lastReq.ModelId != "llama3.3:70b" {
		t.Errorf("chat ran on %q, want the strongest eligible model", fleet.lastReq.ModelId)
	}

	embedder := entry.Client.(EmbeddingAIProvider)
	if _, err := embedder.EmbedBatch(userCtx("alice"), []string{"x"}); err == nil || fleet.lastReq.ModelId != "nomic-embed-text" {
		// The stub returns no vectors, so EmbedBatch errors on the count --
		// what matters here is WHICH model the call was routed to.
		if fleet.lastReq.ModelId != "nomic-embed-text" {
			t.Errorf("embedding ran on %q, want the model that advertises embeddings", fleet.lastReq.ModelId)
		}
	}
}

// The owner's preference reaches the selector resolver through the seam, so a
// policy edit changes routing with no restart.
func TestTheStrongestSelectorHonoursTheOwnersModelPreference(t *testing.T) {
	fleet := &stubFleet{
		models: []FleetModel{
			sized("llama3.3:70b", 70_000_000_000, 8192),
			sized("qwen2.5:7b", 7_000_000_000, 32768),
		},
		answer:     "ok",
		preference: []string{"qwen2.5:7b"},
	}
	r := newProviderRegistry()
	r.SetFleetInference(fleet)
	entry, _ := r.EntryForUser(userCtx("alice"), "alice", FleetStrongest)

	if _, err := entry.Client.(common.ChatAIProvider).CallChat(
		userCtx("alice"), []common.ChatMessage{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if fleet.lastReq.ModelId != "qwen2.5:7b" {
		t.Errorf("ran on %q, want the owner's preferred model", fleet.lastReq.ModelId)
	}
}

// A failed policy read must not decide the model. Degrading to the default
// ordering gives the caller the strongest eligible model; refusing would park
// a run over a row nobody asked about.
func TestAFailedPreferenceReadFallsBackToTheDefaultOrdering(t *testing.T) {
	fleet := &stubFleet{
		models: []FleetModel{
			sized("qwen2.5:7b", 7_000_000_000, 32768),
			sized("llama3.3:70b", 70_000_000_000, 8192),
		},
		answer:        "ok",
		preferenceErr: errors.New("the policy read failed"),
	}
	r := newProviderRegistry()
	r.SetFleetInference(fleet)
	entry, _ := r.EntryForUser(userCtx("alice"), "alice", FleetStrongest)

	if _, err := entry.Client.(common.ChatAIProvider).CallChat(
		userCtx("alice"), []common.ChatMessage{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if fleet.lastReq.ModelId != "llama3.3:70b" {
		t.Errorf("ran on %q, want the default ordering's answer", fleet.lastReq.ModelId)
	}
}

// A selector whose fleet can serve nothing this call needs is the TYPED
// refusal, naming every MODEL considered -- not every machine. The selector
// asked about models, and a machine-shaped report would name one laptop once
// per model it hosts.
func TestASelectorMissNamesTheModelsConsidered(t *testing.T) {
	proseOnly := sized("tinyllama:1b", 1_000_000_000, 2048)
	proseOnly.StructuredOutput = false
	fleet := &stubFleet{models: []FleetModel{proseOnly}}
	r := newProviderRegistry()
	r.SetFleetInference(fleet)
	entry, _ := r.EntryForUser(userCtx("alice"), "alice", FleetStrongest)

	_, err := entry.Client.(common.ChatStructuredProvider).CallChatStructured(
		userCtx("alice"),
		[]common.ChatMessage{{Role: "user", Content: "decide"}},
		common.StructuredSchema{Name: "decision", Schema: []byte(`{"type":"object"}`)},
	)
	if err == nil {
		t.Fatal("a structured turn on a fleet with no structured model must refuse")
	}
	var refusal *FleetUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("want the typed refusal, got %T: %v", err, err)
	}
	if refusal.Considered["tinyllama:1b"] != "does not advertise structured output" {
		t.Errorf("considered = %v, want the model named with its miss", refusal.Considered)
	}
	if !errors.Is(err, ErrFleetUnavailable) {
		t.Error("the refusal must read as unavailable")
	}
}

func TestEligibilityAsksTheSameQuestionsSatisfiesDoes(t *testing.T) {
	m := onlineModel("llama3.1:8b", true)
	m.Embeddings = true
	m.Tools = true
	m.ContextWindow = 8192

	for _, tt := range []struct {
		name  string
		needs FleetNeeds
		ok    bool
		why   string
	}{
		{"nothing", FleetNeeds{}, true, ""},
		{"structured", FleetNeeds{StructuredOutput: true}, true, ""},
		{"embeddings", FleetNeeds{Embeddings: true}, true, ""},
		{"tools", FleetNeeds{Tools: true}, true, ""},
		{"window under the floor", FleetNeeds{MinContextWindow: 131072}, false, "context window 8192 is under the floor 131072"},
	} {
		ok, why := m.eligibleFor(tt.needs)
		if ok != tt.ok || (!ok && why != tt.why) {
			t.Errorf("%s: ok=%v why=%q, want ok=%v why=%q", tt.name, ok, why, tt.ok, tt.why)
		}
	}

	// An embedding model answers nothing but embeddings (memql#6238): what
	// an embedder advertises -- embeddings, a window, nothing that answers a
	// chat -- never makes it a candidate for a conversation or a review.
	embedder := onlineModel("qwen3-embedding:0.6b", false)
	embedder.Embeddings = true
	embedder.ContextWindow = 32768
	for _, tt := range []struct {
		name  string
		needs FleetNeeds
		ok    bool
		why   string
	}{
		{"conversation", FleetNeeds{}, false, "an embedding model answers embedding calls only"},
		{"structured", FleetNeeds{StructuredOutput: true}, false, "an embedding model answers embedding calls only"},
		{"embeddings", FleetNeeds{Embeddings: true}, true, ""},
	} {
		ok, why := embedder.eligibleFor(tt.needs)
		if ok != tt.ok || (!ok && why != tt.why) {
			t.Errorf("embedder %s: ok=%v why=%q, want ok=%v why=%q", tt.name, ok, why, tt.ok, tt.why)
		}
	}

	offline := m
	offline.Machines = []FleetMachine{{RegistrationId: "laptop", Online: false}}
	if ok, why := offline.eligibleFor(FleetNeeds{}); ok || why != "no machine offering it is online" {
		t.Errorf("offline: ok=%v why=%q", ok, why)
	}
}
