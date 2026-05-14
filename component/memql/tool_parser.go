package memql

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/visionarys-io/memql/component/memql/baseparser"
)

// parseToolMemQL parses a .memql tool definition file and returns Tool definitions.
// Tool .memql files use struct-style declarations that mirror
// concept / shape syntax — no receiver function wrapping, no
// arguments, no return:
//
//	use common.agent
//
//	@enabled
//	@description("Search for users across the organization.")
//	@handler(type="function", name="searchUsers")
//	@rateLimit(maxCalls=10, periodSeconds=60)
//	tool searchUsers {
//	  query   string  @required @description("Search query for user lookup.")
//	  limit   int     @default(10) @description("Max results to return.")
//	}
//
// The legacy `func (Tool) name { ... }` form is retired and the
// parser rejects it with a migration hint.
func parseToolMemQL(origin string, raw []byte) ([]*Tool, error) {
	p := &toolMemQLParser{}
	p.Init(string(raw), origin)
	return p.parse(origin)
}

// toolMemQLParser embeds baseparser.Base for the shared scanning
// primitives. builtin_parser further embeds toolMemQLParser to ride
// the same primitives + the parseField + parseParenArgs helpers tools
// and builtins share.
type toolMemQLParser struct {
	baseparser.Base
}

// toolField represents a parsed field declaration in a tool body.
type toolField struct {
	name        string
	typeName    string
	elementType string // element type for `[]T` slice fields; "" for non-slice
	required    bool
	description string
	enumValues  []string
	defaultVal  string
}

// toolDecl represents the parsed top-level tool declaration.
type toolDecl struct {
	name        string
	description string

	// Handler
	handlerType   string
	handlerName   string // function name or query expression
	handlerURL    string // webhook URL
	handlerMethod string // webhook HTTP method (default POST)

	// Annotations
	destructive          bool
	requiresConfirmation bool
	executionTime        string
	rateLimitMaxCalls    int
	rateLimitPeriod      int

	// Fields (inputSchema)
	fields []toolField
}

func (p *toolMemQLParser) parse(origin string) ([]*Tool, error) {
	decl := &toolDecl{}

	// Parse top-level: use statements, decorators, func (Tool) header, body
	for !p.EOF() {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			break
		}

		ch := p.Peek()

		// use statement - skip
		if p.MatchWord("use") {
			p.SkipToEndOfLine()
			continue
		}

		// @ decorator
		if ch == '@' {
			p.Advance() // consume @
			if err := p.parseDecorator(decl); err != nil {
				return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, err)
			}
			continue
		}

		// tool keyword (canonical struct form, mirrors `concept` /
		// `shape`). The body is a field list — same shape the
		// receiver-function form already used — so we reuse
		// parseFuncBody after reading the bare tool name.
		if p.MatchWord("tool") {
			if err := p.parseToolStructHeader(decl); err != nil {
				return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, err)
			}
			if err := p.parseFuncBody(decl); err != nil {
				return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, err)
			}
			break // only one tool per file
		}

		// `func (Tool)` is retired. Hard-fail with a migration hint
		// so any stale file or out-of-tree copy fails loud.
		if p.MatchWord("func") {
			return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, fmt.Errorf("`func (Tool) name { ... }` is retired -- use the canonical struct form: `tool name { field type @required @description(\"...\") }`"))
		}

		// Skip anything else (comments already handled)
		p.Advance()
	}

	if decl.name == "" {
		return nil, fmt.Errorf("%s: no tool definition found", origin)
	}

	tool, err := decl.toTool(origin)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", origin, err)
	}

	return []*Tool{tool}, nil
}

func (p *toolMemQLParser) parseDecorator(decl *toolDecl) error {
	name := p.ReadWord()
	if name == "" {
		return fmt.Errorf("expected decorator name after @")
	}

	switch name {
	case "enabled":
		// no-op, tools are enabled by default
		return nil

	case "disabled":
		// no-op for now
		return nil

	case "description":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.description = val
		return nil

	case "handler":
		args, err := p.parseParenArgs()
		if err != nil {
			return err
		}
		decl.handlerType = args["type"]
		if n, ok := args["name"]; ok {
			decl.handlerName = n
		}
		if q, ok := args["query"]; ok {
			decl.handlerName = q
		}
		if u, ok := args["url"]; ok {
			decl.handlerURL = u
		}
		if m, ok := args["method"]; ok {
			decl.handlerMethod = strings.ToUpper(m)
		}
		return nil

	case "destructive":
		decl.destructive = true
		return nil

	case "requiresConfirmation":
		decl.requiresConfirmation = true
		return nil

	case "executionTime":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.executionTime = val
		return nil

	case "rateLimit":
		args, err := p.parseParenArgs()
		if err != nil {
			return err
		}
		if v, ok := args["maxCalls"]; ok {
			decl.rateLimitMaxCalls, _ = strconv.Atoi(v)
		}
		if v, ok := args["periodSeconds"]; ok {
			decl.rateLimitPeriod, _ = strconv.Atoi(v)
		}
		return nil

	default:
		// Unknown decorator - skip any arguments
		p.SkipWhitespaceInline()
		if !p.EOF() && p.Peek() == '(' {
			p.SkipBalancedParens()
		}
		return nil
	}
}

// parseToolStructHeader reads the canonical struct-form header
// after the `tool` keyword has been consumed:
//
//	tool <name> {
//
// The body parsing is identical to the legacy receiver-function
// form (parseFuncBody) — tools have always had struct-shaped
// bodies (field declarations with types + @annotations). The
// only thing that changed at the surface is dropping the
// `func (Tool)` wrapper.
func (p *toolMemQLParser) parseToolStructHeader(decl *toolDecl) error {
	p.SkipWhitespaceAndComments()
	decl.name = p.ReadWord()
	if decl.name == "" {
		return fmt.Errorf("expected tool name after 'tool'")
	}
	return nil
}

func (p *toolMemQLParser) parseFuncBody(decl *toolDecl) error {
	p.SkipWhitespaceAndComments()

	if p.EOF() || p.Peek() != '{' {
		return fmt.Errorf("expected '{' to start tool body")
	}
	p.Advance() // consume {

	// Parse field declarations until }
	for !p.EOF() {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			return fmt.Errorf("unexpected end of file in tool body")
		}

		if p.Peek() == '}' {
			p.Advance() // consume }
			return nil
		}

		prevPos := p.Pos
		field, err := p.parseField()
		if err != nil {
			return err
		}
		if field != nil {
			decl.fields = append(decl.fields, *field)
		} else if p.Pos == prevPos {
			// Safety: skip unrecognized character to prevent infinite loop.
			p.Advance()
		}
	}

	return fmt.Errorf("unexpected end of file, missing '}'")
}

func (p *toolMemQLParser) parseField() (*toolField, error) {
	name := p.ReadWord()
	if name == "" {
		return nil, nil
	}

	p.SkipWhitespaceInline()
	typeName := p.ReadWord()
	if typeName == "" {
		return nil, fmt.Errorf("expected type for field %q", name)
	}

	field := &toolField{
		name:     name,
		typeName: typeName,
	}

	// Parse field annotations until end of line or next field
	for !p.EOF() {
		p.SkipWhitespaceInline()
		if p.Peek() == '\n' || p.Peek() == '\r' || p.Peek() == '}' {
			break
		}

		if p.Peek() == '@' {
			p.Advance() // consume @
			ann := p.ReadWord()

			switch ann {
			case "required":
				field.required = true
			case "description":
				val, err := p.ParseParenString()
				if err != nil {
					return nil, err
				}
				field.description = val
			case "enum":
				vals, err := p.ParseParenStringList()
				if err != nil {
					return nil, err
				}
				field.enumValues = vals
			case "default":
				val, err := p.ParseParenString()
				if err != nil {
					return nil, err
				}
				field.defaultVal = val
			default:
				// Unknown annotation - skip args
				if !p.EOF() && p.Peek() == '(' {
					p.SkipBalancedParens()
				}
			}
			continue
		}

		// Handle multi-line field annotations (line continuation)
		if p.Peek() == '/' && p.HasNext() && p.PeekAt(1) == '/' {
			p.SkipToEndOfLine()
			continue
		}

		// Skip other characters
		p.Advance()
	}

	return field, nil
}

// toTool converts a parsed tool declaration to a Tool struct.
func (d *toolDecl) toTool(origin string) (*Tool, error) {
	// Build JSON Schema from fields
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
	}

	properties := make(map[string]any)
	var required []string

	for _, f := range d.fields {
		prop := map[string]any{}

		// Map MemQL types to JSON Schema types
		switch strings.ToLower(f.typeName) {
		case "string":
			prop["type"] = "string"
		case "number", "float":
			prop["type"] = "number"
		case "integer", "int":
			prop["type"] = "integer"
		case "bool", "boolean":
			prop["type"] = "boolean"
		case "object":
			prop["type"] = "object"
		case "array":
			prop["type"] = "array"
			// OpenAI requires array schemas to specify "items" with a "type" field.
			// Default to generic object items when no nested schema is defined.
			prop["items"] = map[string]any{"type": "object"}
		default:
			prop["type"] = "string" // default to string
		}

		if f.description != "" {
			prop["description"] = f.description
		}
		if len(f.enumValues) > 0 {
			enumAny := make([]any, len(f.enumValues))
			for i, v := range f.enumValues {
				enumAny[i] = v
			}
			prop["enum"] = enumAny
		}
		if f.defaultVal != "" {
			prop["default"] = f.defaultVal
		}

		properties[f.name] = prop

		if f.required {
			required = append(required, f.name)
		}
	}

	schema["properties"] = properties
	if len(required) > 0 {
		schema["required"] = required
	}

	schemaJSON, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("marshal inputSchema: %w", err)
	}

	tool := &Tool{
		Name:        d.name,
		Description: d.description,
		InputSchema: json.RawMessage(schemaJSON),
		Origin:      origin,
	}

	// Handler
	if d.handlerType != "" {
		handler := &ToolHandler{
			Type: d.handlerType,
		}
		switch d.handlerType {
		case "function":
			handler.FunctionName = d.handlerName
		case "query":
			handler.Query = d.handlerName
		case "webhook":
			handler.URL = d.handlerURL
			if d.handlerMethod != "" {
				handler.Method = d.handlerMethod
			} else {
				handler.Method = "POST"
			}
		}
		tool.Handler = handler
	}

	// Annotations
	if d.destructive || d.requiresConfirmation || d.executionTime != "" || d.rateLimitMaxCalls > 0 {
		ann := &ToolAnnotations{
			Destructive:          d.destructive,
			RequiresConfirmation: d.requiresConfirmation,
			ExecutionTime:        d.executionTime,
		}
		if d.rateLimitMaxCalls > 0 {
			ann.RateLimit = &ToolRateLimit{
				MaxCalls:      d.rateLimitMaxCalls,
				PeriodSeconds: d.rateLimitPeriod,
			}
		}
		tool.Annotations = ann
	}

	return tool, nil
}


// parseParenArgs parses (key="value", key2="value2") or (key=number)
// -- tool/builtin-specific @rateLimit(...) / @args(...) shape.
func (p *toolMemQLParser) parseParenArgs() (map[string]string, error) {
	p.SkipWhitespaceInline()
	if p.EOF() || p.Peek() != '(' {
		return nil, fmt.Errorf("expected '(' for arguments")
	}
	p.Advance()

	args := make(map[string]string)
	for !p.EOF() {
		p.SkipWhitespaceAndComments()
		if p.Peek() == ')' {
			p.Advance()
			return args, nil
		}

		key := p.ReadWord()
		if key == "" {
			return nil, fmt.Errorf("expected argument name")
		}

		p.SkipWhitespaceInline()
		if p.EOF() || p.Peek() != '=' {
			return nil, fmt.Errorf("expected '=' after argument name %q", key)
		}
		p.Advance()
		p.SkipWhitespaceInline()

		var val string
		if !p.EOF() && p.Peek() == '"' {
			var err error
			val, err = p.ReadQuotedString()
			if err != nil {
				return nil, err
			}
		} else {
			val = p.ReadWord()
		}

		args[key] = val

		p.SkipWhitespaceAndComments()
		if !p.EOF() && p.Peek() == ',' {
			p.Advance()
		}
	}
	return nil, fmt.Errorf("unterminated argument list")
}
