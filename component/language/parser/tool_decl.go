package parser

import (
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
)

// parseToolDecl parses a struct-form `tool NAME { ... }` declaration.
// The leading attribute set is supplied by parseDefinition; this
// method consumes the `tool` keyword onward and walks the body's
// typed-field list.
//
// Body grammar (one tool per file):
//
//	tool <name> {
//	  <fieldName> <type> @required @description("...") @default("...") @enum("a", "b") @autoInjected
//	  ...
//	}
//
// The leading attributes that carry a typed payload
// (@handler / @rateLimit / @description / @executionTime) are
// surfaced as named fields on ast.ToolDecl so the converter doesn't
// reparse them. Flag annotations (@enabled / @disabled /
// @destructive / @requiresConfirmation) collapse to bool fields the
// same way.
func (p *Parser) parseToolDecl(attrs []*ast.Attribute) (*ast.ToolDecl, error) {
	if !p.check(TokenIdentifier) || p.current.Literal != "tool" {
		return nil, newParseErrorf(&p.current, "expected 'tool' keyword, got %q", p.current.Literal)
	}
	p.advance()

	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected tool name after 'tool', got %q", p.current.Literal)
	}
	decl := &ast.ToolDecl{Name: p.current.Literal}
	p.advance()

	// Translate the leading attribute set into typed ToolDecl fields.
	// Unknown annotations are hard-rejected (#990) -- every other
	// construct kind already does this; closing the silent-tolerance
	// gap keeps typos and stale annotations from being dropped at load.
	for _, attr := range attrs {
		if attr == nil {
			continue
		}
		switch attr.Name {
		case ast.AttrEnabled:
			// Accepted no-op: enabled is the default (lifecycle ruling, #2606).
		case ast.AttrDisabled:
			decl.Disabled = true
		case "description":
			decl.Description = attrStringValue(attr)
		case "destructive":
			decl.Destructive = true
		case "requiresConfirmation":
			decl.RequiresConfirmation = true
		case "executionTime":
			decl.ExecutionTime = attrStringValue(attr)
		case "handler":
			decl.HandlerType = attrArgString(attr, "type")
			if v := attrArgString(attr, "name"); v != "" {
				decl.HandlerName = v
			}
			if v := attrArgString(attr, "query"); v != "" {
				decl.HandlerName = v
			}
			if v := attrArgString(attr, "url"); v != "" {
				decl.HandlerURL = v
			}
			if v := attrArgString(attr, "method"); v != "" {
				decl.HandlerMethod = strings.ToUpper(v)
			}
		case "rateLimit":
			if v := attrArgString(attr, "maxCalls"); v != "" {
				if n, err := strconv.Atoi(v); err == nil {
					decl.RateLimitMaxCalls = n
				}
			}
			if v := attrArgString(attr, "periodSeconds"); v != "" {
				if n, err := strconv.Atoi(v); err == nil {
					decl.RateLimitPeriod = n
				}
			}
		case "clientExecution":
			decl.ClientExecution = true
		case "allowedRoles":
			decl.AllowedRoles = attrStringListValue(attr)
		case "scopes":
			decl.Scopes = attrStringListValue(attr)
		case "mcp":
			decl.MCPExposed = true
		default:
			return nil, newParseErrorf(&p.current, "tool %q: unknown annotation @%s -- supported: @allowedRoles, @clientExecution, @description, @destructive, @disabled, @enabled, @executionTime, @handler, @mcp, @rateLimit, @requiresConfirmation, @scopes", decl.Name, attr.Name)
		}
	}

	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}

	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		field, err := p.parseToolFieldDecl(decl.Name)
		if err != nil {
			return nil, err
		}
		decl.Fields = append(decl.Fields, *field)
	}

	if err := p.expect(TokenBraceClose); err != nil {
		return nil, err
	}
	return decl, nil
}

// parseToolFieldDecl reads one `name type @annotations` line inside
// a tool body. Returns a populated *ast.ToolFieldDecl. Trailing
// annotations stop at the next bare identifier (next field's name)
// or the body's closing brace.
func (p *Parser) parseToolFieldDecl(toolName string) (*ast.ToolFieldDecl, error) {
	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "tool %q: expected field name, got %q", toolName, p.current.Literal)
	}
	field := &ast.ToolFieldDecl{Name: p.current.Literal}
	p.advance()

	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "tool %q field %q: expected type, got %q", toolName, field.Name, p.current.Literal)
	}
	field.Type = p.current.Literal
	p.advance()

	// First-class enum type + required sigil (#2618): the type form
	// lands exactly where @enum's values land (field.EnumValues).
	if field.Type == "enum" && p.check(TokenParenOpen) {
		values, err := p.parseParenStringList("tool " + toolName + " field " + field.Name)
		if err != nil {
			return nil, err
		}
		field.Type = "string"
		field.EnumValues = values
	}
	if p.check(TokenBang) {
		p.advance()
		field.Required = true
	}

	// Trailing annotations.
	for p.check(TokenAt) {
		attr, err := p.parseAttribute()
		if err != nil {
			return nil, err
		}
		if attr == nil {
			continue
		}
		switch attr.Name {
		case "required":
			field.Required = true
		case "autoInjected":
			field.AutoInjected = true
		case "description":
			field.Description = attrStringValue(attr)
		case "default":
			field.Default = attrStringValue(attr)
		case "enum":
			field.EnumValues = attrEnumValues(attr)
		}
	}

	return field, nil
}

// attrArgString fetches a named argument from an annotation's
// argument map, returning "" when the argument is absent or not a
// string. Used for `@handler(type="query", query="...")` /
// `@rateLimit(maxCalls=10, periodSeconds=60)` shapes.
func attrArgString(attr *ast.Attribute, name string) string {
	if attr == nil || len(attr.Args) == 0 {
		return ""
	}
	v, ok := attr.Args[name]
	if !ok || v == nil {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	case int:
		return strconv.Itoa(s)
	case int64:
		return strconv.FormatInt(s, 10)
	case float64:
		// Integer-valued floats render without decimal noise.
		if s == float64(int64(s)) {
			return strconv.FormatInt(int64(s), 10)
		}
		return strconv.FormatFloat(s, 'f', -1, 64)
	}
	return ""
}

// attrStringListValue extracts an ordered string list from a
// leading-attribute argument set. Handles all three storage shapes
// the parser produces for positional string arguments:
//   - []string (multi-value path in parsePositionalAttrs)
//   - []any   (in-body @enum path that goes through parseValue)
//   - string  (single-value form, e.g. @allowedRoles("assistant"))
//
// Returns nil when the annotation has no value.
func attrStringListValue(attr *ast.Attribute) []string {
	if attr == nil || attr.Value == nil {
		return nil
	}
	switch v := attr.Value.(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	}
	return nil
}

// attrEnumValues extracts the ordered string list from an
// `@enum("a", "b", "c")` annotation. Returns nil when the annotation
// is absent or has no values.
//
// The shared parser stores `@enum(a, b)` arguments in attr.Args as a
// map with bare-identifier keys set to true. For tool field enums the
// values are quoted strings stored on attr.Value as a []any.
func attrEnumValues(attr *ast.Attribute) []string {
	if attr == nil {
		return nil
	}
	// parseAttribute stores multiple comma-separated strings as
	// []string -- the missing case here silently dropped every
	// multi-value tool @enum from MCP inputSchemas until the #2618
	// equivalence probe caught the constraint reappearing when the
	// codemod switched those fields to the enum TYPE.
	if vs, ok := attr.Value.([]string); ok {
		return vs
	}
	if vs, ok := attr.Value.([]any); ok {
		out := make([]string, 0, len(vs))
		for _, v := range vs {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	if s, ok := attr.Value.(string); ok && s != "" {
		return []string{s}
	}
	return nil
}
