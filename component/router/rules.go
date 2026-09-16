package router

// rules.go -- matching a call against the loaded rule corpus (epic memql#5127,
// task memql#5131, design D5).
//
// A RULE IS DATA IN component/memql AND A DECISION HERE. That package owns
// what a rule is and the ORDER the set is walked in, both properties of the
// loaded corpus computed once at load so every replica agrees with no shared
// state. This file owns what a rule DOES: whether it matches a request.
//
// THE FIRST MATCH WINS, and there is always one. The shipped `default` rule
// states no conditions, so it matches every call, and the registry refuses to
// finalize without it -- which is why nothing downstream has a "no rule
// matched" branch to get wrong. A call that matched nothing would have no
// policy, and the only thing left to do with it would be to invent one.

import (
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

// MatchRule returns the first rule that matches, in the registry's evaluation
// order: locked rules first, then unlocked, precedence descending within each
// half.
//
// It returns nil only when there is nothing to match against -- a nil or
// unfinalized registry -- which the resolver treats as a refusal rather than
// as a licence to pick a chain itself.
func (r *Router) MatchRule(req ResolveRequest) *memql.RuleConfig {
	if r == nil || r.rules == nil {
		return nil
	}
	return matchRuleFrom(r.rules, req)
}

func matchRuleFrom(registry *memql.RuleRegistry, req ResolveRequest) *memql.RuleConfig {
	if registry == nil {
		return nil
	}
	for _, rule := range registry.Ordered() {
		if ruleMatches(rule, req) {
			return rule
		}
	}
	return nil
}

// ruleMatches reports whether every PRESENT @when key holds for this request.
//
// PRESENT, not non-empty. An absent key is no condition at all; a key an
// author wrote with an empty value is a condition that matches only an empty
// value -- `@when(prompt="")` selects the Go call sites that render no prompt.
// Collapsing the two would make every rule with an empty value match
// everything, which is the failure mode where a rule an author wrote to narrow
// a case silently widens it to the whole cluster.
//
// All present keys are ANDed. There is no OR: two conditions that should hold
// separately are two rules, which the precedence order can then rank -- and
// which each get their own line on the decision record.
func ruleMatches(rule *memql.RuleConfig, req ResolveRequest) bool {
	if rule == nil {
		return false
	}
	w := rule.When
	if w.Has("level") && w.Level != string(req.Level) {
		return false
	}
	if w.Has("modality") && w.Modality != string(req.Modality) {
		return false
	}
	if w.Has("prompt") && w.Prompt != req.PromptName {
		return false
	}
	// ROLE AND ACTORROLE ARE DIFFERENT QUESTIONS, and the split is the point.
	// `role` is what is ACTING -- the agent's own role slug. `actorRole` is
	// who is WATCHING -- the cluster role of the human whose session drove the
	// turn. An operator reading a colleague's ordinary agent is not an
	// operator turn, and a rule that could not tell those apart would route on
	// the audience.
	if w.Has("role") && w.Role != req.Role {
		return false
	}
	if w.Has("actorRole") && w.ActorRole != req.ActorRole {
		return false
	}
	if w.Has("tag") && !req.HasTag(w.Tag) {
		return false
	}
	if w.Has("touches") && !touchesMatch(req.Touches, w.Touches) {
		return false
	}
	return true
}

// touchesMatch reports whether ANY of the call's footprint entries begins with
// the rule's value.
//
// STARTSWITH, because a footprint is a concept id and concept ids nest: a rule
// about `v1:identity:` is a rule about every identity concept, and spelling
// each one out would be a rule that goes stale the next time a concept is
// added. It is the same prefix semantics the DSL's own `startsWith` filter
// operator gives a query over the same ids.
//
// An empty rule value therefore matches any call that HAS a footprint, and
// that literal reading is deliberate: special-casing empty to mean "no
// footprint at all" would give one spelling two meanings depending on which
// @when key it sat under.
func touchesMatch(touches []string, prefix string) bool {
	for _, t := range touches {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return false
}

// applyExcludes removes a rule's @exclude entries from a chain.
//
// It matches on the ENTRY AS WRITTEN, which is what makes the exclusion
// legible: an operator demoting `fleet:qwen3.5:7b` writes exactly the string a
// policy would have written, and the removal is visible in the decision's
// considered list rather than inferred from an absence.
//
// A chain that empties out is left EMPTY rather than falling back to the
// unexcluded one. The rule's onUnavailable then decides, which is the answer
// an operator asked for when they excluded every entry.
func applyExcludes(chain []string, excludes []string) ([]string, []string) {
	if len(excludes) == 0 {
		return chain, nil
	}
	banned := bannedSet(excludes)
	kept := make([]string, 0, len(chain))
	var removed []string
	for _, entry := range chain {
		if banned[strings.TrimSpace(entry)] {
			removed = append(removed, entry)
			continue
		}
		kept = append(kept, entry)
	}
	return kept, removed
}

// bannedSet is the exclude list as a lookup, trimmed and with blanks dropped.
func bannedSet(excludes []string) map[string]bool {
	if len(excludes) == 0 {
		return nil
	}
	banned := make(map[string]bool, len(excludes))
	for _, e := range excludes {
		if e = strings.TrimSpace(e); e != "" {
			banned[e] = true
		}
	}
	return banned
}
