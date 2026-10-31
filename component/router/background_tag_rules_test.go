package router

// The background tag under the SHIPPED rules (games pack design record, seam 1).
//
// docs/public/language/functions.md, docs/public/language/memql.md,
// docs/public/ai/llm-cost-control.md and the `lane` description in
// dsl/agents/builtins.memql all tell an author what a background-lane call's
// tag does, and those sentences are only true while the rules below hold. They
// are asserted here against the rules as shipped, beside two an owner might
// add, so a change to the rule set that makes a sentence false fails next to
// the rules and not in a page nobody re-reads.

import (
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
)

func TestTheShippedBackgroundTagSelectsTheDefaultChainAndPassesOverOwnerRules(t *testing.T) {
	rules := memql.NewRuleRegistry()
	if _, err := memql.LoadUnifiedRules(nil, rules); err != nil {
		t.Fatal(err)
	}
	// Two rules an owner might add: one for a prompt, and one keyed on the tag.
	for _, owner := range []*memql.RuleConfig{
		{Name: "ownerReflect", When: when("prompt", "reflect"), Policy: "federationStrongest", Precedence: 900, OnUnavailable: memql.OnUnavailableDegrade, SourceFile: "owner.memql"},
		{Name: "ownerBackground", When: when("tag", airoute.TagBackground), Policy: "federationStrongest", Precedence: 901, OnUnavailable: memql.OnUnavailableDegrade, SourceFile: "owner.memql"},
	} {
		if err := rules.Register(owner); err != nil {
			t.Fatal(err)
		}
	}
	if err := rules.Finalize(); err != nil {
		t.Fatal(err)
	}
	r := New(nil, nil, rules, nil, nil)
	tagged := func(req ResolveRequest) ResolveRequest {
		req.Tags = []string{airoute.TagBackground}
		return req
	}

	// THE SAME CHAIN AS `default`: the tag changes which rule the decision
	// record names, and not the policy or what happens when it is exhausted.
	plain := r.MatchRule(ResolveRequest{Level: airoute.LevelStrong, PromptName: "other"})
	background := r.MatchRule(tagged(ResolveRequest{Level: airoute.LevelStrong, PromptName: "other"}))
	if plain == nil || plain.Name != memql.DefaultRuleName {
		t.Fatalf("an untagged strong prompt matched %s, want the default rule", ruleName(plain))
	}
	if background == nil || background.Name != "backgroundLane" {
		t.Fatalf("a tagged strong prompt matched %s, want backgroundLane", ruleName(background))
	}
	if background.Policy != plain.Policy || background.OnUnavailable != plain.OnUnavailable {
		t.Fatalf("backgroundLane selects policy %q (%s), default selects %q (%s): the docs say the tag selects the same chain",
			background.Policy, background.OnUnavailable, plain.Policy, plain.OnUnavailable)
	}

	// A FAST PROMPT KEEPS fastLane, AND A REASONING PROMPT KEEPS reasoningParks.
	for level, want := range map[airoute.Level]string{airoute.LevelFast: "fastLane", airoute.LevelReasoning: "reasoningParks"} {
		if got := r.MatchRule(tagged(ResolveRequest{Level: level, PromptName: "other"})); got == nil || got.Name != want {
			t.Errorf("a tagged %s prompt matched %s, want %s", level, ruleName(got), want)
		}
	}

	// LOCKED RULES EVALUATE BEFORE ANY RULE AN OWNER ADDS, so a tagged call is
	// matched by backgroundLane and an owner's rule for the prompt is not
	// consulted. The control first: the same prompt without the tag does reach it.
	if got := r.MatchRule(ResolveRequest{Level: airoute.LevelStrong, PromptName: "reflect"}); got == nil || got.Name != "ownerReflect" {
		t.Fatalf("an untagged call for the owner's prompt matched %s, want ownerReflect: the control that shows the rule is reachable", ruleName(got))
	}
	if got := r.MatchRule(tagged(ResolveRequest{Level: airoute.LevelStrong, PromptName: "reflect"})); got == nil || got.Name != "backgroundLane" {
		t.Errorf("a tagged call for the owner's prompt matched %s, want backgroundLane: an owner's rule is not consulted for a background call", ruleName(got))
	}
	// And a rule keyed on the tag itself is never reached.
	if got := r.MatchRule(tagged(ResolveRequest{Level: airoute.LevelStrong, PromptName: "other"})); got != nil && got.Name == "ownerBackground" {
		t.Errorf("an owner's rule keyed on the background tag was reached; the shipped backgroundLane rule is locked and evaluates first")
	}
}
