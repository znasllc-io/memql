package parser

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/language/annotations"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/core/num"
)

// parsePolicyDecl parses a struct-form `policy NAME { ... }`
// declaration. The leading attribute set is supplied by
// parseDefinition; this method consumes the `policy` keyword
// onward and walks the (currently-empty) body.
//
// Body grammar:
//
//	policy <name> { }
//
// The body MUST be empty: a policy is an empty-bodied
// provider-selection record; all configuration lives in the leading
// annotations. Non-empty bodies are rejected (memql#2395 HOLE 4 --
// they were previously walked over and silently ignored).
//
// All semantic configuration lives in the leading attribute set:
// @description, @primary, @fallback (repeatable). Repeatable
// attributes accumulate (order preserved); the converter in the
// memql package validates @primary is present, and the loader holds
// every entry to ValidatePolicyEntry's closed grammar.
//
// @maxLatencyMs, @maxTimeToFirstTokenMs and @preferredRole were
// removed with epic memql#5127. All three parsed, stored and steered
// NOTHING -- no selection path read any of them -- and an annotation
// that reads as configuration while doing nothing is worse than its
// absence: an author who writes one believes they have told the
// router something. They are refused now, by name, because the
// receiver no longer carries them.
func (p *Parser) parsePolicyDecl(attrs []*ast.Attribute) (*ast.PolicyDecl, error) {
	if !p.check(TokenIdentifier) || p.current.Literal != "policy" {
		return nil, newParseErrorf(&p.current, "expected 'policy' keyword, got %q", p.current.Literal)
	}
	p.advance()

	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected policy name after 'policy', got %q", p.current.Literal)
	}
	decl := &ast.PolicyDecl{Name: p.current.Literal}
	p.advance()

	// Translate the leading attribute set into typed PolicyDecl
	// fields. Unknown attributes are refused by the annotation registry
	// (checkAnnotations below, memql#5359).
	for _, attr := range attrs {
		if attr == nil {
			continue
		}
		switch attr.Name {
		case "description":
			decl.Description = attrStringValue(attr)
		case "primary":
			decl.Primary = strings.TrimSpace(attrStringValue(attr))
		case "fallback":
			if v := strings.TrimSpace(attrStringValue(attr)); v != "" {
				decl.Fallbacks = append(decl.Fallbacks, v)
			}
		}
	}

	if err := p.checkAnnotations(annotations.Policy, fmt.Sprintf("policy %q", decl.Name), attrs); err != nil {
		return nil, err
	}

	// Body: MUST be an empty `{ }`. A policy is an empty-bodied
	// provider-selection record; configuration lives in the leading
	// annotations. Content inside the braces was previously walked over
	// and silently ignored (memql#2395 HOLE 4) -- now rejected.
	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}
	if p.check(TokenEOF) {
		return nil, newParseErrorf(&p.current, "policy %q: unexpected EOF before closing '}'", decl.Name)
	}
	if !p.check(TokenBraceClose) {
		return nil, newParseErrorf(&p.current,
			"policy %q: non-empty body -- a policy is an empty-bodied provider-selection record; configuration lives in the leading annotations (@description / @primary / @fallback)",
			decl.Name)
	}
	p.advance()
	return decl, nil
}

// attrIntValue reads an integer annotation argument. The shared parser stores
// `@precedence(60)` with attr.Value as an int64 or float64 (the langparser's
// literal numeric types); convert to int. Returns (0, false) when the value
// isn't a number.
//
// SATURATES out of range (memql#4779), rather than wrapping negative on a
// number a person typed into a .memql file. Its caller, ruleIntAttr, refuses a
// fractional value before reaching here, because a rule's evaluation order is
// not a budget and rounding it would pick an order the author did not write.
//
// The same package already answers this question in `numericAsInt`
// (parser.go), which reports ok=false instead. The two are not being unified
// here: this one's `bool` means "the attribute carried a number", and folding
// the range question into it would make a 2^63 budget indistinguishable from
// an absent annotation.
func attrIntValue(attr *ast.Attribute) (int, bool) {
	if attr == nil || attr.Value == nil {
		return 0, false
	}
	switch v := attr.Value.(type) {
	case int:
		return v, true
	case int64:
		return num.ClampInt64(v), true
	case float64:
		return num.ClampFloat64(v), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
