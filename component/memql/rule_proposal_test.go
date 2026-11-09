package memql

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/airoute"
)

// TestRuleLevelsMatchAirouteLevels is the parity gate RuleLevels names.
//
// The loader must be able to refuse a rule whose level is outside the closed
// set WITHOUT importing airoute, so the four words are spelled here. That is a
// copy, and a copy that drifts is worse than no check: a level airoute added
// and this list did not would be refused by the loader as "not one of fast,
// strong, reasoning, embeddings" -- a rule the person wrote correctly, refused
// with a message naming the wrong closed set.
func TestRuleLevelsMatchAirouteLevels(t *testing.T) {
	var fromAiroute []string
	for _, l := range airoute.Levels() {
		fromAiroute = append(fromAiroute, string(l))
	}
	if len(fromAiroute) == 0 {
		t.Fatal("airoute reported no levels -- this gate compared nothing")
	}
	if strings.Join(RuleLevels, ",") != strings.Join(fromAiroute, ",") {
		t.Errorf("RuleLevels is %v; airoute.Levels() is %v.\n"+
			"They must agree IN ORDER as well as in membership: RuleLevelNames renders this list "+
			"into the refusal a person reads, and a list that disagrees names a closed set that is "+
			"not the one being enforced.", RuleLevels, fromAiroute)
	}
}

// shippedForTest is a view of the shipped names with a known, NON-UNIFORM
// content: two rules and two policies, so a check that answers the same way
// for every input fails rather than passing by coincidence.
type shippedForTest struct{ rules, policies map[string]bool }

func (s shippedForTest) HasRule(n string) bool   { return s.rules[n] }
func (s shippedForTest) HasPolicy(n string) bool { return s.policies[n] }

func testShipped() shippedForTest {
	return shippedForTest{
		rules:    map[string]bool{"default": true, "reasoningParks": true},
		policies: map[string]bool{"localFirst": true, "localOnly": true},
	}
}

func validProposal() RuleProposal {
	return RuleProposal{
		Name:          "myOperatorRule",
		When:          map[string]string{"prompt": "agentReply", "role": "operator"},
		Level:         "reasoning",
		Policy:        "localOnly",
		OnUnavailable: "park",
		Excludes:      []string{"fleet:qwen3.5:7b"},
	}
}

func TestValidateRuleProposalAcceptsAGoodOne(t *testing.T) {
	if err := ValidateRuleProposal(validProposal(), testShipped()); err != nil {
		t.Fatalf("a valid proposal was refused: %v", err)
	}
}

// TestValidateRuleProposalRefusesAnUncheckableRequest is the fail-closed arm.
//
// A nil view is not "nothing is shipped": it is "the shipped set could not be
// read", and checking a name against an unknown set admits every name the
// check exists to refuse.
func TestValidateRuleProposalRefusesAnUncheckableRequest(t *testing.T) {
	err := ValidateRuleProposal(validProposal(), nil)
	if err == nil {
		t.Fatal("a proposal with no view of the shipped names must be refused, not admitted")
	}
	if !strings.Contains(err.Error(), "would admit every name") {
		t.Errorf("the refusal must say why an unknown set is not a permissive one; got %q", err)
	}
}

func TestValidateRuleProposalRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mut   func(*RuleProposal)
		wants string
	}{
		{"no name", func(p *RuleProposal) { p.Name = "" }, "needs a name"},
		{"not an identifier", func(p *RuleProposal) { p.Name = "my rule" }, "bare identifier"},
		{"a shipped rule's name", func(p *RuleProposal) { p.Name = "reasoningParks" }, "shipped rule"},
		{"no policy", func(p *RuleProposal) { p.Policy = "" }, "names no policy"},
		{"a policy: reference", func(p *RuleProposal) { p.Policy = "policy:localFirst" }, "policy NAME"},
		{"an unregistered policy", func(p *RuleProposal) { p.Policy = "cheapEverything" }, "not registered on this cluster"},
		{"an unknown level", func(p *RuleProposal) { p.Level = "smart" }, "is not one of"},
		{"an unknown onUnavailable", func(p *RuleProposal) { p.OnUnavailable = "retry" }, "onUnavailable"},
		{"an unknown condition key", func(p *RuleProposal) { p.When = map[string]string{"model": "gpt"} }, "not a condition key"},
		{"an unknown when level", func(p *RuleProposal) { p.When = map[string]string{"level": "smart"} }, "never be true"},
		{"a quote in a value", func(p *RuleProposal) { p.When = map[string]string{"prompt": `a"b`} }, "cannot carry"},
		{"a bad exclude", func(p *RuleProposal) { p.Excludes = []string{"cloud:cheapest"} }, "exclude"},
		{"the retired fleet wildcard", func(p *RuleProposal) { p.Excludes = []string{"fleet:*"} }, "fleet:strongest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := validProposal()
			tc.mut(&p)
			err := ValidateRuleProposal(p, testShipped())
			if err == nil {
				t.Fatalf("the proposal was accepted; it should be refused for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("refusal %q does not contain %q", err, tc.wants)
			}
		})
	}
}
