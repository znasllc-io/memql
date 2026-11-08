package router

import (
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

// A described rule: a sentence, compiled once, into a rule a person confirms
// (epic memql#5137, D7).
//
// ===========================================================================
// COMPILED ONCE, NOT EVALUATED PER REQUEST
// ===========================================================================
// A person types "nothing that touches campaign recipients leaves the fleet".
// The alternative to compiling that is asking a model, per request, whether
// this request touches campaign recipients -- a model in front of every model
// call, which is the cost the whole program exists to avoid. So the sentence is
// compiled to a `rule` once, pinned, and from then on the router matches
// structurally at no cost.
//
// The owner's cache argument, adopted: the sentence does not change at request
// time, so its hash is the key and the compiled rule is the value.
//
// ===========================================================================
// THE COMPILED FORM IS THE CONTRACT, THE SENTENCE IS THE RECORD
// ===========================================================================
// What runs is the rule. The sentence is kept as the rule's description so a
// later reader can see what was meant -- but if the two disagree, the rule is
// what the cluster does, and the simulation is how a person finds that out
// BEFORE confirming rather than from a bill afterwards.

// WhenKeys is the closed `@when` vocabulary the compiler hands to the model as
// the words it may use (epic memql#5127).
//
// IT IS THE LOADER'S OWN LIST, NOT A COPY OF IT. An earlier draft restated the
// seven literals here on the argument that a prompt vocabulary and a parser
// vocabulary are different things -- which is true right up until they differ,
// and then the compiler emits rules that refuse to load while telling the
// person, confidently, what their new rule does. Deriving costs nothing and
// removes the only way those two can disagree.
func WhenKeys() []string { return append([]string(nil), memql.RuleWhenKeys...) }

// OnUnavailableValues is the closed set for what happens when a rule's policy
// resolves to nothing.
var OnUnavailableValues = []string{"degrade", "park"}

// CompiledRule is what a sentence becomes.
type CompiledRule struct {
	// Name is the rule's identifier, lowerCamelCase.
	Name string
	// Conditions are the `@when` keys. EMPTY IS LEGAL and means "every call" --
	// that is how the shipped `default` rule is the floor -- but a compiled
	// rule with no conditions is almost certainly not what a person meant, and
	// Validate says so rather than refusing: it is their cluster.
	Conditions map[string]string
	// Policy the rule routes to. Required.
	Policy string
	// Level overrides the call's, when the sentence said one.
	Level string
	// Precedence decides between rules that both match; higher wins.
	Precedence int
	// OnUnavailable is degrade or park. Empty means degrade.
	OnUnavailable string
	// Excludes are `fleet:<modelId>` entries the rule rules out.
	Excludes []string
	// Restatement is the compiler's one-sentence account of what it produced,
	// shown beside the person's own words. NOT the sentence back -- a compiler
	// that echoes the input proves nothing about what it understood.
	Restatement string
	// Sentence is what the person typed, kept as the rule's description.
	Sentence string
}

// Warning is a thing a person should see before confirming, which is not a
// reason to refuse.
type Warning struct {
	Field string
	Text  string
}

// Validate checks a compiled rule against the closed vocabularies.
//
// IT SEPARATES REFUSALS FROM WARNINGS, and the split is the design. A rule
// naming a policy that does not exist cannot be activated -- the loader would
// refuse it and the person would be told nothing useful. A rule that matches
// every call CAN be activated and is probably a mistake, so it is surfaced and
// left to them: it is their cluster, and a compiler that refused anything
// surprising would be unable to express the shipped rules themselves.
// ShippedNames answers "is this name the embedded tree's", so the compiler can
// refuse a rule the loader would refuse.
//
// It is the SHARED contract rather than a local one: the form door,
// routingRuleSave in component/memql, passes the same view, and the live
// implementation on both sides is the engine's own registries.
type ShippedNames = memql.ShippedNames

// Proposal is the compiled rule in the shape the shared validator takes, so
// one function decides what may be activated.
//
// THE COMPILER IS A DIFFERENT FRONT DOOR TO THE SAME PIPELINE, and that is why
// this conversion exists rather than a second set of checks. An operator
// filling in a form and an operator typing a sentence must be able to activate
// exactly the same set of rules; anything the compiler let through that the
// form refuses would be a rule that validates, renders and then fails to load,
// with the person holding a confident restatement of it.
func (r CompiledRule) Proposal() memql.RuleProposal {
	return memql.RuleProposal{
		Name:          strings.TrimSpace(r.Name),
		When:          r.Conditions,
		Level:         strings.TrimSpace(r.Level),
		Policy:        strings.TrimSpace(r.Policy),
		OnUnavailable: strings.TrimSpace(r.OnUnavailable),
		Excludes:      r.Excludes,
	}
}

// Validate checks a compiled rule and reports what a person should see before
// confirming it.
//
// IT SEPARATES REFUSALS FROM WARNINGS, and the split is the design. A rule
// naming a policy that does not exist cannot be activated -- the loader would
// refuse it and the person would be told nothing useful. A rule that matches
// every call CAN be activated and is probably a mistake, so it is surfaced and
// left to them: it is their cluster, and a compiler that refused anything
// surprising would be unable to express the shipped rules themselves.
//
// EVERY REFUSAL IS memql.ValidateRuleProposal'S. An earlier draft of this
// function re-implemented all of them here against four hardcoded lists -- the
// shipped rule names, the shipped policy names, the `@when` keys and the
// onUnavailable values. Three of those were already wrong when they were
// written: the rule list named a POLICY as a rule and missed four of the six
// rules the tree actually ships, so a sentence could take `reasoningParks`'
// name and get exactly the load-order coin flip the list existed to prevent.
// Lists like that do not drift because somebody is careless; they drift
// because the thing they copy is somewhere else and nothing connects them.
// `shipped` reads the live registries.
func (r CompiledRule) Validate(shipped ShippedNames) ([]Warning, error) {
	if err := memql.ValidateRuleProposal(r.Proposal(), shipped); err != nil {
		return nil, err
	}
	for _, ex := range r.Excludes {
		// THE SHARED VALIDATOR CHECKS THAT AN EXCLUSION IS A VALID POLICY
		// ENTRY; this narrows it to the fleet door, which is the only one an
		// exclusion has ever meant. A sentence saying "not on the laptop"
		// names a machine's model; there is no reading of it that names a
		// vendor record. It stays HERE rather than moving into the shared
		// validator because the form door does not impose it -- an operator
		// typing into a field may have a reason a compiler inferring from
		// prose does not.
		if !strings.HasPrefix(ex, "fleet:") {
			return nil, fmt.Errorf("rule %q excludes %q; an exclusion names a fleet model as fleet:<modelId>", r.Name, ex)
		}
	}

	var warnings []Warning
	if len(r.Conditions) == 0 {
		warnings = append(warnings, Warning{
			Field: "when",
			Text:  "This rule matches every call. That is legal and is how the shipped default works, but it is rarely what a sentence about a particular kind of work means.",
		})
	}
	if strings.TrimSpace(r.Restatement) == "" {
		warnings = append(warnings, Warning{
			Field: "restatement",
			Text:  "The compiler produced no restatement, so there is nothing to check the rule against except the rule itself.",
		})
	}
	sort.SliceStable(warnings, func(i, j int) bool { return warnings[i].Field < warnings[j].Field })
	return warnings, nil
}

// CompilerPromptName is the prompt that does the compiling.
//
// NAMED AS A CONSTANT because the shipped locked rule `compilerLocalOnly`
// matches on it (`@when(prompt="compileRule")`). If the two ever disagree the
// compiler resolves through the ordinary rules instead -- which means a rule
// about how to spend money could be compiled by a paid model, silently, and the
// only sign would be a line in a decision record nobody was looking at.
const CompilerPromptName = "compileRule"

// CompilerRuleName is the locked rule that keeps the compiler local.
const CompilerRuleName = "compilerLocalOnly"

// CompilerPolicyName is the policy that rule routes to.
const CompilerPolicyName = "localOnly"
