package router

// Degrade or park, and what the decision says afterwards (epic memql#5127,
// task memql#5131, design D9 / D10).
//
// NO DEGRADATION IS SILENT. The failure this file guards is not "the wrong
// model served the call" -- it is "a weaker model served the call and nothing
// downstream can tell". Every assertion below is about what the decision
// RECORDS, because the answer itself looks identical either way.

import (
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
)

// levelRouter builds a router with two chains: one that cannot serve anything
// (it names a provider nobody registered) and one that can.
//
// The rule set is the shape that makes a degrade MEAN something: a rule keyed
// to the higher level names the dead chain, and the floor rule names the live
// one. Without two different policies a second walk would be a second reading
// of the same list.
func levelRouter(t *testing.T, higher airoute.Level, higherRule *memql.RuleConfig) (*Router, *countingCloud) {
	t.Helper()
	cloud := &countingCloud{}
	providers := registryOpenToEverybody()
	providers.RegisterWithParamsForTest("streamClaudeSonnet", "AnthropicStream", "claude-sonnet",
		map[string]any{"contextWindow": 200000}, cloud)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{
		"deadChain": {"nobodyRegisteredThis"},
		"liveChain": {"streamClaudeSonnet"},
	})
	if higherRule == nil {
		higherRule = &memql.RuleConfig{
			Name: "higherLane", When: when("level", string(higher)),
			Policy: "deadChain", Precedence: 100, Locked: true,
			OnUnavailable: memql.OnUnavailableDegrade,
		}
	}
	rules := testRules(t, defaultRule("liveChain"), higherRule)
	return New(providers, policies, rules, nil, nil), cloud
}

// A degrade walks DOWN, re-matches the rule at the new level, and records both
// the level asked for and the level served.
func TestDegrade_WalksDownAndRecordsServedLevel(t *testing.T) {
	r, cloud := levelRouter(t, airoute.LevelStrong, nil)

	_, resolved, err := r.ResolveChat(ResolveRequest{Level: airoute.LevelStrong, UserId: "alice"})
	if err != nil {
		t.Fatalf("the chain at the level below must serve this call: %v", err)
	}
	if resolved.ProviderName != "streamClaudeSonnet" {
		t.Fatalf("resolved %q", resolved.ProviderName)
	}
	d := resolved.Decision
	if d.Level != airoute.LevelStrong {
		t.Fatalf("decision level = %q, want the level the call asked for", d.Level)
	}
	if d.ServedLevel != airoute.LevelFast {
		t.Fatalf("served level = %q, want %q -- the level BELOW the one asked for",
			d.ServedLevel, airoute.LevelFast)
	}
	if !d.Degraded {
		t.Fatal("degraded = false while servedLevel differs from level; a degradation nothing " +
			"records is a degradation nothing downstream can tell happened")
	}
	if d.Rule != memql.DefaultRuleName {
		t.Fatalf("rule = %q, want the rule that matched at the level that SERVED", d.Rule)
	}
	if d.Policy != "liveChain" {
		t.Fatalf("policy = %q, want the chain that actually served", d.Policy)
	}
	if cloud.calls != 0 {
		t.Fatalf("resolution alone must not call anything, got %d", cloud.calls)
	}

	// The control: an undegraded call at the SAME level must not be marked
	// degraded. Without it, a decision that hardcoded degraded=true would pass
	// every assertion above.
	_, plain, err := r.ResolveChat(ResolveRequest{Level: airoute.LevelFast, UserId: "alice"})
	if err != nil {
		t.Fatalf("resolve at the floor: %v", err)
	}
	if plain.Decision.Degraded || plain.Decision.ServedLevel != airoute.LevelFast {
		t.Fatalf("a call served at the level it asked for must not be marked degraded: %+v", plain.Decision)
	}
}

// THE DOOR REPORT IS KEPT ON SUCCESS (design D10). A rule is falsifiable only
// if the decisions it made can be read, and what a chain did NOT pick is half
// of that -- the pre-rules walk accumulated exactly this and dropped it with
// the stack frame the moment an entry won.
func TestConsideredIsKeptOnSuccess(t *testing.T) {
	r, _ := levelRouter(t, airoute.LevelStrong, nil)
	_, resolved, err := r.ResolveChat(ResolveRequest{Level: airoute.LevelStrong, UserId: "alice"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	considered := resolved.Decision.Considered
	if len(considered) < 2 {
		t.Fatalf("considered = %+v, want the entry that failed AND the one that won", considered)
	}
	var sawRejected, sawSelected bool
	for _, c := range considered {
		if c.Entry == "nobodyRegisteredThis" && strings.Contains(c.Reason, "no provider by that name") {
			sawRejected = true
		}
		if c.Entry == "streamClaudeSonnet" && c.Reason == "selected" {
			sawSelected = true
		}
	}
	if !sawRejected {
		t.Errorf("the entry that failed at the higher level is missing from the decision: %+v", considered)
	}
	if !sawSelected {
		t.Errorf("the winning entry is missing from the decision: %+v", considered)
	}
}

// `fast` IS THE FLOOR. It does not degrade further, and the report says so
// rather than leaving an exhausted chain unexplained.
func TestDegrade_FastDoesNotDegradeFurther(t *testing.T) {
	providers := registryOpenToEverybody()
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"deadChain": {"nobodyRegisteredThis"}})
	r := New(providers, policies, testRules(t, defaultRule("deadChain")), nil, nil)

	_, _, err := r.ResolveChat(ResolveRequest{Level: airoute.LevelFast, UserId: "alice"})
	if err == nil {
		t.Fatal("an exhausted chain at the floor must refuse")
	}
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want the typed refusal", err)
	}
	if !consideredMentions(refusal.Considered, "no level below fast") {
		t.Fatalf("the decision must say why it stopped, got %+v", refusal.Considered)
	}
	if refusal.Decision.ServedLevel != airoute.LevelFast {
		t.Fatalf("served level = %q on a refusal; a park is as legible as a hit",
			refusal.Decision.ServedLevel)
	}
}

// EMBEDDINGS NEVER DEGRADES. A degraded embedder answers in a different vector
// space, so the result is not a worse vector -- it is one that does not belong
// in the index it was about to be written to, and nothing downstream can tell.
func TestDegrade_EmbeddingsNeverDegrades(t *testing.T) {
	cloud := &countingCloud{}
	providers := registryOpenToEverybody()
	providers.RegisterWithParamsForTest("streamClaudeSonnet", "AnthropicStream", "claude-sonnet",
		map[string]any{"contextWindow": 200000}, cloud)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{
		"deadChain": {"nobodyRegisteredThis"},
		"liveChain": {"streamClaudeSonnet"},
	})
	rules := testRules(t,
		defaultRule("liveChain"),
		&memql.RuleConfig{
			Name: "embeddingsLane", When: when("level", string(airoute.LevelEmbeddings)),
			Policy: "deadChain", Precedence: 110, Locked: true,
			OnUnavailable: memql.OnUnavailableDegrade,
		},
	)
	r := New(providers, policies, rules, nil, nil)

	_, _, err := r.ResolveChat(ResolveRequest{Level: airoute.LevelEmbeddings, UserId: "alice"})
	if err == nil {
		t.Fatal("an embeddings call whose chain is exhausted must refuse; the live chain below it " +
			"would answer in a different vector space")
	}
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want the typed refusal", err)
	}
	if !consideredMentions(refusal.Considered, "different vector space") {
		t.Fatalf("the decision must say WHY embeddings does not degrade, got %+v", refusal.Considered)
	}
	// The control: the very same live chain serves a `strong` call, so the
	// refusal above is about the level rather than about an empty registry.
	if _, _, err := r.ResolveChat(ResolveRequest{Level: airoute.LevelStrong, UserId: "alice"}); err != nil {
		t.Fatalf("the live chain must serve a strong call: %v", err)
	}
}

// PARK RETURNS THE REFUSAL WITH THE REPORT, and it does not degrade -- even
// when the level below would have served the call.
func TestPark_ReturnsTheRefusalWithTheDoorReport(t *testing.T) {
	parks := &memql.RuleConfig{
		Name: "reasoningParks", When: when("level", string(airoute.LevelReasoning)),
		Policy: "deadChain", Precedence: 100, Locked: true,
		OnUnavailable: memql.OnUnavailablePark,
	}
	r, cloud := levelRouter(t, airoute.LevelReasoning, parks)

	_, _, err := r.ResolveChat(ResolveRequest{Level: airoute.LevelReasoning, UserId: "alice"})
	if err == nil {
		t.Fatal("a parking rule must refuse rather than degrade into the chain below it")
	}
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want the typed refusal", err)
	}
	if refusal.Code != work.RefusalEveryDoorShut {
		t.Fatalf("code = %q", refusal.Code)
	}
	if len(refusal.Doors) == 0 {
		t.Fatal("the refusal must carry the door report; 'no provider available' has no action in it")
	}
	if !consideredMentions(refusal.Considered, "onUnavailable=park") {
		t.Fatalf("the decision must say the rule parked, got %+v", refusal.Considered)
	}
	if refusal.Decision.Rule != "reasoningParks" {
		t.Fatalf("the refusal must name the rule that decided, got %q", refusal.Decision.Rule)
	}
	if cloud.calls != 0 {
		t.Fatalf("nothing may be spent while parking, got %d", cloud.calls)
	}

	// THE CONTROL that makes the park meaningful: the level below WOULD have
	// served this call. Without it, the refusal above proves only that the
	// dead chain was dead.
	if _, _, err := r.ResolveChat(ResolveRequest{Level: airoute.LevelFast, UserId: "alice"}); err != nil {
		t.Fatalf("the chain a degrade would have reached must be able to serve: %v", err)
	}
}

// AN EXPLICIT PIN STILL WINS, over every rule and without a level override.
// A rule that could beat a pin would make the pin advisory, which is not what
// a person pinning a provider for one turn is asking for.
func TestExplicitProviderStillWins(t *testing.T) {
	cloud := &countingCloud{}
	pinned := &countingCloud{}
	providers := registryOpenToEverybody()
	providers.RegisterWithParamsForTest("ruleWouldPick", "AnthropicStream", "claude-sonnet",
		map[string]any{"contextWindow": 200000}, cloud)
	providers.RegisterWithParamsForTest("pinnedByTheCaller", "OpenAI", "gpt",
		map[string]any{"contextWindow": 200000}, pinned)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"p": {"ruleWouldPick"}})
	rules := testRules(t,
		defaultRule("p"),
		&memql.RuleConfig{
			Name: "raisesTheLevel", When: when("level", string(airoute.LevelFast)),
			Policy: "p", Level: airoute.LevelReasoning, Precedence: 100, Locked: true,
		},
	)
	r := New(providers, policies, rules, nil, nil)

	_, resolved, err := r.ResolveChat(ResolveRequest{
		Level:            airoute.LevelFast,
		UserId:           "alice",
		ExplicitProvider: "pinnedByTheCaller",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ProviderName != "pinnedByTheCaller" {
		t.Fatalf("resolved %q, want the pin", resolved.ProviderName)
	}
	if resolved.Decision.Rule != "" {
		t.Fatalf("rule = %q: a pin consults no rule", resolved.Decision.Rule)
	}
	if resolved.Decision.Level != airoute.LevelFast || resolved.Decision.ServedLevel != airoute.LevelFast {
		t.Fatalf("a pin overrides no level: %+v", resolved.Decision)
	}

	// The control: without the pin the rule decides, and it raises the level.
	_, byRule, err := r.ResolveChat(ResolveRequest{Level: airoute.LevelFast, UserId: "alice"})
	if err != nil {
		t.Fatalf("resolve by rule: %v", err)
	}
	if byRule.ProviderName != "ruleWouldPick" || byRule.Decision.Rule != "raisesTheLevel" {
		t.Fatalf("without a pin the rule must decide, got %+v", byRule.Decision)
	}
	if byRule.Decision.Level != airoute.LevelReasoning {
		t.Fatalf("the rule's @level must be recorded as the level asked for, got %q", byRule.Decision.Level)
	}
	if byRule.Decision.RequestedLevel != airoute.LevelFast {
		t.Fatalf("the level the CALL declared must survive beside it, got %q", byRule.Decision.RequestedLevel)
	}
	if byRule.Decision.Degraded {
		t.Fatal("a rule raising the level is the routing decision, not a degradation")
	}
}

// @exclude REMOVES A CONCRETE MODEL from resolution -- the demotion vehicle of
// epic 4. It has to bite on the EXPANDED name, because no chain entry is ever
// spelled `fleet:qwen3.5:7b`: an author writes `fleet:strongest`.
func TestExcludeRemovesAConcreteModelFromResolution(t *testing.T) {
	models := []memql.FleetModel{
		sizedModel("big", 70_000_000_000, 131072),
		sizedModel("small", 8_000_000_000, 131072),
	}
	providers := registryOpenToEverybody()
	providers.SetFleetInference(&stubFleetInference{models: models})
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"p": {memql.FleetStrongest}})

	// Without the exclude the strongest model wins.
	plain := New(providers, policies, testRules(t, defaultRule("p")), nil, nil)
	_, before, err := plain.ResolveChat(ResolveRequest{UserId: "alice"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if before.ProviderName != "fleet:big" {
		t.Fatalf("resolved %q before the exclude", before.ProviderName)
	}

	// With it, the same chain resolves to the next one down.
	demoted := New(providers, policies, testRules(t, &memql.RuleConfig{
		Name: memql.DefaultRuleName, When: memql.RuleWhen{Present: map[string]bool{}},
		Policy: "p", OnUnavailable: memql.OnUnavailableDegrade, Locked: true,
		Excludes: []string{"fleet:big"},
	}), nil, nil)
	_, after, err := demoted.ResolveChat(ResolveRequest{UserId: "alice"})
	if err != nil {
		t.Fatalf("resolve with the exclude: %v", err)
	}
	if after.ProviderName != "fleet:small" {
		t.Fatalf("resolved %q, want the excluded model passed over", after.ProviderName)
	}
	if !consideredMentions(after.Decision.Considered, "excluded by") {
		t.Fatalf("the decision must say the model was excluded rather than leaving it absent: %+v",
			after.Decision.Considered)
	}
	for _, name := range after.Chain {
		if name == "fleet:big" {
			t.Fatal("an excluded model must not survive on the fallback chain either")
		}
	}
}

// A CHAIN THAT EMPTIES OUT after excludes is walked as empty, and the rule's
// onUnavailable decides. Falling back to the unexcluded chain would quietly
// undo the exclusion an operator wrote.
func TestEmptyChainAfterExclude_OnUnavailableDecides(t *testing.T) {
	cloud := &countingCloud{}
	providers := registryOpenToEverybody()
	providers.RegisterWithParamsForTest("streamClaudeSonnet", "AnthropicStream", "claude-sonnet",
		map[string]any{"contextWindow": 200000}, cloud)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{
		"onlyOne":   {"streamClaudeSonnet"},
		"liveChain": {"streamClaudeSonnet"},
	})

	// PARK: the excluded chain is empty and the rule refuses.
	parking := testRules(t, defaultRule("liveChain"), &memql.RuleConfig{
		Name: "excludesEverything", When: when("level", string(airoute.LevelStrong)),
		Policy: "onlyOne", Precedence: 100, Locked: true,
		OnUnavailable: memql.OnUnavailablePark,
		Excludes:      []string{"streamClaudeSonnet"},
	})
	r := New(providers, policies, parking, nil, nil)
	if _, _, err := r.ResolveChat(ResolveRequest{Level: airoute.LevelStrong, UserId: "alice"}); err == nil {
		t.Fatal("a chain emptied by excludes with onUnavailable=park must refuse, not fall back to " +
			"the entries the operator excluded")
	}

	// DEGRADE: the same empty chain falls to the level below, which serves.
	degrading := testRules(t, defaultRule("liveChain"), &memql.RuleConfig{
		Name: "excludesEverything", When: when("level", string(airoute.LevelStrong)),
		Policy: "onlyOne", Precedence: 100, Locked: true,
		OnUnavailable: memql.OnUnavailableDegrade,
		Excludes:      []string{"streamClaudeSonnet"},
	})
	r2 := New(providers, policies, degrading, nil, nil)
	_, resolved, err := r2.ResolveChat(ResolveRequest{Level: airoute.LevelStrong, UserId: "alice"})
	if err != nil {
		t.Fatalf("the same empty chain with onUnavailable=degrade must reach the level below: %v", err)
	}
	if !resolved.Decision.Degraded {
		t.Fatalf("the decision must record the degrade: %+v", resolved.Decision)
	}
}

func consideredMentions(entries []airoute.ConsideredEntry, substr string) bool {
	for _, c := range entries {
		if strings.Contains(c.Reason, substr) {
			return true
		}
	}
	return false
}
