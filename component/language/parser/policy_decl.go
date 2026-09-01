package parser

import (
	"strconv"
	"strings"

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
// @description, @primary, @fallback (repeatable), @maxLatencyMs,
// @maxTimeToFirstTokenMs, @preferredRole (repeatable). Repeatable
// attributes accumulate (order preserved); the converter in the
// memql package validates @primary is present.
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
	// fields. Unknown attributes are rejected against the canonical
	// registry (validateDeclAnnotations below) -- matches the
	// hand-rolled parser's drain-and-skip default branch.
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
		case "maxLatencyMs":
			if n, ok := attrIntValue(attr); ok {
				decl.MaxLatencyMs = n
			}
		case "maxTimeToFirstTokenMs":
			if n, ok := attrIntValue(attr); ok {
				decl.MaxTimeToFirstTokenMs = n
			}
		case "preferredRole":
			if v := strings.TrimSpace(attrStringValue(attr)); v != "" {
				decl.PreferredRoles = append(decl.PreferredRoles, v)
			}
		}
	}

	if err := p.validateDeclAnnotations("Policy", "policy", decl.Name, attrs); err != nil {
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
			"policy %q: non-empty body -- a policy is an empty-bodied provider-selection record; configuration lives in the leading annotations (@primary / @fallback / @maxLatencyMs / @maxTimeToFirstTokenMs / @preferredRole)",
			decl.Name)
	}
	p.advance()
	return decl, nil
}

// attrIntValue pulls a single integer value off an annotation. The
// shared parser stores `@maxLatencyMs(8000)` with attr.Value as a
// float64 (the langparser's literal numeric type); convert to int
// for the PolicyDecl int fields. Returns (0, false) when the value
// isn't a number.
// attrIntValue reads an integer annotation argument.
//
// SATURATES out of range (memql#4779). These are latency BUDGETS
// (@maxLatencyMs, @maxTimeToFirstTokenMs), and a budget that wrapped negative
// would read as "every provider is too slow" -- a policy that routes nowhere,
// from a number a person typed into a .memql file.
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
