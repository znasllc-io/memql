package router

// Selector expansion (epic memql#5127, task memql#5131, design D4).
//
// A selector turns a question -- "the strongest local model", "the cheapest
// federated one" -- into an ordered list of concrete names. Every failure
// worth guarding here is an ordering that puts an UNKNOWN first: a model that
// did not report its size, a vendor record whose prices nobody filled in.
// Missing must never win by silence, because winning by silence produces a
// decision record that looks exactly like a deliberate one.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// pricedProvider is a vendor record stand-in. It answers chat so the walk can
// reach it; the interesting part is its price params.
type pricedProvider struct{}

func (pricedProvider) Call(context.Context, string) (any, error) { return "", nil }
func (pricedProvider) CallChat(context.Context, []common.ChatMessage) (string, error) {
	return "answer", nil
}

// sizedModel is a catalog entry with an explicit parameter count and window.
func sizedModel(id string, params int64, window int) memql.FleetModel {
	return memql.FleetModel{
		ModelId:          id,
		Params:           params,
		ContextWindow:    window,
		StructuredOutput: true,
		Tools:            true,
		Machines:         []memql.FleetMachine{{RegistrationId: "laptop", Name: "laptop", Online: true}},
	}
}

// sizedEmbedder is an embedding model: it advertises embeddings and nothing a
// conversation needs, so it serves embedding calls only.
func sizedEmbedder(id string, window int) memql.FleetModel {
	return memql.FleetModel{
		ModelId:       id,
		Params:        600_000_000,
		ContextWindow: window,
		Embeddings:    true,
		Machines:      []memql.FleetMachine{{RegistrationId: "laptop", Name: "laptop", Online: true}},
	}
}

// fleetRouter builds a router whose one rule names a one-entry chain.
func fleetRouter(t *testing.T, entry string, models []memql.FleetModel) *Router {
	t.Helper()
	providers := registryOpenToEverybody()
	providers.SetFleetInference(&stubFleetInference{models: models})
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"p": {entry}})
	return New(providers, policies, testRules(t, defaultRule("p")), nil, nil)
}

// STRONGEST: parameters descending, then context window descending, and a
// model that never said how big it is sorts LAST.
//
// The direction is the whole rule. A cockpit that predates the attribute, or a
// runtime that reports nothing, would otherwise become the fleet's strongest
// model on every machine it runs on -- and every planning turn would quietly
// land on a 1B model.
func TestSelectorStrongest_ParametersThenContextWindowMissingLast(t *testing.T) {
	r := fleetRouter(t, memql.FleetStrongest, []memql.FleetModel{
		sizedModel("small", 8_000_000_000, 8192),
		sizedModel("silent", 0, 131072),
		sizedModel("big", 70_000_000_000, 8192),
		sizedModel("bigWideWindow", 70_000_000_000, 131072),
	})
	_, resolved, err := r.ResolveChat(ResolveRequest{UserId: "alice"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ProviderName != "fleet:bigWideWindow" {
		t.Fatalf("resolved %q, want the largest model, window breaking the tie", resolved.ProviderName)
	}
	// The rest of the order shows up on the chain the fallback wrapper walks.
	want := []string{"fleet:bigWideWindow", "fleet:big", "fleet:small", "fleet:silent"}
	if got := resolved.Chain; !equalStrings(got, want) {
		t.Fatalf("chain = %v, want %v -- the model that reported no size sorts LAST, never first", got, want)
	}
}

// FASTEST: fewest parameters, and a model that reported no size sorts LAST
// here too. That is the same rule as strongest rather than its mirror:
// "the machine did not say" is not "zero parameters", and reading it as zero
// would make every silent model the fastest thing on the fleet.
func TestSelectorFastest_FewestParametersWhenNothingIsMeasured(t *testing.T) {
	r := fleetRouter(t, memql.FleetFastest, []memql.FleetModel{
		sizedModel("big", 70_000_000_000, 131072),
		sizedModel("silent", 0, 131072),
		sizedModel("small", 8_000_000_000, 8192),
	})
	_, resolved, err := r.ResolveChat(ResolveRequest{UserId: "alice"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ProviderName != "fleet:small" {
		t.Fatalf("resolved %q, want the smallest model", resolved.ProviderName)
	}
	// Missing size cannot demonstrate the fast quality floor.
	want := []string{"fleet:small", "fleet:big"}
	if got := resolved.Chain; !equalStrings(got, want) {
		t.Fatalf("chain = %v, want %v", got, want)
	}
}

// The two selectors are DIFFERENT QUESTIONS about one catalog, and a fleet
// where they answer the same is a fleet with one model.
func TestSelectorsStrongestAndFastestDisagree(t *testing.T) {
	models := []memql.FleetModel{
		sizedModel("big", 70_000_000_000, 131072),
		sizedModel("small", 8_000_000_000, 131072),
	}
	_, strongest, err := fleetRouter(t, memql.FleetStrongest, models).ResolveChat(ResolveRequest{UserId: "alice"})
	if err != nil {
		t.Fatalf("strongest: %v", err)
	}
	_, fastest, err := fleetRouter(t, memql.FleetFastest, models).ResolveChat(ResolveRequest{UserId: "alice"})
	if err != nil {
		t.Fatalf("fastest: %v", err)
	}
	if strongest.ProviderName == fastest.ProviderName {
		t.Fatalf("both selectors resolved %q; if they cannot disagree on this catalog the second one "+
			"is decoration", strongest.ProviderName)
	}
}

// `fleet:*` IS RETIRED. It refuses by name and the refusal carries the
// replacement, because an author who wrote it meant "the best local model" and
// "invalid entry" sends them to the source to find out what to write instead.
func TestFleetWildcardIsRefusedAndNamesItsReplacement(t *testing.T) {
	r := fleetRouter(t, memql.FleetWildcard, []memql.FleetModel{sizedModel("llama3.1:8b", 8_000_000_000, 131072)})
	_, _, err := r.ResolveChat(ResolveRequest{UserId: "alice"})
	if err == nil {
		t.Fatal("fleet:* resolved; it is retired")
	}
	if !strings.Contains(err.Error(), memql.FleetStrongest) {
		t.Fatalf("the refusal must name the replacement, got %q", err)
	}
}

// CHEAPEST is input plus output per million, ascending.
func TestSelectorCheapest_InputPlusOutputAscending(t *testing.T) {
	r, _ := federationRouter(t, "federation:cheapest", map[string][2]float64{
		"dear":   {15, 75},
		"middle": {3, 15},
		"cheap":  {0.15, 0.6},
	})
	_, resolved, err := r.ResolveChat(ResolveRequest{UserId: "alice"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ProviderName != "cheap" {
		t.Fatalf("resolved %q, want the cheapest by input+output", resolved.ProviderName)
	}
	want := []string{"cheap", "middle", "dear"}
	if got := resolved.Chain; !equalStrings(got, want) {
		t.Fatalf("chain = %v, want %v", got, want)
	}
}

// A RECORD WITH NO COST FIGURES SORTS LAST, AND THE REASON SAYS SO.
//
// Sorting it first is the natural bug and the expensive one: an unpriced
// record reads as zero, zero is the smallest sum in the catalog, and the
// record nobody filled in becomes the cheapest thing the router can find --
// silently free rather than visibly unpriced.
func TestSelectorCheapest_ARecordWithNoCostFiguresSortsLastAndIsReported(t *testing.T) {
	r, _ := federationRouter(t, "federation:cheapest", map[string][2]float64{
		"dear":     {15, 75},
		"unpriced": {0, 0},
	})
	_, resolved, err := r.ResolveChat(ResolveRequest{UserId: "alice"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ProviderName != "dear" {
		t.Fatalf("resolved %q: a record with no prices must not read as the cheapest one", resolved.ProviderName)
	}
	if got := resolved.Chain; !equalStrings(got, []string{"dear", "unpriced"}) {
		t.Fatalf("chain = %v, want the unpriced record last", got)
	}
	// And the decision SAYS why it sorted there, so an operator can tell
	// "expensive" from "nobody wrote the price down".
	var note string
	for _, c := range resolved.Decision.Considered {
		if c.Entry == "unpriced" {
			note = c.Reason
		}
	}
	if !strings.Contains(note, "no inputCostPerMillion") {
		t.Fatalf("the decision must name the missing figures, got %q", note)
	}
}

// A `federation:<providerName>` entry is that one record, and a bare name is
// the same thing spelled without the prefix.
func TestFederationNamedEntryAndBareNameAreTheSame(t *testing.T) {
	for _, entry := range []string{"federation:middle", "middle"} {
		r, _ := federationRouter(t, entry, map[string][2]float64{
			"cheap":  {0.15, 0.6},
			"middle": {3, 15},
		})
		_, resolved, err := r.ResolveChat(ResolveRequest{UserId: "alice"})
		if err != nil {
			t.Fatalf("resolve %q: %v", entry, err)
		}
		if resolved.ProviderName != "middle" {
			t.Fatalf("entry %q resolved %q, want the named record and no selection at all",
				entry, resolved.ProviderName)
		}
	}
}

// An entry under the CONTEXT FLOOR is skipped, and the report says which floor
// it missed. On the fleet side the floor is checked against the machine's own
// report -- which this epic connects for the first time: the call-time chooser
// never had a floor to check.
func TestSelectorsSkipEntriesUnderTheContextFloor(t *testing.T) {
	r := fleetRouter(t, memql.FleetStrongest, []memql.FleetModel{
		sizedModel("bigButNarrow", 70_000_000_000, 8192),
		sizedModel("smallButWide", 8_000_000_000, 131072),
	})
	req := ResolveRequest{UserId: "alice", Needs: airoute.Needs{MinContextTokens: 32768}}
	_, resolved, err := r.ResolveChat(req)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ProviderName != "fleet:smallButWide" {
		t.Fatalf("resolved %q: the strongest model cannot hold the call, so it must be passed over",
			resolved.ProviderName)
	}
	var note string
	for _, c := range resolved.Decision.Considered {
		if c.Entry == "fleet:bigButNarrow" {
			note = c.Reason
		}
	}
	if !strings.Contains(note, "8192") || !strings.Contains(note, "32768") {
		t.Fatalf("the reason must name the window and the floor, got %q", note)
	}
	if resolved.Decision.MinContextTokens != 32768 {
		t.Fatalf("the decision must record the floor it was made against, got %d",
			resolved.Decision.MinContextTokens)
	}
}

// A FEDERATION record that declares no contextWindow is a MISS, not a pass,
// and the reason names the param to add rather than the model to change.
//
// Reading a missing figure as "big enough" is the same silence-wins failure as
// the ordering above, arriving at the one place where being wrong means a
// truncated answer nobody can see.
func TestFederationRecordWithNoContextWindowIsSkippedAndNamesTheParam(t *testing.T) {
	providers := registryOpenToEverybody()
	providers.RegisterForTest("noWindow", "AnthropicStream", "claude-sonnet", pricedProvider{})
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"p": {"noWindow"}})
	r := New(providers, policies, testRules(t, defaultRule("p")), nil, nil)

	_, _, err := r.ResolveChat(ResolveRequest{UserId: "alice", Needs: airoute.Needs{MinContextTokens: 4096}})
	if err == nil {
		t.Fatal("a record that cannot be shown to hold the call must not serve it")
	}
	if !strings.Contains(err.Error(), "contextWindow") {
		t.Fatalf("the refusal must name the param that is missing, got %q", err)
	}

	// The control: with NO floor declared there is nothing to miss, and the
	// same record serves the call. Without this, the assertion above would
	// also pass for a router that refused every record.
	_, resolved, err := r.ResolveChat(ResolveRequest{UserId: "alice"})
	if err != nil {
		t.Fatalf("with no floor the same record must serve the call: %v", err)
	}
	if resolved.ProviderName != "noWindow" {
		t.Fatalf("resolved %q", resolved.ProviderName)
	}
}

// An entry that cannot serve the NEEDS is skipped with the need named. A
// runtime handed tools it cannot honour answers prose, which surfaces three
// layers away as an agent that stopped using its tools for no visible reason.
func TestSelectorsSkipEntriesThatCannotServeTheNeeds(t *testing.T) {
	proseOnly := sizedModel("proseOnly", 70_000_000_000, 131072)
	proseOnly.Tools = false
	r := fleetRouter(t, memql.FleetStrongest, []memql.FleetModel{
		proseOnly,
		sizedModel("toolCapable", 8_000_000_000, 131072),
	})

	_, resolved, err := r.ResolveWithTools(ResolveRequest{UserId: "alice", Needs: airoute.Needs{Tools: true}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ProviderName != "fleet:toolCapable" {
		t.Fatalf("resolved %q, want the tool-capable model", resolved.ProviderName)
	}
	var note string
	for _, c := range resolved.Decision.Considered {
		if c.Entry == "fleet:proseOnly" {
			note = c.Reason
		}
	}
	if !strings.Contains(note, "tool") {
		t.Fatalf("the reason must say the model does not advertise tool calling, got %q", note)
	}
}

// THE THREE NEW MODALITIES each check their own interface and say which one an
// entry does not serve.
func TestNewModalityEntryPointsNameTheModalityAnEntryCannotServe(t *testing.T) {
	providers := registryOpenToEverybody()
	// A chat-only client: it answers CallChat and nothing else.
	providers.RegisterForTest("chatOnly", "OpenAI", "gpt", pricedProvider{})
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"p": {"chatOnly"}})
	r := New(providers, policies, testRules(t, defaultRule("p")), nil, nil)

	for _, tc := range []struct {
		name string
		call func() error
		want string
	}{
		{"structured", func() error {
			_, _, err := r.ResolveStructured(ResolveRequest{UserId: "alice"})
			return err
		}, "structured output"},
		{"vision", func() error {
			_, _, err := r.ResolveVision(ResolveRequest{UserId: "alice"})
			return err
		}, "vision turns"},
		{"embedding", func() error {
			_, _, err := r.ResolveEmbedding(ResolveRequest{UserId: "alice", Modality: airoute.ModalityEmbedding})
			return err
		}, "embeddings"},
	} {
		err := tc.call()
		if err == nil {
			t.Fatalf("%s: a chat-only client served it", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: the report must name the modality, got %q", tc.name, err)
		}
	}

	// The control: the same client DOES serve chat, so the refusals above are
	// about the modality rather than about an unreachable registry.
	if _, _, err := r.ResolveChat(ResolveRequest{UserId: "alice"}); err != nil {
		t.Fatalf("the same client must still serve chat: %v", err)
	}
}

// A `policy:` entry must never reach resolution -- chains are expanded at
// load. If one does, saying so is the difference between a five-minute fix and
// an afternoon in the provider registry looking for a record that was never
// meant to be there.
func TestAPolicyReferenceAtResolutionIsNamedAsALoaderBug(t *testing.T) {
	providers := registryOpenToEverybody()
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"p": {"policy:localFirst"}})
	r := New(providers, policies, testRules(t, defaultRule("p")), nil, nil)

	_, _, err := r.ResolveChat(ResolveRequest{UserId: "alice"})
	if err == nil {
		t.Fatal("an unexpanded policy reference resolved")
	}
	if !strings.Contains(err.Error(), "expanded at load") {
		t.Fatalf("the error must say where the expansion should have happened, got %q", err)
	}
	if strings.Contains(err.Error(), "no provider by that name") {
		t.Fatal("a policy reference must not be reported as an unknown provider name")
	}
}

// federationRouter registers one priced record per name and points the one
// rule at a chain naming `entry`.
func federationRouter(t *testing.T, entry string, prices map[string][2]float64) (*Router, *memql.ProviderRegistry) {
	t.Helper()
	providers := registryOpenToEverybody()
	for name, p := range prices {
		providers.RegisterWithParamsForTest(name, "AnthropicStream", "model-"+name, map[string]any{
			"inputCostPerMillion":  p[0],
			"outputCostPerMillion": p[1],
			"contextWindow":        200000,
		}, pricedProvider{})
	}
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"p": {entry}})
	return New(providers, policies, testRules(t, defaultRule("p")), nil, nil), providers
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// ---- the vendor selectors follow the job (design record 2026-11-21, R2) ----

type jobRecordSpec struct {
	name, providerType, levels string
	rank                       int
	in, out                    float64
}

// jobRouter is a router whose one chain entry is a vendor selector, over
// records that declare a job.
func jobRouter(t *testing.T, entry string, records ...jobRecordSpec) *Router {
	t.Helper()
	providers := memql.NewProviderRegistryForTest()
	for _, rec := range records {
		params := map[string]any{"contextWindow": 200000}
		if rec.levels != "" {
			params["levels"] = rec.levels
		}
		if rec.rank != 0 {
			params["rank"] = rec.rank
		}
		if rec.in != 0 || rec.out != 0 {
			params["inputCostPerMillion"], params["outputCostPerMillion"] = rec.in, rec.out
		}
		providers.RegisterWithParamsForTest(rec.name, rec.providerType, "model-"+rec.name, params, pricedProvider{})
	}
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"p": {entry}})
	return New(providers, policies, testRules(t, defaultRule("p")), nil, nil)
}

var jobCatalog = []jobRecordSpec{
	{"haiku", "Anthropic", "fast", 300, 0.1, 0.5},
	{"sonnet", "Anthropic", "fast strong", 600, 2, 10},
	{"opus", "Anthropic", "strong reasoning", 900, 5, 25},
	{"astra", "OpenAI", "strong reasoning", 900, 8, 40},
	{"legacy", "OpenAI", "", 0, 1, 2},
}

// "The strongest vendor" is the highest-ranked model that serves the level the
// call asks for. Before this it was the first record by name, so with only
// Anthropic set up Haiku answered a reasoning call ahead of Opus.
func TestTheStrongestVendorIsTheHighestRankAtTheCallsLevel(t *testing.T) {
	r := jobRouter(t, "federation:strongest", jobCatalog...)
	for level, want := range map[airoute.Level][]string{
		airoute.LevelReasoning: {"astra", "opus"},
		airoute.LevelStrong:    {"astra", "opus", "sonnet"},
		airoute.LevelFast:      {"sonnet", "haiku"},
	} {
		_, resolved, err := r.ResolveChat(ResolveRequest{Level: level})
		if err != nil {
			t.Fatalf("%s: %v", level, err)
		}
		if resolved.ProviderName != want[0] || !equalStrings(resolved.Chain, want) {
			t.Errorf("%s resolved %q over %v, want %v", level, resolved.ProviderName, resolved.Chain, want)
		}
	}
}

func TestTheCheapestVendorIsThePriceOrderAtTheCallsLevel(t *testing.T) {
	r := jobRouter(t, "federation:cheapest", jobCatalog...)
	for level, want := range map[airoute.Level][]string{
		airoute.LevelReasoning: {"opus", "astra"},
		airoute.LevelStrong:    {"sonnet", "opus", "astra"},
		airoute.LevelFast:      {"haiku", "sonnet"},
	} {
		_, resolved, err := r.ResolveChat(ResolveRequest{Level: level})
		if err != nil {
			t.Fatalf("%s: %v", level, err)
		}
		if !equalStrings(resolved.Chain, want) {
			t.Errorf("%s chain = %v, want %v", level, resolved.Chain, want)
		}
	}
}

// A record that declares NO level is on the decision, once, with why: it is a
// record somebody may have expected a selector to choose. A record that serves
// OTHER levels is not: it was never a candidate for this call, and a line for
// each of them on every decision roughly doubled the row and buried the lines
// that say what happened.
func TestTheDecisionNamesARecordWithNoLevelAndNotTheOnesForOtherLevels(t *testing.T) {
	r := jobRouter(t, "federation:strongest", jobCatalog...)
	_, resolved, err := r.ResolveChat(ResolveRequest{Level: airoute.LevelReasoning})
	if err != nil {
		t.Fatal(err)
	}
	notes := map[string][]string{}
	for _, c := range resolved.Decision.Considered {
		notes[c.Entry] = append(notes[c.Entry], c.Reason)
	}
	if got, want := notes["legacy"], "declares no level, so a vendor selector does not choose it"; len(got) != 1 || got[0] != want {
		t.Errorf("legacy is noted as %q, want exactly %q", got, want)
	}
	for _, name := range []string{"haiku", "sonnet"} {
		if got := notes[name]; len(got) != 0 {
			t.Errorf("%s serves other levels and is noted on a reasoning call as %q", name, got)
		}
	}
	// The whole walk of a served call is short: the record with no level, and
	// the candidates the selector offered.
	if n := len(resolved.Decision.Considered); n > 4 {
		t.Errorf("a served call under one selector carries %d considered lines: %+v", n, resolved.Decision.Considered)
	}
}

// No record declares the level: the selector yields nothing and the refusal
// says which level had no model, so the fix (declare one, or choose another
// source) is readable from the record of the call.
func TestASelectorWithNoRecordAtTheLevelSaysSo(t *testing.T) {
	r := jobRouter(t, "federation:strongest", jobRecordSpec{"haiku", "Anthropic", "fast", 300, 0.1, 0.5})
	_, _, err := r.ResolveChat(ResolveRequest{Level: airoute.LevelReasoning})
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	said := false
	for _, door := range refusal.Doors {
		if door.Name == "federation:strongest" && strings.Contains(door.Reason, "no vendor model declares the reasoning level") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the refusal does not say which level had no model: %+v", refusal.Doors)
	}
}

// A record that declares no level still resolves by its own name.
func TestARecordWithNoLevelStillServesByName(t *testing.T) {
	r := jobRouter(t, "federation:legacy", jobCatalog...)
	_, resolved, err := r.ResolveChat(ResolveRequest{Level: airoute.LevelReasoning})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ProviderName != "legacy" {
		t.Fatalf("resolved %q, want the record named", resolved.ProviderName)
	}
}

// THE JOB IS ABOUT TEXT MODELS. A call for speech or transcription asks a
// selector for a record of another kind entirely, which declares no level and
// may not: for those calls the selector offers every record, as it always did,
// and the walk's own check of what each record can do chooses.
func TestASelectorStillOffersEveryRecordToACallThatIsNotForATextModel(t *testing.T) {
	for _, mod := range []providerModality{modalitySpeech, modalityTranscribe, modalityImage, modalityEmbedding} {
		if selectsByJob(mod) {
			t.Errorf("modality %v is chosen by a text model's level and rank", mod)
		}
	}
	for _, mod := range []providerModality{modalityStreamTools, modalityChat, modalityStreamChat, modalityTools, modalityStructured, modalityVision} {
		if !selectsByJob(mod) {
			t.Errorf("modality %v is served by a vendor's text model and is not chosen by its job", mod)
		}
	}
	r := jobRouter(t, "federation:cheapest", append([]jobRecordSpec{{"speech", "OpenAITTS", "", 0, 0, 0}}, jobCatalog...)...)
	var report doorReporter
	names := []string{}
	for _, c := range r.federationCandidates(FederationSelectorCheapest, ResolveRequest{Level: airoute.LevelFast}, modalitySpeech, &report) {
		names = append(names, c.Name)
	}
	found := false
	for _, n := range names {
		found = found || n == "speech"
	}
	if !found {
		t.Fatalf("a speech call under a selector was offered %v, without the speech record", names)
	}
}

// A request that names no level, or the embeddings level, is not a job a text
// record can declare: the selector keeps the order it always had.
func TestASelectorKeepsItsOldOrderForACallWithNoJobLevel(t *testing.T) {
	r := jobRouter(t, "federation:cheapest", jobCatalog...)
	for _, level := range []airoute.Level{"", airoute.LevelEmbeddings} {
		var report doorReporter
		var names []string
		for _, c := range r.federationCandidates(FederationSelectorCheapest, ResolveRequest{Level: level}, modalityChat, &report) {
			names = append(names, c.Name)
		}
		want := []string{"haiku", "legacy", "sonnet", "opus", "astra"}
		if !equalStrings(names, want) {
			t.Errorf("level %q: %v, want every record by price, %v", level, names, want)
		}
	}
}

// A RULE MAY RAISE THE LEVEL A CALL IS SERVED AT (`@level`), and the vendor
// selectors choose for the level it is served at, not the one the caller
// declared. Before this the walk handed the selectors the declared level: a
// strong call a rule raised to reasoning was served by a record that declares
// only strong, while the decision said it was served at reasoning.
func TestAVendorSelectorChoosesForTheLevelARuleRaisedTheCallTo(t *testing.T) {
	for selector, want := range map[string]string{
		"federation:strongest": "opus",
		// The cheapest record that serves REASONING, not the cheapest that
		// serves strong (sonnet).
		"federation:cheapest": "opus",
	} {
		t.Run(selector, func(t *testing.T) {
			providers := memql.NewProviderRegistryForTest()
			for _, rec := range []jobRecordSpec{
				{"sonnet", "Anthropic", "strong", 600, 2, 10},
				{"opus", "Anthropic", "reasoning", 900, 5, 25},
			} {
				providers.RegisterWithParamsForTest(rec.name, rec.providerType, "model-"+rec.name, map[string]any{
					"contextWindow": 200000, "levels": rec.levels, "rank": rec.rank,
					"inputCostPerMillion": rec.in, "outputCostPerMillion": rec.out,
				}, pricedProvider{})
			}
			policies := memql.NewPolicyRegistryForTest(map[string][]string{"p": {selector}})
			raise := &memql.RuleConfig{
				Name:          "operatorReasoning",
				When:          memql.RuleWhen{Prompt: "agentReply", Present: map[string]bool{"prompt": true}},
				Level:         airoute.LevelReasoning,
				Policy:        "p",
				Precedence:    60,
				OnUnavailable: memql.OnUnavailableDegrade,
				Locked:        true,
			}
			r := New(providers, policies, testRules(t, raise, defaultRule("p")), nil, nil)

			_, resolved, err := r.ResolveChat(ResolveRequest{Level: airoute.LevelStrong, PromptName: "agentReply"})
			if err != nil {
				t.Fatalf("ResolveChat: %v", err)
			}
			d := resolved.Decision
			if d.Rule != "operatorReasoning" || d.Level != airoute.LevelReasoning || d.ServedLevel != airoute.LevelReasoning {
				t.Fatalf("rule=%q level=%q served=%q, want the raising rule at reasoning", d.Rule, d.Level, d.ServedLevel)
			}
			if resolved.ProviderName != want {
				t.Fatalf("served by %q, want %q: the decision says reasoning and the record must serve it", resolved.ProviderName, want)
			}
			// A call the rule does not match is still served at its own level.
			_, plain, err := r.ResolveChat(ResolveRequest{Level: airoute.LevelStrong})
			if err != nil || plain.ProviderName != "sonnet" {
				t.Fatalf("an unmatched strong call got %q, %v; want sonnet", plain.ProviderName, err)
			}
		})
	}
}
