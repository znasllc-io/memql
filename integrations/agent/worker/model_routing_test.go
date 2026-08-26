//go:build agent

package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	workerservice "github.com/znasllc-io/memql/component/worker"
)

// sharedFleet adds the cross-owner read to the fixture store. It is a
// SEPARATE type from fakeFleet for the reason SharedFleetStore is a separate
// interface: a fixture that answered the cross-owner question for every test
// would let a user-scoped path pass its ownership assertion for the wrong
// reason.
type sharedFleet struct {
	*fakeFleet
	all    []Candidate
	allErr error
}

func (f *sharedFleet) SharedInferenceWorkers(context.Context) ([]Candidate, error) {
	if f.allErr != nil {
		return nil, f.allErr
	}
	out := make([]Candidate, len(f.all))
	copy(out, f.all)
	return out, nil
}

const (
	smallModel = "llama3.1:8b"
	bigModel   = "qwen2.5:72b"
	embedModel = "nomic-embed-text"
)

// modelMachine builds a candidate advertising models with attributes.
func modelMachine(id string, models map[string]ModelAttributes, opts ...func(*Candidate)) Candidate {
	c := machine(id)
	c.Capabilities = []string{workerservice.CapabilityHeadless, workerservice.ModelCapability}
	c.Labels = map[string]string{"runtime:ollama": "1"}
	for modelId, attrs := range models {
		c.Labels[workerservice.ModelLabel(modelId)] = attrs.String()
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func modelRouter(t *testing.T, store FleetStore) *Router {
	t.Helper()
	return NewRouter(store, testLogger(), fleetNow)
}

// --- the advertisement contract ---------------------------------------------

// Every capability defaults to FALSE. A model that silently answers prose to a
// conductor turn produces a parse failure three layers away naming nothing, so
// "did not say" must never read as "can".
func TestUnadvertisedCapabilitiesAreNotAssumed(t *testing.T) {
	a := ParseModelAttributes("ctx=8192")
	if a.StructuredOutput || a.Embeddings {
		t.Fatalf("silence must not read as capability: %+v", a)
	}
	if ok, why := a.Satisfies(ModelNeeds{StructuredOutput: true}); ok {
		t.Fatal("a model that never claimed structured output must not satisfy a structured prompt")
	} else if !strings.Contains(why, "structured output") {
		t.Fatalf("the rule-out reason must name the miss, got %q", why)
	}
}

// A garbled value must cost eligibility, never grant it -- and must not take
// the machine out of the fleet entirely for a cosmetic reason.
func TestGarbledAttributesFailClosedWithoutLosingTheMachine(t *testing.T) {
	a := ParseModelAttributes("ctx=banana,structured=maybe,max=-4,,junk")
	if a.ContextWindow != 0 || a.StructuredOutput || a.MaxConcurrent != 0 {
		t.Fatalf("garbled attributes must parse to the zero value, got %+v", a)
	}
	if ok, _ := a.Satisfies(ModelNeeds{}); !ok {
		t.Fatal("a machine offering the model with unreadable attributes still offers the model")
	}
}

func TestModelAttributesRoundTripThroughTheLabelValue(t *testing.T) {
	in := ModelAttributes{ContextWindow: 131072, StructuredOutput: true, Embeddings: true, MaxConcurrent: 3}
	if got := ParseModelAttributes(in.String()); got != in {
		t.Fatalf("round trip = %+v, want %+v -- the cockpit contract and the engine's reading "+
			"of it must have exactly one definition", got, in)
	}
}

// --- selection --------------------------------------------------------------

func TestPlanModelSelectsOnlyMachinesOfferingTheModel(t *testing.T) {
	store := &fakeFleet{machines: []Candidate{
		modelMachine("has-it", map[string]ModelAttributes{smallModel: {ContextWindow: 8192}}),
		modelMachine("has-other", map[string]ModelAttributes{bigModel: {ContextWindow: 32768}}),
	}}
	plan, err := modelRouter(t, store).PlanModel(context.Background(), "alice", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatalf("PlanModel: %v", err)
	}
	if got := ids(plan.Candidates); len(got) != 1 || got[0] != "has-it" {
		t.Fatalf("candidates = %v, want only the machine offering %s", got, smallModel)
	}
	if why := plan.Rejected["has-other"]; !strings.Contains(why, smallModel) {
		t.Fatalf("rejection must name the model, got %q -- an empty candidate set that says "+
			"only 'nothing matched' leaves the operator's actual question unanswered", why)
	}
}

// Capability gating is AVAILABILITY: a structured prompt simply does not see a
// model that cannot do structured output.
func TestStructuredPromptsSkipModelsWithoutStructuredOutput(t *testing.T) {
	store := &fakeFleet{machines: []Candidate{
		modelMachine("prose-only", map[string]ModelAttributes{smallModel: {ContextWindow: 8192}}),
		modelMachine("structured", map[string]ModelAttributes{smallModel: {ContextWindow: 8192, StructuredOutput: true}}),
	}}
	plan, err := modelRouter(t, store).PlanModel(context.Background(), "alice", smallModel, ModelNeeds{StructuredOutput: true})
	if err != nil {
		t.Fatalf("PlanModel: %v", err)
	}
	if got := ids(plan.Candidates); len(got) != 1 || got[0] != "structured" {
		t.Fatalf("candidates = %v, want only the structured-output machine", got)
	}
}

func TestEmbeddingCallsSkipModelsWithoutEmbeddings(t *testing.T) {
	store := &fakeFleet{machines: []Candidate{
		modelMachine("chat", map[string]ModelAttributes{embedModel: {ContextWindow: 2048}}),
		modelMachine("vectors", map[string]ModelAttributes{embedModel: {ContextWindow: 2048, Embeddings: true}}),
	}}
	plan, err := modelRouter(t, store).PlanModel(context.Background(), "alice", embedModel, ModelNeeds{Embeddings: true})
	if err != nil {
		t.Fatalf("PlanModel: %v", err)
	}
	if got := ids(plan.Candidates); len(got) != 1 || got[0] != "vectors" {
		t.Fatalf("candidates = %v, want only the embeddings-capable machine", got)
	}
}

func TestContextFloorRulesOutASmallWindowAndAnUnstatedOne(t *testing.T) {
	store := &fakeFleet{machines: []Candidate{
		modelMachine("small", map[string]ModelAttributes{smallModel: {ContextWindow: 4096}}),
		modelMachine("silent", map[string]ModelAttributes{smallModel: {}}),
		modelMachine("big", map[string]ModelAttributes{smallModel: {ContextWindow: 131072}}),
	}}
	plan, err := modelRouter(t, store).PlanModel(context.Background(), "alice", smallModel, ModelNeeds{MinContextWindow: 32768})
	if err != nil {
		t.Fatalf("PlanModel: %v", err)
	}
	if got := ids(plan.Candidates); len(got) != 1 || got[0] != "big" {
		t.Fatalf("candidates = %v, want only the machine over the floor", got)
	}
	if why := plan.Rejected["silent"]; !strings.Contains(why, "no context window") {
		t.Fatalf("an unstated window must be reported as unstated, not as zero: %q", why)
	}
}

// leastLoaded must ration by the ceiling the machine declared for THIS MODEL.
// A machine advertising max=1 for a 70B and max=8 for a 1B has one load ratio
// per model; ordering by a single number would send eight concurrent 70B calls
// to a laptop that said it could take one.
func TestLeastLoadedUsesThePerModelConcurrencyCap(t *testing.T) {
	// "roomy" is busier in absolute terms but declared a far higher ceiling
	// for this model, so it is the less loaded of the two.
	tight := modelMachine("tight", map[string]ModelAttributes{smallModel: {MaxConcurrent: 1}})
	tight.ActiveCount = 1
	roomy := modelMachine("roomy", map[string]ModelAttributes{smallModel: {MaxConcurrent: 8}})
	roomy.ActiveCount = 2

	store := &fakeFleet{
		machines: []Candidate{tight, roomy},
		policy:   &Policy{Strategy: StrategyLeastLoaded, Fallback: FallbackNextMatching},
	}
	plan, err := modelRouter(t, store).PlanModel(context.Background(), "alice", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatalf("PlanModel: %v", err)
	}
	if got := ids(plan.Candidates); len(got) != 2 || got[0] != "roomy" {
		t.Fatalf("order = %v, want the machine with headroom for THIS model first", got)
	}
}

// The per-model cap must not leak into the machine's other capability slots --
// the router stamps a copy, not the row.
func TestPerModelCapDoesNotMutateTheCandidatesOtherCaps(t *testing.T) {
	m := modelMachine("m", map[string]ModelAttributes{smallModel: {MaxConcurrent: 4}})
	m.Concurrency = map[string]uint32{workerservice.CapabilityHeadless: 2}
	store := &fakeFleet{machines: []Candidate{m}}
	if _, err := modelRouter(t, store).PlanModel(context.Background(), "alice", smallModel, ModelNeeds{}); err != nil {
		t.Fatalf("PlanModel: %v", err)
	}
	if got := store.machines[0].Concurrency[workerservice.ModelCapability]; got != 0 {
		t.Fatalf("the fixture row grew a MODEL cap of %d -- narrowToModel must stamp a copy", got)
	}
	if got := store.machines[0].Concurrency[workerservice.CapabilityHeadless]; got != 2 {
		t.Fatalf("the machine's HEADLESS cap changed to %d", got)
	}
}

func TestOfflineAndRevokedMachinesAreRuledOutWithTheirReason(t *testing.T) {
	stale := modelMachine("stale", map[string]ModelAttributes{smallModel: {}})
	stale.LastSeenAt = fleetNow().Add(-time.Hour)
	revoked := modelMachine("revoked", map[string]ModelAttributes{smallModel: {}})
	revoked.RevokedAt = fleetNow().Add(-time.Minute)
	live := modelMachine("live", map[string]ModelAttributes{smallModel: {}})

	store := &fakeFleet{machines: []Candidate{stale, revoked, live}}
	plan, err := modelRouter(t, store).PlanModel(context.Background(), "alice", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatalf("PlanModel: %v", err)
	}
	if got := ids(plan.Candidates); len(got) != 1 || got[0] != "live" {
		t.Fatalf("candidates = %v", got)
	}
	if plan.Rejected["stale"] != "offline" || plan.Rejected["revoked"] != "revoked" {
		t.Fatalf("rejections = %v, want the two states named separately", plan.Rejected)
	}
}

// --- the ownership boundary -------------------------------------------------

// A model call carries the acting user's PROMPTS. There is no cross-user
// routing path, and the way that is guaranteed is that the read itself is
// caller-scoped: another user's machine is not in the result to be filtered.
func TestAModelCallNeverReachesAnotherUsersMachine(t *testing.T) {
	store := &fakeFleet{
		owner: "alice",
		machines: []Candidate{
			modelMachine("alice-laptop", map[string]ModelAttributes{smallModel: {StructuredOutput: true}}),
		},
	}
	plan, err := modelRouter(t, store).PlanModel(context.Background(), "bob", smallModel, ModelNeeds{})
	if err != nil {
		t.Fatalf("PlanModel: %v", err)
	}
	if len(plan.Candidates) != 0 {
		t.Fatalf("bob was offered %v -- a machine matching perfectly but owned by somebody else "+
			"must never be selected; the prompt is the user's", ids(plan.Candidates))
	}
}

// A blank acting user is a caller that failed to resolve one. Treating it as
// a system call would send somebody's prompt to a machine chosen under a
// different consent model, so it is refused.
func TestABlankActingUserIsRefusedRatherThanWidened(t *testing.T) {
	store := &fakeFleet{machines: []Candidate{modelMachine("m", map[string]ModelAttributes{smallModel: {}})}}
	_, err := modelRouter(t, store).PlanModel(context.Background(), "", smallModel, ModelNeeds{})
	if err == nil {
		t.Fatal("a blank acting user must be refused, not silently treated as system work")
	}
	if !strings.Contains(err.Error(), "acting user") {
		t.Fatalf("the refusal must say what is missing, got %v", err)
	}
}

// --- the shared-inference opt-in --------------------------------------------

// System calls reach only machines whose OWNER opted in. The opt-in is on
// operatorLabels, which is the map a reconnect does not overwrite.
func TestSystemCallsReachOnlyOptedInMachines(t *testing.T) {
	optedIn := modelMachine("shared", map[string]ModelAttributes{smallModel: {}})
	optedIn.SharedInference = true
	optedIn.OwnerUserId = "alice"
	private := modelMachine("private", map[string]ModelAttributes{smallModel: {}})
	private.OwnerUserId = "bob"

	store := &sharedFleet{fakeFleet: &fakeFleet{}, all: []Candidate{optedIn, private}}
	plan, err := modelRouter(t, store).PlanSharedModel(context.Background(), smallModel, ModelNeeds{})
	if err != nil {
		t.Fatalf("PlanSharedModel: %v", err)
	}
	if got := ids(plan.Candidates); len(got) != 1 || got[0] != "shared" {
		t.Fatalf("candidates = %v, want only the opted-in machine", got)
	}
	if why := plan.Rejected["private"]; !strings.Contains(why, "opted") {
		t.Fatalf("rejection must say the owner never opted in, got %q -- an operator wondering "+
			"why their fleet is idle for system work should read the answer", why)
	}
}

// The opt-in must come from operatorLabels, never from the cockpit's `labels`.
// A machine that reports the label itself has NOT been opted in by its owner,
// and the store is what resolves that -- so a Candidate carrying the label in
// its merged map but not the flag stays out.
func TestAMachineCannotOptItselfIn(t *testing.T) {
	selfClaimed := modelMachine("self-claimed", map[string]ModelAttributes{smallModel: {}})
	// The cockpit reported it. The owner did not set it, so the flag the
	// store projects from operatorLabels alone stays false.
	selfClaimed.Labels[SharedInferenceLabel] = "true"
	selfClaimed.SharedInference = false

	store := &sharedFleet{fakeFleet: &fakeFleet{}, all: []Candidate{selfClaimed}}
	plan, err := modelRouter(t, store).PlanSharedModel(context.Background(), smallModel, ModelNeeds{})
	if err != nil {
		t.Fatalf("PlanSharedModel: %v", err)
	}
	if len(plan.Candidates) != 0 {
		t.Fatal("a machine that reported sharedInference in its OWN labels was selected -- the " +
			"opt-in belongs to the owner, and `labels` is rewritten on every reconnect")
	}
}

// A fleet store with no cross-owner read must refuse system work rather than
// silently routing it somewhere narrower.
func TestSystemCallsRefuseWhenTheStoreCannotReadTheCluster(t *testing.T) {
	store := &fakeFleet{machines: []Candidate{modelMachine("m", map[string]ModelAttributes{smallModel: {}})}}
	if _, err := modelRouter(t, store).PlanSharedModel(context.Background(), smallModel, ModelNeeds{}); err == nil {
		t.Fatal("expected a refusal from a store with no shared-inference read")
	}
}

// System work runs under the DEFAULT policy: a routing policy belongs to a
// user and expresses how THEY want their machines used, so applying one user's
// policy to a call that may land on another user's machine would carry a
// preference across the boundary the rest of this file holds.
func TestSystemCallsDoNotInheritAUsersRoutingPolicy(t *testing.T) {
	first := modelMachine("first", map[string]ModelAttributes{smallModel: {}})
	first.SharedInference = true
	second := modelMachine("second", map[string]ModelAttributes{smallModel: {}})
	second.SharedInference = true

	store := &sharedFleet{
		fakeFleet: &fakeFleet{policy: &Policy{Strategy: StrategyLabelMatch, RequireLabels: map[string]string{"nope": "1"}}},
		all:       []Candidate{first, second},
	}
	plan, err := modelRouter(t, store).PlanSharedModel(context.Background(), smallModel, ModelNeeds{})
	if err != nil {
		t.Fatalf("PlanSharedModel: %v", err)
	}
	if plan.Policy.Strategy != StrategyFirstFit {
		t.Fatalf("strategy = %q, want the default -- a user's policy must not steer cluster work",
			plan.Policy.Strategy)
	}
	if len(plan.Candidates) != 2 {
		t.Fatalf("candidates = %v; a user's requireLabels must not narrow cluster work", ids(plan.Candidates))
	}
}

func TestCandidateModelInventoryIsStable(t *testing.T) {
	c := modelMachine("m", map[string]ModelAttributes{bigModel: {}, smallModel: {}, embedModel: {Embeddings: true}})
	got := c.ModelsOffered()
	if len(got) != 3 {
		t.Fatalf("ModelsOffered = %v", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("ModelsOffered must be sorted so a catalog over several machines is stable: %v", got)
		}
	}
	if r := c.Runtimes(); len(r) != 1 || r[0] != "ollama" {
		t.Fatalf("Runtimes = %v", r)
	}
}
