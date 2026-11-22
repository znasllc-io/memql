package router

// Rule matching (epic memql#5127, task memql#5131, design D5).
//
// The interesting failures here are all the same shape: a rule that matches
// MORE than its author wrote. An absent condition read as an empty one, a
// role read as the actor's role, a prefix read as an equality -- each of them
// turns a rule written to narrow one case into one that decides every call,
// and every one of them looks correct in the file.

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
)

// when builds a condition set from key/value pairs, recording exactly the keys
// the caller wrote -- which is the distinction the matcher rests on.
func when(pairs ...string) memql.RuleWhen {
	if len(pairs)%2 != 0 {
		panic("when takes key/value pairs")
	}
	w := memql.RuleWhen{Present: map[string]bool{}}
	for i := 0; i < len(pairs); i += 2 {
		k, v := pairs[i], pairs[i+1]
		w.Present[k] = true
		switch k {
		case "level":
			w.Level = v
		case "modality":
			w.Modality = v
		case "prompt":
			w.Prompt = v
		case "role":
			w.Role = v
		case "actorRole":
			w.ActorRole = v
		case "tag":
			w.Tag = v
		case "touches":
			w.Touches = v
		default:
			panic("unknown @when key " + k)
		}
	}
	return w
}

// testRules builds a finalized registry. Every caller must supply a rule named
// `default`, exactly as the loader requires: it is the floor, and a registry
// without it refuses to finalize rather than leaving some calls unrouted.
func testRules(t *testing.T, rules ...*memql.RuleConfig) *memql.RuleRegistry {
	t.Helper()
	reg := memql.NewRuleRegistry()
	for _, r := range rules {
		if r.SourceFile == "" {
			r.SourceFile = "dsl/rules/rules.memql"
		}
		if err := reg.Register(r); err != nil {
			t.Fatalf("register %q: %v", r.Name, err)
		}
	}
	if err := reg.Finalize(); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	return reg
}

// defaultRule is the floor: no conditions, degrade, locked, precedence 0 --
// the shipped rule exactly as dsl/rules/rules.memql declares it.
//
// Every OTHER rule in these tests is locked too, and that is not decoration:
// the registry evaluates every locked rule before every unlocked one, and
// `default` matches everything, so an unlocked rule beneath it is never
// reached. A test rule left unlocked would pass while asserting nothing.
func defaultRule(policy string) *memql.RuleConfig {
	return &memql.RuleConfig{
		Name:          memql.DefaultRuleName,
		When:          memql.RuleWhen{Present: map[string]bool{}},
		Policy:        policy,
		OnUnavailable: memql.OnUnavailableDegrade,
		Locked:        true,
	}
}

// LOCKED BEATS PRECEDENCE, and that is what locked MEANS operationally: an
// owner may add rules and give them any precedence they like, and the shipped
// ones still evaluate first.
func TestMatchRule_LockedFirstThenPrecedence(t *testing.T) {
	r := New(nil, nil, testRules(t,
		defaultRule("localFirst"),
		&memql.RuleConfig{
			Name: "shippedLow", When: when("level", "strong"),
			Policy: "localOnly", Precedence: 1, Locked: true,
		},
		&memql.RuleConfig{
			Name: "ownerHigh", When: when("level", "strong"),
			Policy: "federationStrongest", Precedence: 900,
		},
	), nil, nil)

	got := r.MatchRule(ResolveRequest{Level: airoute.LevelStrong})
	if got == nil || got.Name != "shippedLow" {
		t.Fatalf("matched %v, want the LOCKED rule at precedence 1 -- an owner rule at 900 "+
			"must not outrank a shipped one", ruleName(got))
	}

	// Within one half, precedence orders.
	r2 := New(nil, nil, testRules(t,
		defaultRule("localFirst"),
		&memql.RuleConfig{Name: "lower", When: when("level", "strong"), Policy: "a", Precedence: 10, Locked: true},
		&memql.RuleConfig{Name: "higher", When: when("level", "strong"), Policy: "b", Precedence: 20, Locked: true},
	), nil, nil)
	if got := r2.MatchRule(ResolveRequest{Level: airoute.LevelStrong}); got == nil || got.Name != "higher" {
		t.Fatalf("matched %v, want the higher precedence of two rules in the same half", ruleName(got))
	}
}

// Every PRESENT key must hold. A rule that matched on any one of its
// conditions would fire on calls its author never described.
func TestMatchRule_AllPresentWhenKeysAreANDed(t *testing.T) {
	rule := &memql.RuleConfig{
		Name:   "operatorReasoning",
		When:   when("prompt", "agentReply", "role", "operator"),
		Policy: "federationStrongest", Precedence: 60, Locked: true,
	}
	r := New(nil, nil, testRules(t, defaultRule("localFirst"), rule), nil, nil)

	both := ResolveRequest{PromptName: "agentReply", Role: "operator"}
	if got := r.MatchRule(both); got == nil || got.Name != "operatorReasoning" {
		t.Fatalf("both conditions held and %v matched", ruleName(got))
	}
	for _, half := range []ResolveRequest{
		{PromptName: "agentReply", Role: "writer"},
		{PromptName: "classifySymptom", Role: "operator"},
	} {
		if got := r.MatchRule(half); got == nil || got.Name != memql.DefaultRuleName {
			t.Fatalf("one condition held and %v matched; all present keys are ANDed", ruleName(got))
		}
	}
}

// AN ABSENT KEY AND A KEY WRITTEN EMPTY ARE DIFFERENT CONDITIONS. Collapsing
// them makes every rule with an empty value match everything.
func TestMatchRule_AnAbsentKeyIsNotAnEmptyOne(t *testing.T) {
	empty := &memql.RuleConfig{
		Name: "noPromptCalls", When: when("prompt", ""),
		Policy: "localOnly", Precedence: 50, Locked: true,
	}
	r := New(nil, nil, testRules(t, defaultRule("localFirst"), empty), nil, nil)

	if got := r.MatchRule(ResolveRequest{PromptName: ""}); got == nil || got.Name != "noPromptCalls" {
		t.Fatalf("a call with no prompt name must match @when(prompt=\"\"), got %v", ruleName(got))
	}
	if got := r.MatchRule(ResolveRequest{PromptName: "agentReply"}); got == nil || got.Name != memql.DefaultRuleName {
		t.Fatalf("a call WITH a prompt name must not match @when(prompt=\"\"), got %v", ruleName(got))
	}
}

// A footprint is a set of concept ids and concept ids nest, so the match is a
// PREFIX. Equality would make a rule about `v1:identity:` go stale the next
// time an identity concept was added.
func TestMatchRule_TouchesUsesStartsWith(t *testing.T) {
	rule := &memql.RuleConfig{
		Name: "identityWork", When: when("touches", "v1:identity:"),
		Policy: "localOnly", Precedence: 70, Locked: true,
	}
	r := New(nil, nil, testRules(t, defaultRule("localFirst"), rule), nil, nil)

	hit := ResolveRequest{Touches: []string{"v1:work:step", "v1:identity:user"}}
	if got := r.MatchRule(hit); got == nil || got.Name != "identityWork" {
		t.Fatalf("a footprint containing v1:identity:user must match the prefix, got %v", ruleName(got))
	}
	miss := ResolveRequest{Touches: []string{"v1:work:step", "v1:library:file"}}
	if got := r.MatchRule(miss); got == nil || got.Name != memql.DefaultRuleName {
		t.Fatalf("a footprint with no identity concept must not match, got %v", ruleName(got))
	}
	if got := r.MatchRule(ResolveRequest{}); got == nil || got.Name != memql.DefaultRuleName {
		t.Fatalf("a call with no footprint at all must not match a touches rule, got %v", ruleName(got))
	}
}

// THE FLOOR. `default` states no conditions, so a call that matches nothing
// else still has a policy -- which is why nothing downstream carries a
// no-rule-matched branch.
func TestMatchRule_DefaultIsTheFloorAndAlwaysMatches(t *testing.T) {
	r := New(nil, nil, testRules(t,
		defaultRule("localFirst"),
		&memql.RuleConfig{Name: "narrow", When: when("level", "reasoning"), Policy: "x", Precedence: 100, Locked: true},
	), nil, nil)

	for _, req := range []ResolveRequest{
		{},
		{Level: airoute.LevelFast, PromptName: "docSummary"},
		{Level: airoute.LevelEmbeddings, Modality: airoute.ModalityEmbedding, Tags: []string{"nobodyDeclaredThis"}},
	} {
		got := r.MatchRule(req)
		if got == nil {
			t.Fatalf("no rule matched %+v; the default rule matches every call", req)
		}
		if got.Name != memql.DefaultRuleName && req.Level != airoute.LevelReasoning {
			t.Fatalf("matched %q for %+v", got.Name, req)
		}
	}
}

// ROLE AND ACTORROLE ARE DIFFERENT QUESTIONS. `role` is what is ACTING;
// `actorRole` is who is WATCHING. A rule on role="operator" must not fire
// because a cluster operator happens to be reading a colleague's ordinary
// agent -- routing on the audience rather than on the actor is the bug the
// split exists to prevent, and it is invisible: the turn resolves, it just
// resolves to the wrong chain and bills the wrong way.
func TestMatchRule_RoleAndActorRoleAreDifferentQuestions(t *testing.T) {
	onRole := &memql.RuleConfig{
		Name: "operatorAgents", When: when("role", "operator"),
		Policy: "federationStrongest", Precedence: 60, Locked: true,
	}
	r := New(nil, nil, testRules(t, defaultRule("localFirst"), onRole), nil, nil)

	// The positive control first: without it, a matcher that never matched
	// anything would pass every assertion below.
	if got := r.MatchRule(ResolveRequest{Role: "operator"}); got == nil || got.Name != "operatorAgents" {
		t.Fatalf("an operator AGENT must match the rule on role, got %v", ruleName(got))
	}

	// A human operator driving a NON-operator agent. The actor is an
	// operator; the thing acting is not.
	watching := ResolveRequest{Role: "writer", ActorRole: "operator"}
	if got := r.MatchRule(watching); got == nil || got.Name != memql.DefaultRuleName {
		t.Fatalf("matched %v: a rule on the AGENT's role must not fire because the WATCHING human "+
			"is an operator", ruleName(got))
	}

	// And the mirror: a rule on actorRole must not fire for an operator AGENT
	// driven by an ordinary user.
	onActor := &memql.RuleConfig{
		Name: "operatorHumans", When: when("actorRole", "operator"),
		Policy: "federationStrongest", Precedence: 60, Locked: true,
	}
	r2 := New(nil, nil, testRules(t, defaultRule("localFirst"), onActor), nil, nil)
	if got := r2.MatchRule(ResolveRequest{ActorRole: "operator"}); got == nil || got.Name != "operatorHumans" {
		t.Fatalf("an operator ACTOR must match the rule on actorRole, got %v", ruleName(got))
	}
	acting := ResolveRequest{Role: "operator", ActorRole: "writer"}
	if got := r2.MatchRule(acting); got == nil || got.Name != memql.DefaultRuleName {
		t.Fatalf("matched %v: a rule on the ACTOR's role must not fire for an operator agent", ruleName(got))
	}
}

// A tag rule matches on MEMBERSHIP, so a call carrying several tags matches
// each rule that names one of them, in precedence order.
func TestMatchRule_TagIsMembership(t *testing.T) {
	r := New(nil, nil, testRules(t,
		defaultRule("localFirst"),
		&memql.RuleConfig{Name: "backgroundLane", When: when("tag", airoute.TagBackground), Policy: "localFirst", Precedence: 40, Locked: true},
		&memql.RuleConfig{Name: "backgroundEscalation", When: when("tag", airoute.TagBackgroundEscalation), Policy: "localFirst", Precedence: 50, Locked: true},
	), nil, nil)

	if got := r.MatchRule(ResolveRequest{Tags: []string{airoute.TagBackground}}); got == nil || got.Name != "backgroundLane" {
		t.Fatalf("matched %v", ruleName(got))
	}
	if got := r.MatchRule(ResolveRequest{Tags: []string{airoute.TagBackgroundEscalation}}); got == nil || got.Name != "backgroundEscalation" {
		t.Fatalf("matched %v", ruleName(got))
	}
	// Both tags: the higher precedence wins, which is what makes escalation
	// an escalation rather than a coin flip.
	both := ResolveRequest{Tags: []string{airoute.TagBackground, airoute.TagBackgroundEscalation}}
	if got := r.MatchRule(both); got == nil || got.Name != "backgroundEscalation" {
		t.Fatalf("matched %v for a call carrying both tags", ruleName(got))
	}
}

// A disabled rule is loaded and NOT evaluated -- the ordinary @disabled
// lifecycle, reversible and still maintained.
func TestMatchRule_DisabledRulesAreNotEvaluated(t *testing.T) {
	r := New(nil, nil, testRules(t,
		defaultRule("localFirst"),
		&memql.RuleConfig{
			Name: "off", When: when("level", "strong"),
			Policy: "localOnly", Precedence: 100, Locked: true, Disabled: true,
		},
	), nil, nil)
	if got := r.MatchRule(ResolveRequest{Level: airoute.LevelStrong}); got == nil || got.Name != memql.DefaultRuleName {
		t.Fatalf("matched %v; a disabled rule must not be evaluated", ruleName(got))
	}
}

// @exclude removes an entry from the chain BEFORE anything is tried, matching
// on the entry as an author wrote it.
func TestApplyExcludes_RemovesTheEntryAsWritten(t *testing.T) {
	chain := []string{"fleet:strongest", "app:*", "federation:cheapest"}
	kept, removed := applyExcludes(chain, []string{"app:*"})
	if len(kept) != 2 || kept[0] != "fleet:strongest" || kept[1] != "federation:cheapest" {
		t.Fatalf("kept = %v", kept)
	}
	if len(removed) != 1 || removed[0] != "app:*" {
		t.Fatalf("removed = %v, want the excluded entry named so the decision can say so", removed)
	}
	// Nothing excluded leaves the chain untouched, including its identity.
	same, none := applyExcludes(chain, nil)
	if len(same) != 3 || none != nil {
		t.Fatalf("an empty exclude list must change nothing: %v %v", same, none)
	}
}

// A NIL RULE REGISTRY IS A REFUSAL, NOT A FALLBACK. Without rules there is no
// default rule and therefore no chain; the only alternative to refusing is the
// router picking one on its own, which is exactly the pre-rules precedence
// this epic deleted, arriving back with nothing to show it.
func TestNilRuleRegistryIsARefusalNotAFallback(t *testing.T) {
	providers := registryOpenToEverybody()
	providers.RegisterForTest("streamClaudeSonnet", "AnthropicStream", "claude-sonnet", &countingCloud{})
	// There was a SetDefaultForTest call here, pinning that provider as the
	// registry default so the refusal below could not be attributed to the
	// absence of one. Epic memql#5137 deletes the registry default entirely, so
	// the line is now impossible AND unnecessary: RegisterForTest already makes
	// the provider available, which is the whole premise -- an available
	// provider is not a licence to route without a rule.
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"localFirst": {"streamClaudeSonnet"}})

	r := New(providers, policies, nil, nil, nil)
	_, _, err := r.ResolveChat(ResolveRequest{Level: airoute.LevelStrong, UserId: "alice"})
	if err == nil {
		t.Fatal("a router with no rule registry resolved a call; it must refuse -- an available " +
			"provider is not a licence to route without a rule")
	}
	if !strings.Contains(err.Error(), "no rule registry") {
		t.Fatalf("the refusal must say what is missing, got %q", err)
	}
	if got := r.MatchRule(ResolveRequest{}); got != nil {
		t.Fatalf("MatchRule returned %q with no registry", got.Name)
	}
}

func ruleName(r *memql.RuleConfig) string {
	if r == nil {
		return "<nil>"
	}
	return r.Name
}
