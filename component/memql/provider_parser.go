package memql

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/visionarys-io/memql/component/memql/baseparser"
)

// parseProviderMemQL parses a .memql provider definition file and
// returns a ProviderConfig. Providers use the canonical struct form
// — no receiver function wrapping, no arguments, no return — that
// mirrors concept / shape / tool syntax:
//
//	@description("OpenAI GPT-5 Mini for fast chat completions")
//	@type("OpenAI")
//	@model("gpt-5-mini")
//	@modality("text")
//	provider chat5Mini {
//	  auth {
//	    apiKey     env("MEMQL_SI_OPENAI_API_KEY")
//	    projectId  env("MEMQL_SI_OPENAI_PROJECT_ID")
//	  }
//	  params {
//	    maxTokens            4096
//	    maxCompletionTokens  4096
//	    voice                "nova"
//	    format               "wav"
//	    speed                1.0
//	  }
//	}
//
// The legacy `func (Provider) name { ... }` form is retired and
// the parser rejects it with a migration hint.
func parseProviderMemQL(origin string, raw []byte) (*ProviderConfig, error) {
	p := &providerMemQLParser{}
	p.Init(string(raw), origin)
	return p.parse(origin)
}

// providerMemQLParser embeds baseparser.Base for scanning primitives.
// Provider-specific helpers (parseEnvCall, parseAuthBlock,
// parseParamsBlock, readNumber, readProviderName) stay on the wrapper.
type providerMemQLParser struct {
	baseparser.Base
}

// providerDecl represents the parsed top-level provider declaration.
type providerDecl struct {
	name        string
	description string
	typeName    string            // "OpenAI", "Anthropic", "OpenAIStream", etc.
	model       string
	modality    string            // "text", "tts", "stt" (default: "text")
	isDefault   bool
	isBase      bool              // true for @base provider definitions (no func keyword)
	extendsName string            // name of base provider to inherit from via @extends
	auth        map[string]string // key -> "${ENV_VAR}" strings
	params      map[string]any    // key -> value (string, int, float)
}

func (p *providerMemQLParser) parse(origin string) (*ProviderConfig, error) {
	decl := &providerDecl{
		auth:   make(map[string]string),
		params: make(map[string]any),
	}

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

		// provider keyword — canonical struct form. Mirrors the
		// concept / shape / tool syntax. Both regular providers
		// (formerly `func (Provider) name { ... }`) and `@base`
		// providers use this single form now.
		if p.MatchWord("provider") {
			p.SkipWhitespaceAndComments()
			decl.name = p.readProviderName()
			if decl.name == "" {
				return nil, fmt.Errorf("%s:%d:%d: expected provider name", origin, p.Line, p.Col)
			}
			if err := p.parseFuncBody(decl); err != nil {
				return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, err)
			}
			break
		}

		// `func (Provider)` is retired. Hard-fail with a migration
		// hint so any stale file or out-of-tree copy fails loud.
		if p.MatchWord("func") {
			return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, fmt.Errorf("`func (Provider) name { ... }` is retired -- use the canonical struct form: `provider name { params { ... } }`"))
		}

		// Skip anything else
		p.Advance()
	}

	if decl.name == "" {
		return nil, fmt.Errorf("%s: no provider definition found", origin)
	}

	return decl.toProviderConfig()
}

func (p *providerMemQLParser) parseDecorator(decl *providerDecl) error {
	name := p.ReadWord()
	if name == "" {
		return fmt.Errorf("expected decorator name after @")
	}

	switch name {
	case "description":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.description = val
		return nil

	case "type":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.typeName = val
		return nil

	case "model":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.model = val
		return nil

	case "modality":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.modality = val
		return nil

	case "default":
		decl.isDefault = true
		return nil

	case "base":
		decl.isBase = true
		return nil

	case "extends":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.extendsName = val
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

func (p *providerMemQLParser) parseFuncBody(decl *providerDecl) error {
	p.SkipWhitespaceAndComments()

	if p.EOF() || p.Peek() != '{' {
		return fmt.Errorf("expected '{' to start provider body")
	}
	p.Advance() // consume {

	for !p.EOF() {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			return fmt.Errorf("unexpected end of file in provider body")
		}

		if p.Peek() == '}' {
			p.Advance() // consume }
			return nil
		}

		// Read sub-block name
		blockName := p.ReadWord()
		if blockName == "" {
			p.Advance() // skip unrecognized character
			continue
		}

		switch blockName {
		case "auth":
			if err := p.parseAuthBlock(decl); err != nil {
				return err
			}
		case "params":
			if err := p.parseParamsBlock(decl); err != nil {
				return err
			}
		default:
			// Unknown block - skip its body if it has braces
			p.SkipWhitespaceAndComments()
			if !p.EOF() && p.Peek() == '{' {
				p.SkipBalancedBraces()
			}
		}
	}

	return fmt.Errorf("unexpected end of file, missing '}'")
}

func (p *providerMemQLParser) parseAuthBlock(decl *providerDecl) error {
	p.SkipWhitespaceAndComments()
	if p.EOF() || p.Peek() != '{' {
		return fmt.Errorf("expected '{' to start auth block")
	}
	p.Advance() // consume {

	for !p.EOF() {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			return fmt.Errorf("unexpected end of file in auth block")
		}

		if p.Peek() == '}' {
			p.Advance() // consume }
			return nil
		}

		key := p.ReadWord()
		if key == "" {
			p.Advance()
			continue
		}

		p.SkipWhitespaceInline()

		// Value is either env("VAR_NAME") or a quoted string
		if p.MatchWord("env") {
			val, err := p.parseEnvCall()
			if err != nil {
				return fmt.Errorf("auth %q: %w", key, err)
			}
			decl.auth[key] = val
		} else if !p.EOF() && p.Peek() == '"' {
			val, err := p.ReadQuotedString()
			if err != nil {
				return fmt.Errorf("auth %q: %w", key, err)
			}
			decl.auth[key] = val
		} else {
			return fmt.Errorf("expected env() or quoted string for auth key %q", key)
		}
	}

	return fmt.Errorf("unexpected end of file in auth block")
}

// parseEnvCall parses ("VAR_NAME") and returns "${VAR_NAME}".
func (p *providerMemQLParser) parseEnvCall() (string, error) {
	p.SkipWhitespaceInline()
	if p.EOF() || p.Peek() != '(' {
		return "", fmt.Errorf("expected '(' after env")
	}
	p.Advance() // consume (
	p.SkipWhitespaceAndComments()

	val, err := p.ReadQuotedString()
	if err != nil {
		return "", err
	}

	p.SkipWhitespaceAndComments()
	if p.EOF() || p.Peek() != ')' {
		return "", fmt.Errorf("expected ')' after env value")
	}
	p.Advance() // consume )

	return "${" + val + "}", nil
}

func (p *providerMemQLParser) parseParamsBlock(decl *providerDecl) error {
	p.SkipWhitespaceAndComments()
	if p.EOF() || p.Peek() != '{' {
		return fmt.Errorf("expected '{' to start params block")
	}
	p.Advance() // consume {

	for !p.EOF() {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			return fmt.Errorf("unexpected end of file in params block")
		}

		if p.Peek() == '}' {
			p.Advance() // consume }
			return nil
		}

		key := p.ReadWord()
		if key == "" {
			p.Advance()
			continue
		}

		p.SkipWhitespaceInline()

		// Value can be a quoted string, integer, or float
		if !p.EOF() && p.Peek() == '"' {
			val, err := p.ReadQuotedString()
			if err != nil {
				return fmt.Errorf("params %q: %w", key, err)
			}
			decl.params[key] = val
		} else {
			// Read numeric value
			val, err := p.readNumber()
			if err != nil {
				return fmt.Errorf("params %q: %w", key, err)
			}
			decl.params[key] = val
		}
	}

	return fmt.Errorf("unexpected end of file in params block")
}

// readNumber reads an integer or float literal and returns the appropriate Go type.
func (p *providerMemQLParser) readNumber() (any, error) {
	var b strings.Builder
	hasDecimal := false

	// Handle negative sign
	if !p.EOF() && p.Peek() == '-' {
		b.WriteByte('-')
		p.Advance()
	}

	if p.EOF() || (!unicode.IsDigit(rune(p.Peek())) && p.Peek() != '.') {
		return nil, fmt.Errorf("expected number")
	}

	for !p.EOF() {
		ch := p.Peek()
		if unicode.IsDigit(rune(ch)) {
			b.WriteByte(ch)
			p.Advance()
		} else if ch == '.' && !hasDecimal {
			hasDecimal = true
			b.WriteByte(ch)
			p.Advance()
		} else {
			break
		}
	}

	raw := b.String()
	if hasDecimal {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid float %q: %w", raw, err)
		}
		return f, nil
	}

	i, err := strconv.Atoi(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid integer %q: %w", raw, err)
	}
	return i, nil
}

// toProviderConfig converts a parsed provider declaration to a ProviderConfig struct.
func (d *providerDecl) toProviderConfig() (*ProviderConfig, error) {
	// Base providers don't need @model, and @extends providers inherit @type from base.
	if d.typeName == "" && d.extendsName == "" && !d.isBase {
		return nil, fmt.Errorf("provider %q: @type is required", d.name)
	}
	if d.model == "" && !d.isBase {
		return nil, fmt.Errorf("provider %q: @model is required", d.name)
	}

	cfg := &ProviderConfig{
		Name:        d.name,
		Type:        d.typeName,
		Model:       d.model,
		Auth:        d.auth,
		Params:      d.params,
		Default:     d.isDefault,
		Modality:    d.modality,
		Description: d.description,
		Base:        d.isBase,
		Extends:     d.extendsName,
	}

	return cfg, nil
}

// readProviderName reads a provider name that may contain dots and hyphens.
func (p *providerMemQLParser) readProviderName() string {
	var b strings.Builder
	for !p.EOF() {
		ch := p.Peek()
		if unicode.IsLetter(rune(ch)) || unicode.IsDigit(rune(ch)) || ch == '_' || ch == '.' || ch == '-' {
			b.WriteByte(ch)
			p.Advance()
		} else {
			break
		}
	}
	return b.String()
}

