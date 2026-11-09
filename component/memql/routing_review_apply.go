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
	"encoding/json"
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

// RoutingReviewChange is what an approved routing review asks of the routing
// configuration: SAVE the approved rule text (a demotion), or RETIRE a saved
// rule (a promotion retires the demotion it lifts). Exactly one is set.
//
// It names no direction on purpose. "demotion" and "promotion" are the
// evidence fold's vocabulary (component/router), which this package cannot
// import; the work integration maps a direction to a change, and this side
// only has to know what a change does to the document.
type RoutingReviewChange struct {
	// SaveName is the rule the approval says the text declares; text naming
	// another rule is refused rather than saved under a name nobody approved.
	SaveName string
	// SaveSource is the exact approved rule text, saved as parsed.
	SaveSource string
	// RetireName is the saved rule to remove.
	RetireName string
}

// RoutingReviewApplied is what the routing configuration holds afterwards.
type RoutingReviewApplied struct {
	Rule string
	// Revision is the configuration revision the change landed in, or the
	// current one when there was nothing to change.
	Revision int64
	// Changed is false when the document already said what the approval says:
	// the rule was saved identically, or the rule to retire is already gone.
	Changed bool
}

// routingReviewAttempts bounds the re-validation when another writer moves the
// configuration between this apply's read and its swap. Each attempt re-reads
// and re-checks against the newer document, so a proposal is refused only for
// what routing is now, never merely because it changed since the fold ran.
const routingReviewAttempts = 3

// ApplyRoutingReview makes an approved routing review's change to the routing
// configuration (design record section 2).
//
// The caller must be allowed to change routing (RoutingConfigurationAuthor);
// the revision records them as its author. A save refuses text that declares
// another rule, a rule tying with a saved or product-bundle rule on
// precedence (naming it), and a rule an earlier rule always beats (naming
// it), and otherwise passes routingRuleSave's own checks (stageConfiguredRule).
// A retire refuses a shipped or product-bundle rule. A change the document
// already reflects writes no revision, which is what makes a retried decide
// harmless.
func (e *MemQLEngine) ApplyRoutingReview(ctx context.Context, change RoutingReviewChange) (RoutingReviewApplied, error) {
	actor, err := RoutingConfigurationAuthor(ctx)
	if err != nil {
		return RoutingReviewApplied{}, err
	}
	save := strings.TrimSpace(change.SaveSource) != ""
	retire := strings.TrimSpace(change.RetireName) != ""
	if save == retire {
		return RoutingReviewApplied{}, fmt.Errorf("a routing review change saves one rule or retires one, not both and not neither")
	}
	if e.policies == nil || e.rules == nil {
		return RoutingReviewApplied{}, fmt.Errorf("routing configuration is unavailable on this node")
	}
	var approved *RuleConfig
	retireName := strings.TrimSpace(change.RetireName)
	if save {
		if approved, err = parseApprovedRule(change.SaveSource); err != nil {
			return RoutingReviewApplied{}, err
		}
		if approved.Name != strings.TrimSpace(change.SaveName) {
			return RoutingReviewApplied{}, fmt.Errorf("the approved text declares rule %q, but the review proposes %q; nothing was saved", approved.Name, change.SaveName)
		}
		if !isRuleIdentifier(approved.Name) {
			// A REVIEW RAISED BEFORE 2026-11-08 names its rule with underscores
			// (component/router's RuleName then), a shape no saved rule may take
			// (ValidateRuleProposal, and MemQL OS's rule forms). Approving it can
			// never save it, so the refusal says so and names the way out,
			// rather than quoting an identifier rule the person never wrote.
			return RoutingReviewApplied{}, fmt.Errorf("this review proposes rule %q under a name from before reviews could be applied, which a saved rule cannot take, "+
				"so approving it can never change routing. Decline it; the nightly review proposes the change again under a name that can be saved "+
				"while the evidence still holds", approved.Name)
		}
	} else if _, shipped := e.rules.Lookup(retireName); shipped {
		return RoutingReviewApplied{}, fmt.Errorf("%q is a shipped rule and cannot be removed; out-rank it with a custom rule at a higher precedence", retireName)
	}
	store := e.policies.attachedStore()
	if store == nil {
		return RoutingReviewApplied{}, fmt.Errorf("persistent routing storage is unavailable")
	}
	for attempt := 1; ; attempt++ {
		doc, defaults, err := e.policies.configuration(ctx)
		if err != nil {
			return RoutingReviewApplied{}, err
		}
		var next PolicyDocument
		var result RoutingReviewApplied
		if save {
			next, result, err = e.stageApprovedRule(doc, defaults, approved)
		} else {
			next, result = retireSavedRule(doc, retireName)
		}
		if err != nil {
			return RoutingReviewApplied{}, err
		}
		if !result.Changed {
			result.Revision = doc.Revision
			return result, nil
		}
		next.Revision = doc.Revision + 1
		next.UpdatedBy = actor
		swapErr := store.CompareAndSwap(ctx, doc.Revision, next)
		if swapErr == nil {
			result.Revision = next.Revision
			return result, nil
		}
		// Retry only when the revision demonstrably moved: any other failure
		// (the database refusing, a lost connection) is reported as it is.
		fresh, _, readErr := e.policies.configuration(ctx)
		if readErr != nil || fresh.Revision == doc.Revision || attempt >= routingReviewAttempts {
			return RoutingReviewApplied{}, swapErr
		}
	}
}

// stageApprovedRule stages the approved rule into doc, or reports that doc
// already holds it exactly.
func (e *MemQLEngine) stageApprovedRule(doc PolicyDocument, defaults map[string]*PolicyConfig, approved *RuleConfig) (PolicyDocument, RoutingReviewApplied, error) {
	result := RoutingReviewApplied{Rule: approved.Name}
	if existing, ok := doc.Rules[approved.Name]; ok && sameSavedRule(existing, *approved) {
		return doc, result, nil
	}
	current, err := configuredRules(e.rules, doc)
	if err != nil {
		return doc, result, err
	}
	// Named before the generic tie refusal stageConfiguredRule would give,
	// because the person reading this is deciding an approval and the useful
	// sentence names the rule in the way and where to change it. Every
	// proposal the fold renders sits at one precedence, so this is also how a
	// second demotion at a level meets the first (decision D4).
	if holder := precedenceHolder(current, approved); holder != nil {
		return doc, result, fmt.Errorf("rule %q already holds precedence %d, and two custom rules cannot share one: "+
			"remove or re-number %q in Fleet > Routing > Rules and approve again, or decline this proposal",
			holder.Name, approved.Precedence, holder.Name)
	}
	staged, rules, err := e.stageConfiguredRule(doc, defaults, approved)
	if err != nil {
		return doc, result, err
	}
	if beaten := ruleBeatenBy(rules, approved); beaten != nil {
		return doc, result, fmt.Errorf("rule %q would never decide a call: %q is tried first and matches every call it matches, "+
			"so saving it would change no routing", approved.Name, beaten.Name)
	}
	result.Changed = true
	return staged, result, nil
}

// retireSavedRule removes name from doc's saved rules, or reports that it is
// already gone.
func retireSavedRule(doc PolicyDocument, name string) (PolicyDocument, RoutingReviewApplied) {
	result := RoutingReviewApplied{Rule: name}
	if _, ok := doc.Rules[name]; !ok {
		return doc, result
	}
	next := doc
	next.Rules = make(map[string]RuleConfig, len(doc.Rules))
	for n, rule := range doc.Rules {
		if n != name {
			next.Rules[n] = rule
		}
	}
	result.Changed = true
	return next, result
}

// precedenceHolder is the unlocked rule, other than r's own saved copy, that
// already holds r's precedence -- the rule RuleRegistry.Finalize would refuse
// r as tying with.
func precedenceHolder(rules *RuleRegistry, r *RuleConfig) *RuleConfig {
	if rules == nil || r == nil {
		return nil
	}
	for _, other := range rules.Ordered() {
		if other.Name != r.Name && !other.Locked && other.Precedence == r.Precedence {
			return other
		}
	}
	return nil
}

// sameSavedRule compares two rules as the routing configuration stores them.
// The document round-trips through JSON, so comparing the encodings compares
// exactly what a reader of the document would see.
func sameSavedRule(a, b RuleConfig) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(x) == string(y)
}

// attachedStore is the routing configuration's store, or nil on a registry
// with none attached.
func (r *PolicyRegistry) attachedStore() PolicyStore {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.store
}
