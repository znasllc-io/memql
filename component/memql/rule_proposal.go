package memql

// rule_proposal.go -- would this rule load? (epic memql#5137)
//
// A RULE ARRIVES BY TWO FRONT DOORS AND MUST BE JUDGED IDENTICALLY. An
// operator fills in a form in MemQL OS; an operator types a sentence and a
// model compiles it (routingRuleDescribe, rule_assist.go). Both are validated
// and saved into the routing configuration through routingRuleValidate and
// routingRuleSave (routing_configuration_rules.go), so anything one door lets
// through that the other does not is a rule that validates and then fails --
// with the person holding a confident account of what it does.
//
// WHY HERE RATHER THAN IN EITHER DOOR. `component/router`'s compiled-rule
// check is a LEAF module the root depends on, so it cannot import the root
// module's door. The first attempt to share by importing (when the form door
// was component/routingrules, retired 2026-11-08) was a dependency-direction
// violation the module-boundaries lane caught -- correctly, and invisibly to
// both `go build ./...` and `make test`. This package is what they already
// share: it owns RuleConfig, RuleWhenKeys and the OnUnavailable values, and it
// already imports the policy-entry parser.
//
// THE POINT IS TO SHARE THE PREDICATES, NOT TO SHARE A LIST. The state this
// replaces was four literals restated in component/router -- the shipped rule
// names, the shipped policy names, the @when keys and the onUnavailable values
// -- and three of them were wrong when they were written. One of them named a
// POLICY as a rule and missed four of the six rules the tree ships, so the
// gate that stops a custom rule shadowing a shipped one was, for
// `reasoningParks`, watching nothing. Lists like that do not drift because
// somebody is careless; they drift because the thing they copy is somewhere
// else and nothing connects them.

import (
	"fmt"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// RuleProposal is a rule somebody wants to activate, in the shape both doors
// can produce. It is deliberately NOT RuleConfig: a proposal has not been
// parsed, carries the author's raw strings, and has no source file.
type RuleProposal struct {
	Name          string
	When          map[string]string
	Level         string
	Policy        string
	OnUnavailable string
	Excludes      []string
}

// ShippedNames answers "is this name the embedded tree's".
//
// It is an interface so each door can pass its own view -- the live registries
// on a running engine, the corpus itself in a test -- and so the one live
// implementation is the engine's own registries rather than a second list that
// would drift from them.
type ShippedNames interface {
	HasRule(name string) bool
	HasPolicy(name string) bool
}

// ValidateRuleProposal reports the first reason this rule could not be
// activated, or nil.
//
// It duplicates checks the parser also makes, and that duplication is the
// point: the parser's message is about a construct the operator never wrote,
// at a line number in a file that does not exist.
func ValidateRuleProposal(p RuleProposal, shipped ShippedNames) error {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return fmt.Errorf("a rule needs a name")
	}
	if !isRuleIdentifier(name) {
		return fmt.Errorf("rule name %q must be a bare identifier: letters and digits, starting with a letter", name)
	}
	if shipped == nil {
		return fmt.Errorf("rule %q cannot be checked: no view of the shipped names was supplied, and a check "+
			"against an unknown set would admit every name it exists to refuse", name)
	}
	if shipped.HasRule(name) {
		return fmt.Errorf("%q is a shipped rule. A shipped rule is re-read from the embedded tree on every "+
			"boot, so a custom rule taking its name would be replaced at the next restart with nothing "+
			"saying so. Choose another name; a custom rule can still take precedence with @precedence", name)
	}

	policy := strings.TrimSpace(p.Policy)
	if policy == "" {
		return fmt.Errorf("rule %q names no policy: a rule that selects no chain resolves nothing", name)
	}
	if err := languageParser.ValidatePolicyEntry(policy); err != nil {
		return fmt.Errorf("rule %q: %w", name, err)
	}
	if _, isPolicyRef := languageParser.IsPolicyEntry(policy); isPolicyRef {
		return fmt.Errorf("rule %q: @policy takes a policy NAME, not a policy: reference -- write %q",
			name, strings.TrimPrefix(policy, "policy:"))
	}
	// THE POLICY MUST EXIST, and this is the only place that can say so before
	// the rule is live. ValidatePolicyEntry above checks the FORM of a chain
	// entry, which a policy name passes trivially -- any identifier does. A
	// rule naming a policy nobody registered renders, loads, and then refuses
	// EVERY CALL IT MATCHES at request time with "names a policy that is not
	// registered" on the door report, which is a place the person who wrote
	// the rule is not looking. They activated it and were told it was fine.
	//
	// A NAME NOBODY MINTS IS THE COMMON CASE, not a typo: minting a policy is
	// how somebody expresses a chain, and it is exactly what neither door may
	// do -- a provider chain from a form or a sentence is a spending decision
	// in a place no review looks.
	if !shipped.HasPolicy(policy) {
		return fmt.Errorf("rule %q names policy %q, which is not registered on this cluster. A rule may NAME "+
			"a policy and may not mint one; a rule naming a policy that does not exist loads cleanly and "+
			"then refuses every call it matches", name, policy)
	}

	if lvl := strings.TrimSpace(p.Level); lvl != "" && !IsRuleLevel(lvl) {
		return fmt.Errorf("rule %q: level %q is not one of %s", name, lvl, RuleLevelNames())
	}
	switch strings.TrimSpace(p.OnUnavailable) {
	case "", OnUnavailableDegrade, OnUnavailablePark:
	default:
		return fmt.Errorf("rule %q: onUnavailable %q is not %q or %q",
			name, p.OnUnavailable, OnUnavailableDegrade, OnUnavailablePark)
	}

	for key, value := range p.When {
		if !IsRuleWhenKey(key) {
			return fmt.Errorf("rule %q: %q is not a condition key -- the closed set is %s",
				name, key, RuleWhenKeyNames())
		}
		if strings.ContainsAny(value, `"\`+"\n") {
			return fmt.Errorf("rule %q: the value for %q contains a quote, a backslash or a newline, "+
				"which a rendered annotation cannot carry", name, key)
		}
	}
	if lvl, ok := p.When["level"]; ok && lvl != "" && !IsRuleLevel(lvl) {
		return fmt.Errorf("rule %q: @when(level=%q) is not one of %s -- "+
			"a level outside the closed set is a condition that can never be true, which presents as a "+
			"rule that is simply never reached", name, lvl, RuleLevelNames())
	}

	for _, entry := range p.Excludes {
		if err := languageParser.ValidatePolicyEntry(entry); err != nil {
			return fmt.Errorf("rule %q: exclude %q: %w", name, entry, err)
		}
	}
	return nil
}

// RuleLevels is the closed level vocabulary, in strength order.
//
// It is spelled here rather than imported from core/airoute because this
// package is below airoute for the loader's purposes and the loader must be
// able to refuse a bad level without one. The parity gate is
// TestRuleLevelsMatchAirouteLevels.
var RuleLevels = []string{"fast", "strong", "reasoning", "embeddings"}

// IsRuleLevel reports whether s is one of the closed set.
func IsRuleLevel(s string) bool {
	for _, known := range RuleLevels {
		if known == s {
			return true
		}
	}
	return false
}

// RuleLevelNames renders the set for an error message.
func RuleLevelNames() string { return strings.Join(RuleLevels, ", ") }

// isRuleIdentifier reports whether s is a bare lowerCamelCase-shaped
// identifier: letters and digits, starting with a letter.
func isRuleIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
