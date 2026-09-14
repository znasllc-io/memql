package parser

import (
	"fmt"
	"github.com/znasllc-io/memql/component/language/annotations"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/core/airoute"
)

// rule_decl.go parses the struct-form `rule NAME { }` declaration -- the
// construct that maps a call's declared metadata to a policy (design D5, epic
// memql#5127). It mirrors policy_decl.go: the leading attribute set is supplied
// by parseDefinition, this method consumes the `rule` keyword onward, and the
// body must be empty because every part of a rule is a leading annotation.

// ruleWhenKeys is the CLOSED @when key set, in the order an error message lists
// them.
//
// It is closed because the whole point of a level is that a call site never
// names a model. An open key set would let a rule branch on something the
// router does not carry, and the failure mode is the worst kind: a rule that
// loads, is listed, is read as governing something, and silently never matches.
//
// THE PARSER VALIDATES THIS ITSELF, and that is not the redundancy it looks
// like. annotations.KeywordArgs carries the same seven names and is consumed by
// exactly two call sites -- component/memql/sense's complete.go and hover.go --
// so it drives COMPLETION AND HOVER ONLY. The generic parseAttribute accepts
// any key=value pair into Attribute.Args with no key-name check at all, which
// is why @trigger, @handler and @relationship do not reject an unknown key
// today either. Reading KeywordArgs here instead would couple a load-time
// refusal to an editor-affordance table; declaring the set here and mirroring
// it there is the arrangement, in that order.
var ruleWhenKeys = []string{"level", "modality", "prompt", "role", "actorRole", "tag", "touches"}

// onUnavailableValues is the closed set for @onUnavailable.
var onUnavailableValues = []string{"degrade", "park"}

// parseRuleDecl parses `rule NAME { }` with its leading attribute set.
//
// Body grammar:
//
//	rule <name> { }
//
// The body MUST be empty. A rule is a record whose whole content is its
// annotations; a non-empty body is refused rather than walked over, because a
// body somebody wrote expecting it to mean something would otherwise be
// silently dropped (the memql#2395 HOLE 4 lesson, applied on the way in rather
// than years later).
func (p *Parser) parseRuleDecl(attrs []*ast.Attribute) (*ast.RuleDecl, error) {
	if !p.check(TokenIdentifier) || p.current.Literal != "rule" {
		return nil, newParseErrorf(&p.current, "expected 'rule' keyword, got %q", p.current.Literal)
	}
	p.advance()

	// A LEXER-PROMOTED KEYWORD IS A VALID RULE NAME, and this position needs
	// the allowance where `policy NAME` does not: the shipped floor rule is
	// literally named `default`, which the lexer promotes to
	// TokenKeywordDefault for its switch/case role. Without this,
	// `rule default { }` fails with "expected rule name, got \"default\"" --
	// a message that reads as a syntax error in the declaration rather than as
	// a collision with a keyword.
	//
	// The identical allowance already exists one position over, for annotation
	// NAMES (@default, @return, @case), and the argument is the same: a name
	// sits immediately after the `rule` keyword, a position no control-flow
	// keyword can occupy, so there is nothing to disambiguate.
	if !p.check(TokenIdentifier) && !isKeywordTokenForAttribute(p.current.Type) {
		return nil, newParseErrorf(&p.current, "expected rule name after 'rule', got %q", p.current.Literal)
	}
	decl := &ast.RuleDecl{Name: p.current.Literal, Attributes: attrs}
	p.advance()

	// Reject an annotation the Rule receiver does not carry BEFORE reading the
	// ones it does, so a typo is reported as a typo rather than as a missing
	// @policy.
	if err := p.checkAnnotations(annotations.Rule, fmt.Sprintf("rule %q", decl.Name), attrs); err != nil {
		return nil, err
	}

	sawPolicy := false
	for _, attr := range attrs {
		if attr == nil {
			continue
		}
		switch attr.Name {
		case "description":
			decl.Description = attrStringValue(attr)
		case "enabled":
			decl.Enabled = true
		case "disabled":
			decl.Disabled = true
		case "locked":
			// A BARE FLAG THE PARSER ACCEPTS UNCONDITIONALLY. @locked is legal
			// only in the embedded tree, and the parser does not know which
			// tree it is reading -- a slice handed to ParseRuleDecl carries no
			// origin. The loader has raw.Path and refuses it there.
			decl.Locked = true
		case "when":
			when, err := p.parseRuleWhen(decl.Name, attr)
			if err != nil {
				return nil, err
			}
			decl.When = when
		case "policy":
			sawPolicy = true
			value := strings.TrimSpace(attrStringValue(attr))
			if value == "" {
				return nil, newParseErrorf(&p.current,
					"rule %q: @policy expects a policy name, e.g. @policy(\"localFirst\")", decl.Name)
			}
			// A policy NAME, so the bare-provider form of the entry grammar:
			// `@policy("policy:localFirst")` would be a policy named "policy",
			// which is a mistake worth catching at its source.
			if err := ValidatePolicyEntry(value); err != nil {
				return nil, newParseErrorf(&p.current, "rule %q: @policy(%q) is not a policy name -- %v", decl.Name, value, err)
			}
			if strings.Contains(value, ":") {
				return nil, newParseErrorf(&p.current,
					"rule %q: @policy(%q) names a selector, not a policy -- @policy takes the bare NAME of a policy, "+
						"and the selectors belong inside that policy's own @primary / @fallback chain", decl.Name, value)
			}
			decl.Policy = value
		case "level":
			value := strings.TrimSpace(attrStringValue(attr))
			if _, err := airoute.ParseLevel(value); err != nil {
				return nil, newParseErrorf(&p.current, "rule %q: %v", decl.Name, err)
			}
			decl.Level = value
		case "precedence":
			n, err := p.ruleIntAttr(decl.Name, attr)
			if err != nil {
				return nil, err
			}
			decl.Precedence = n
		case "onUnavailable":
			value := strings.TrimSpace(attrStringValue(attr))
			if !isOnUnavailableValue(value) {
				return nil, newParseErrorf(&p.current,
					"rule %q: @onUnavailable(%q) is not one of %s -- \"degrade\" walks down to the next level "+
						"and records that it did, \"park\" returns the refusal with the door report",
					decl.Name, value, strings.Join(onUnavailableValues, ", "))
			}
			decl.OnUnavailable = value
		case "exclude":
			// Repeatable, so every occurrence accumulates in declaration order.
			for _, value := range attrStringValues(attr) {
				value = strings.TrimSpace(value)
				if value == "" {
					return nil, newParseErrorf(&p.current,
						"rule %q: @exclude expects an entry to remove, e.g. @exclude(\"fleet:qwen3.5:7b\")", decl.Name)
				}
				if err := ValidatePolicyEntry(value); err != nil {
					return nil, newParseErrorf(&p.current, "rule %q: @exclude(%q) -- %v", decl.Name, value, err)
				}
				decl.Excludes = append(decl.Excludes, value)
			}
		}
	}

	// @policy is REQUIRED. A rule that names no policy decides nothing: it
	// would match a call, win over every rule below it, and hand the router an
	// empty chain -- which reads on the decision record as "no provider could
	// serve this" rather than as the authoring mistake it is.
	if !sawPolicy {
		return nil, newParseErrorf(&p.current,
			"rule %q: @policy is required -- a rule maps a call to a policy, and one that names none "+
				"would match, win, and resolve nothing", decl.Name)
	}

	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}
	if p.check(TokenEOF) {
		return nil, newParseErrorf(&p.current, "rule %q: unexpected EOF before closing '}'", decl.Name)
	}
	if !p.check(TokenBraceClose) {
		return nil, newParseErrorf(&p.current,
			"rule %q: non-empty body -- a rule is an empty-bodied record; its whole content lives in the "+
				"leading annotations (@when / @policy / @level / @precedence / @onUnavailable / @exclude / @locked)",
			decl.Name)
	}
	p.advance()
	return decl, nil
}

// parseRuleWhen reads @when's keyword arguments into the typed condition set,
// refusing a key outside the closed seven.
//
// @when() with NO arguments is legal and states no condition, which matches
// every call. That is what makes the shipped `default` rule the floor: a call
// matching no rule is impossible by construction rather than by care, so
// nothing downstream has to handle the case.
//
// A REPEATED key -- @when(level="fast", level="strong") -- is refused, but not
// here: the generic parseAttribute already refuses a duplicate argument name on
// every annotation (memql#2968), naming the repeated key. It has to be refused
// there, because Attribute.Args is a map: by the time a per-annotation
// validator sees it, the two spellings are indistinguishable and only the last
// survives. The reason is the standing one -- a reader scanning left to right
// sees the first value and the engine uses the last.
func (p *Parser) parseRuleWhen(ruleName string, attr *ast.Attribute) (ast.RuleWhen, error) {
	when := ast.RuleWhen{Present: map[string]bool{}}
	if attr == nil {
		return when, nil
	}

	// @when("something") -- a positional value rather than keyword arguments.
	// Caught by name, because the shape is a plausible guess at the syntax and
	// falling through would leave a rule with no conditions at all, matching
	// every call at whatever precedence its author gave it.
	if attr.Value != nil {
		return when, newParseErrorf(&p.current,
			"rule %q: @when takes keyword arguments, not a bare value -- write @when(level=\"fast\") with keys from %s",
			ruleName, strings.Join(ruleWhenKeys, ", "))
	}

	keys := make([]string, 0, len(attr.Args))
	for key := range attr.Args {
		keys = append(keys, key)
	}
	// Sorted so an unknown key is reported deterministically when an author
	// writes two of them.
	sort.Strings(keys)

	for _, key := range keys {
		if !isRuleWhenKey(key) {
			return when, newParseErrorf(&p.current,
				"rule %q: @when(%s=...) is not a condition -- the keys are %s. A rule branches on what a call "+
					"DECLARES, and a call never names a model",
				ruleName, key, strings.Join(ruleWhenKeys, ", "))
		}
		value, ok := attr.Args[key].(string)
		if !ok {
			return when, newParseErrorf(&p.current,
				"rule %q: @when(%s=...) expects a string value", ruleName, key)
		}
		when.Present[key] = true
		switch key {
		case "level":
			// A @when level is a MATCH against what the call declared, so it is
			// held to the same closed four the call is. A rule matching
			// level="smart" would never fire and would say nothing about why.
			if _, err := airoute.ParseLevel(value); err != nil {
				return when, newParseErrorf(&p.current, "rule %q: @when(level=...): %v", ruleName, err)
			}
			when.Level = value
		case "modality":
			when.Modality = value
		case "prompt":
			when.Prompt = value
		case "role":
			when.Role = value
		case "actorRole":
			when.ActorRole = value
		case "tag":
			when.Tag = value
		case "touches":
			when.Touches = value
		}
	}
	return when, nil
}

// ruleIntAttr reads @precedence's integer, refusing anything that is not one.
//
// attrIntValue answers "did the attribute carry a number", and SATURATES a
// value out of int range -- the right answer for a latency budget, where a
// wrapped negative would read as "every provider is too slow". Precedence is
// not a budget: a fractional or non-numeric value is an authoring mistake, and
// rounding it would pick an evaluation order the author did not write.
func (p *Parser) ruleIntAttr(ruleName string, attr *ast.Attribute) (int, error) {
	if attr == nil || attr.Value == nil {
		return 0, newParseErrorf(&p.current,
			"rule %q: @precedence expects an integer, e.g. @precedence(60)", ruleName)
	}
	if f, isFloat := attr.Value.(float64); isFloat && f != float64(int64(f)) {
		return 0, newParseErrorf(&p.current,
			"rule %q: @precedence(%v) is not a whole number -- rules evaluate in integer order", ruleName, f)
	}
	n, ok := attrIntValue(attr)
	if !ok {
		return 0, newParseErrorf(&p.current,
			"rule %q: @precedence(%v) is not an integer -- rules evaluate highest first, in integer order",
			ruleName, attr.Value)
	}
	return n, nil
}

// attrStringValues returns every string an annotation carried, so a repeatable
// annotation reads the same whether it was written once per value
// (@exclude("a") @exclude("b")) or as a list (@exclude("a", "b")). The
// multi-value form lands in Attribute.Value as a []string; the single form as a
// string.
func attrStringValues(attr *ast.Attribute) []string {
	if attr == nil || attr.Value == nil {
		return nil
	}
	switch v := attr.Value.(type) {
	case string:
		return []string{v}
	case []string:
		return append([]string(nil), v...)
	}
	return nil
}

func isRuleWhenKey(key string) bool {
	for _, known := range ruleWhenKeys {
		if known == key {
			return true
		}
	}
	return false
}

func isOnUnavailableValue(value string) bool {
	for _, known := range onUnavailableValues {
		if known == value {
			return true
		}
	}
	return false
}

// RuleWhenKeys returns the closed @when key set, for a caller that needs to
// state it -- an error message, an editor surface, a loader's own check.
func RuleWhenKeys() []string { return append([]string(nil), ruleWhenKeys...) }

// OnUnavailableValues returns the closed @onUnavailable set.
func OnUnavailableValues() []string { return append([]string(nil), onUnavailableValues...) }
