package memql

// routing_review_apply.go -- the routing configuration's half of the nightly
// routing review (epic memql#5146 D5; design record
// docs/superpowers/specs/2026-11-08-routing-review-apply-design.md).
//
// The fold renders a demotion as rule TEXT and a person approves that text by
// its hash. Two questions are answered here, against the same rule set the
// router walks (shipped and product-bundle rules plus the routing
// configuration document, PolicyRegistry.SnapshotRouting):
//
//   - RoutingRuleBeatenBy: would this rule ever decide a call? The fold asks
//     before it opens an approval, so a person is never asked to approve a
//     rule that cannot take effect (decision D3).
//   - ApplyRoutingReview: save exactly the approved text, or retire the
//     demotion a promotion names, through the same checks routingRuleSave
//     runs (stageConfiguredRule).
//
// WHY "BEATEN" IS A REAL CASE AND NOT A CORNER. The router takes the first
// matching rule, and every shipped rule is @locked, which evaluates before any
// custom rule whatever its precedence (RuleRegistry.Finalize). fastLane,
// reasoningParks and embeddingsBound match every call at fast, reasoning and
// embeddings, so a demotion saved at those levels is a rule no call ever
// reaches: saved, listed, and inert.

import (
	"context"
	"fmt"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// ruleCovers reports whether every call r matches is matched by earlier too.
//
// CONSERVATIVE BY CONSTRUCTION: earlier covers r only when every condition
// earlier states, r states as well, with the same value -- for `touches`, r's
// prefix extends earlier's, because a footprint entry starting with the longer
// prefix starts with the shorter one. A key earlier states and r does not means
// r also matches calls earlier would not, so earlier does not cover it. A key
// written EMPTY is a condition, not an absence (RuleWhen.Present), and is
// compared like any other.
//
// The cost of the conservatism is stated in the design record: a rule beaten
// only by a COMBINATION of earlier rules is not detected, and is proposed.
func ruleCovers(earlier, r *RuleConfig) bool {
	if earlier == nil || r == nil {
		return false
	}
	for _, key := range RuleWhenKeys {
		if !earlier.When.Has(key) {
			continue
		}
		if !r.When.Has(key) {
			return false
		}
		a, b := ruleWhenValue(earlier.When, key), ruleWhenValue(r.When, key)
		if key == "touches" {
			if !strings.HasPrefix(b, a) {
				return false
			}
			continue
		}
		if a != b {
			return false
		}
	}
	return true
}

// ruleBeatenBy returns the first rule the router would try before r that
// matches every call r matches, or nil when r can decide some call.
//
// "Before r" is the router's own order (RuleRegistry.Ordered): a locked rule
// other than `default`, which is the floor and always sorts last, or an
// unlocked rule of HIGHER precedence. An equal precedence is a tie, which the
// save refuses on its own terms; it is not "before". A rule with r's own name
// is r's saved copy, not a rival.
func ruleBeatenBy(rules *RuleRegistry, r *RuleConfig) *RuleConfig {
	if rules == nil || r == nil {
		return nil
	}
	for _, earlier := range rules.Ordered() {
		if earlier.Name == r.Name || earlier.Name == DefaultRuleName {
			continue
		}
		if !earlier.Locked && earlier.Precedence <= r.Precedence {
			continue
		}
		if ruleCovers(earlier, r) {
			return earlier
		}
	}
	return nil
}

// parseApprovedRule reads approved rule text into the value the router acts on.
//
// The text is PARSED rather than rebuilt from the approval's other fields
// (decision D2): the approval's hash covers the text, so what is saved is
// exactly what the person read. The text comes from the tree's one renderer
// (component/routingrules.GenerateRule), which never writes either annotation
// refused below; the refusals are what keep that true of anything else that
// reaches here.
func parseApprovedRule(source string) (*RuleConfig, error) {
	decl, err := languageParser.ParseRuleDecl(source)
	if err != nil {
		return nil, fmt.Errorf("the approved rule text does not parse: %w", err)
	}
	if decl.Locked {
		return nil, fmt.Errorf("rule %q carries @locked, which only the shipped rules may declare: a saved rule "+
			"carrying it would evaluate before every rule an owner wrote", decl.Name)
	}
	if decl.Disabled {
		return nil, fmt.Errorf("rule %q carries @disabled, so saving it would decide no call", decl.Name)
	}
	return ruleDeclToRuleConfig(decl, routingConfigurationSource)
}

// RoutingRuleBeatenBy names the rule that would match every call this rule
// matches, before it, if it were saved now -- or "" when it could decide a
// call. It saves nothing.
//
// It reads the configuration as it is and does NOT stage the rule, so a rule
// that would tie is still answered: the tie is the save's refusal to make, with
// the person reading it, and hiding the answer behind it would make the fold
// propose a rule that is beaten as well as tied.
func (e *MemQLEngine) RoutingRuleBeatenBy(ctx context.Context, source string) (string, error) {
	r, err := parseApprovedRule(source)
	if err != nil {
		return "", err
	}
	if e.rules == nil {
		// Unknown is not "nothing beats it": answering "" here would let the
		// fold propose a rule nobody checked.
		return "", fmt.Errorf("no rule set is loaded on this node, so whether rule %q could decide a call is unknown", r.Name)
	}
	doc, _, err := e.policies.configuration(ctx)
	if err != nil {
		return "", err
	}
	rules, err := configuredRules(e.rules, doc)
	if err != nil {
		return "", err
	}
	if beaten := ruleBeatenBy(rules, r); beaten != nil {
		return beaten.Name, nil
	}
	return "", nil
}
