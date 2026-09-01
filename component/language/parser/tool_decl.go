package parser

import (
	"sort"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/core/num"
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
			// A mistyped kwarg used to be DROPPED, not refused (memql#3625).
			// The two shapes that produced were the memql#3605 archetype
			// exactly: `@handler(type="function", nmae="createTodo")` left the
			// function name empty, and `@handler(tipe="function",
			// name="createTodo")` dropped the ENTIRE handler -- decl.HandlerType
			// stayed "", so toolDeclToTool never built a ToolHandler and the
			// tool registered with no way to execute. Both then reached the LLM
			// as an advertised, callable tool.
			if err := rejectUnknownAttrArgs(&p.current, decl.Name, "handler", attr, toolHandlerArgNames); err != nil {
				return nil, err
			}
			decl.HandlerType = attrArgString(attr, "type")
			if decl.HandlerType == "" {
				return nil, newParseErrorf(&p.current, "tool %q: @handler requires a non-empty type=... argument (\"function\", \"query\", \"webhook\" or \"delegate\") -- a @handler whose type does not resolve is dropped whole, and the tool then registers with no way to execute", decl.Name)
			}
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
			if err := rejectUnknownAttrArgs(&p.current, decl.Name, "rateLimit", attr, toolRateLimitArgNames); err != nil {
				return nil, err
			}
			// A non-integer value was silently discarded here too, which is the
			// same defect one annotation over: the author declared a ceiling and
			// got none (memql#3625).
			if v := attrArgString(attr, "maxCalls"); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil {
					return nil, newParseErrorf(&p.current, "tool %q: @rateLimit(maxCalls=%q) is not an integer -- a non-integer was discarded, leaving the tool with no rate limit at all", decl.Name, v)
				}
				decl.RateLimitMaxCalls = n
			}
			if v := attrArgString(attr, "periodSeconds"); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil {
					return nil, newParseErrorf(&p.current, "tool %q: @rateLimit(periodSeconds=%q) is not an integer -- a non-integer was discarded, leaving the tool with no rate limit period at all", decl.Name, v)
				}
				decl.RateLimitPeriod = n
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
		at := p.current
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
		default:
			// Anything else used to fall off the end of this switch and be
			// discarded (memql#3625). `@enums("a", "b")` was the measured case:
			// pure declaration theatre -- the constraint the author wrote never
			// reached the JSON schema, so the LLM was told the field was an
			// unconstrained string and nothing complained. Concept property
			// annotations are already closed this way; tool fields now are too.
			return nil, newParseErrorf(&at, "tool %q field %q: unknown annotation @%s (supported: @required, @autoInjected, @description, @default, @enum) -- an unknown annotation was silently discarded, so the constraint you wrote was never enforced", toolName, field.Name, attr.Name)
		}
	}

	return field, nil
}

// toolHandlerArgNames is the closed set of `@handler(...)` keyword arguments.
// `type` picks the dispatch arm; the rest carry that arm's target.
var toolHandlerArgNames = map[string]bool{
	"type": true, "name": true, "query": true, "url": true, "method": true,
}

// toolRateLimitArgNames is the closed set of `@rateLimit(...)` keyword
// arguments.
var toolRateLimitArgNames = map[string]bool{
	"maxCalls": true, "periodSeconds": true,
}

// rejectUnknownAttrArgs refuses any keyword argument on a typed-payload
// annotation that is not in the allowed set, naming the offender and the
// supported spellings.
//
// The annotations this guards read their arguments by NAME out of
// attr.Args, so an unrecognised key is not an error today -- it is simply
// never read. That makes a single-letter typo indistinguishable from
// omitting the argument, which is how a tool with `nmae=` shipped an empty
// function name and a tool with `tipe=` shipped no handler at all
// (memql#3625, the memql#3605 archetype).
func rejectUnknownAttrArgs(tok *Token, toolName, annotation string, attr *ast.Attribute, allowed map[string]bool) error {
	if attr == nil || len(attr.Args) == 0 {
		return nil
	}
	unknown := make([]string, 0, len(attr.Args))
	for k := range attr.Args {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	supported := make([]string, 0, len(allowed))
	for k := range allowed {
		supported = append(supported, k)
	}
	sort.Strings(supported)
	return newParseErrorf(tok, "tool %q: unknown @%s argument(s) %s (supported: %s) -- an unrecognised argument name is never read, so the value you wrote was dropped",
		toolName, annotation, strings.Join(unknown, ", "), strings.Join(supported, ", "))
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
		//
		// narrowing: GUARDED -- num.WholeInt64 IS the guard, replacing
		// `s == float64(int64(s))`, whose result is undefined for an s outside
		// int64 (memql#4779). A whole float too large for an int64 renders
		// through the float branch, which is the honest answer for it.
		if whole, ok := num.WholeInt64(s); ok {
			return strconv.FormatInt(whole, 10)
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
