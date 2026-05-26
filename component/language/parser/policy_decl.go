package parser

import (
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
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
// The body is reserved for future per-vendor tuning knobs; today
// every policy carries an empty body and the parser tolerates
// unknown content inside the braces.
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
	// fields. Unknown attributes are tolerated -- matches the
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

	// Body: an empty `{ }`. Reserved for future use; tolerate
	// arbitrary content inside the braces by walking to the matching
	// close.
	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}
	depth := 1
	for !p.check(TokenEOF) && depth > 0 {
		switch {
		case p.check(TokenBraceOpen):
			depth++
		case p.check(TokenBraceClose):
			depth--
			if depth == 0 {
				p.advance()
				return decl, nil
			}
		}
		p.advance()
	}
	return nil, newParseErrorf(&p.current, "policy %q: unexpected EOF before closing '}'", decl.Name)
}

// attrIntValue pulls a single integer value off an annotation. The
// shared parser stores `@maxLatencyMs(8000)` with attr.Value as a
// float64 (the langparser's literal numeric type); convert to int
// for the PolicyDecl int fields. Returns (0, false) when the value
// isn't a number.
func attrIntValue(attr *ast.Attribute) (int, bool) {
	if attr == nil || attr.Value == nil {
		return 0, false
	}
	switch v := attr.Value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
