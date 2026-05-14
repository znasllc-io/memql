package memql

import (
	"fmt"
	"strings"
)

// parseBuiltinMemQL parses a .memql builtin function definition and
// returns a Function. Builtins use the canonical struct form —
// mirrors concept / shape / tool / provider syntax. No receiver
// function wrapping; the body is a field list that describes the
// builtin's input schema. The actual implementation is the Go
// executor identified by @executor("integration.X.Y").
//
//	@enabled
//	@description("Returns full details for a specific function by name.")
//	@executor("help")
//	@alias("helpFunc")
//	@args(profile="stringOrObject", stringKey="name")
//	builtin help {
//	  name  string  @required
//	}
//
// An empty body (or body with only comments) means profile="none"
// with no args. The legacy `func (Builtin) name { ... }` form is
// retired; the parser rejects it with a migration hint.
func parseBuiltinMemQL(origin string, raw []byte) (*Function, error) {
	p := &builtinMemQLParser{}
	p.Init(string(raw), origin)
	return p.parse(origin)
}

// builtinMemQLParser embeds toolMemQLParser for the shared
// tool/builtin-specific helper (parseParenArgs); toolMemQLParser in
// turn embeds baseparser.Base, so the scanning primitives are reached
// through the embedding chain.
type builtinMemQLParser struct {
	toolMemQLParser
}

// builtinDecl represents the parsed builtin declaration.
type builtinDecl struct {
	name        string
	description string
	executor    string
	aliases     []string

	// Args contract
	argProfile              string
	argStringKey            string
	argAdditionalProperties *bool

	// Fields from body (maps to properties + required)
	fields []builtinField
}

type builtinField struct {
	name     string
	typeName string
	required bool
}

func (p *builtinMemQLParser) parse(origin string) (*Function, error) {
	decl := &builtinDecl{}

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
			p.Advance()
			if err := p.parseBuiltinDecorator(decl); err != nil {
				return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, err)
			}
			continue
		}

		// builtin keyword — canonical struct form, mirrors concept /
		// shape / tool / provider. Body shape (field declarations
		// with types + @annotations) is identical to the legacy
		// receiver-function form; we just drop the `func (Builtin)`
		// wrapper.
		if p.MatchWord("builtin") {
			p.SkipWhitespaceAndComments()
			decl.name = p.ReadWord()
			if decl.name == "" {
				return nil, fmt.Errorf("%s:%d:%d: expected builtin name after 'builtin'", origin, p.Line, p.Col)
			}
			if err := p.parseBuiltinFuncBody(decl); err != nil {
				return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, err)
			}
			break
		}

		// `func (Builtin)` is retired. Hard-fail with a migration
		// hint so any stale file fails loud.
		if p.MatchWord("func") {
			return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, fmt.Errorf("`func (Builtin) name { ... }` is retired -- use the canonical struct form: `builtin name { field type @required @description(\"...\") }`"))
		}

		p.Advance()
	}

	if decl.name == "" {
		return nil, fmt.Errorf("%s: no builtin definition found", origin)
	}
	if decl.executor == "" {
		return nil, fmt.Errorf("%s: @executor is required for builtin functions", origin)
	}

	return decl.toFunction(origin)
}

func (p *builtinMemQLParser) parseBuiltinDecorator(decl *builtinDecl) error {
	name := p.ReadWord()
	if name == "" {
		return fmt.Errorf("expected decorator name after @")
	}

	switch name {
	case "enabled", "disabled":
		return nil

	case "description":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.description = val
		return nil

	case "executor":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.executor = val
		return nil

	case "alias":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.aliases = append(decl.aliases, val)
		return nil

	case "args":
		args, err := p.parseParenArgs()
		if err != nil {
			return err
		}
		if v, ok := args["profile"]; ok {
			decl.argProfile = v
		}
		if v, ok := args["stringKey"]; ok {
			decl.argStringKey = v
		}
		if v, ok := args["additionalProperties"]; ok {
			b := v == "true"
			decl.argAdditionalProperties = &b
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

func (p *builtinMemQLParser) parseBuiltinFuncBody(decl *builtinDecl) error {
	p.SkipWhitespaceAndComments()

	if p.EOF() || p.Peek() != '{' {
		return fmt.Errorf("expected '{' to start builtin body")
	}
	p.Advance()

	for !p.EOF() {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			return fmt.Errorf("unexpected end of file in builtin body")
		}

		if p.Peek() == '}' {
			p.Advance()
			return nil
		}

		field, err := p.parseBuiltinField()
		if err != nil {
			return err
		}
		if field != nil {
			decl.fields = append(decl.fields, *field)
		}
	}

	return fmt.Errorf("unexpected end of file, missing '}'")
}

// readBuiltinFieldType reads a field type annotation, tolerating an
// optional `[]` prefix for array-of-primitive types like `[]string`,
// `[]int`, `[]number`. readWord alone only accepts alphanumerics, so
// we handle the `[]` prefix + trailing word here.
func (p *builtinMemQLParser) readBuiltinFieldType() string {
	if p.Peek() == '[' && p.HasNext() && p.PeekAt(1) == ']' {
		p.Advance()
		p.Advance()
		word := p.ReadWord()
		if word == "" {
			return ""
		}
		return "[]" + word
	}
	return p.ReadWord()
}

func (p *builtinMemQLParser) parseBuiltinField() (*builtinField, error) {
	name := p.ReadWord()
	if name == "" {
		return nil, nil
	}

	p.SkipWhitespaceInline()
	typeName := p.readBuiltinFieldType()
	if typeName == "" {
		return nil, fmt.Errorf("expected type for field %q", name)
	}

	field := &builtinField{
		name:     name,
		typeName: typeName,
	}

	// Parse field annotations
	for !p.EOF() {
		p.SkipWhitespaceInline()
		if p.Peek() == '\n' || p.Peek() == '\r' || p.Peek() == '}' {
			break
		}

		if p.Peek() == '@' {
			p.Advance()
			ann := p.ReadWord()
			switch ann {
			case "required":
				field.required = true
			default:
				if !p.EOF() && p.Peek() == '(' {
					p.SkipBalancedParens()
				}
			}
			continue
		}

		if p.Peek() == '/' && p.HasNext() && p.PeekAt(1) == '/' {
			p.SkipToEndOfLine()
			continue
		}

		p.Advance()
	}

	return field, nil
}

func (d *builtinDecl) toFunction(origin string) (*Function, error) {
	// Determine profile
	profile := BuiltinArgProfile(strings.TrimSpace(d.argProfile))
	if profile == "" {
		// Infer from fields
		if len(d.fields) == 0 {
			profile = BuiltinArgProfileNone
		} else {
			profile = BuiltinArgProfileObject
		}
	}

	// Validate profile
	switch profile {
	case BuiltinArgProfileNone, BuiltinArgProfileObject, BuiltinArgProfileOptionalObject,
		BuiltinArgProfileStringOrObject, BuiltinArgProfileOptionalString, BuiltinArgProfileOptionalStringOrObject:
	default:
		return nil, fmt.Errorf("unsupported args profile %q", profile)
	}

	// Build args contract
	contract := &BuiltinArgContract{
		Profile:   profile,
		StringKey: d.argStringKey,
	}

	if d.argAdditionalProperties != nil {
		contract.AdditionalProperties = d.argAdditionalProperties
	} else {
		// Default to false (matching all current builtins)
		f := false
		contract.AdditionalProperties = &f
	}

	if len(d.fields) > 0 {
		contract.Properties = make(map[string]string, len(d.fields))
		for _, f := range d.fields {
			contract.Properties[f.name] = f.typeName
			if f.required {
				contract.Required = append(contract.Required, f.name)
			}
		}
	}

	// Validate stringKey requirement
	switch profile {
	case BuiltinArgProfileStringOrObject, BuiltinArgProfileOptionalString, BuiltinArgProfileOptionalStringOrObject:
		if contract.StringKey == "" {
			return nil, fmt.Errorf("args profile %q requires stringKey", profile)
		}
	}

	fn := &Function{
		Name:           d.name,
		Description:    d.description,
		Type:           FunctionTypeBuiltin,
		Executor:       d.executor,
		BuiltinAliases: d.aliases,
		BuiltinArgs:    contract,
		Origin:         origin,
	}

	return fn, nil
}
