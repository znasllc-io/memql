package seeder

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseSeedMemQL parses a seed.memql file into a list of seed records with an optional default actor.
//
// Syntax:
//
//	@actor("system")
//
//	seed "record-id" {
//	  name        "Sofia"
//	  active      true
//	  count       42
//	  score       3.14
//	  tags        ["a", "b", "c"]
//	  nested {
//	    field     "value"
//	  }
//	}
func ParseSeedMemQL(content []byte) (*parsedSeedFile, error) {
	p := &seedParser{
		input: string(content),
		pos:   0,
		line:  1,
		col:   1,
	}
	return p.parse()
}

type parsedSeedFile struct {
	Actor   string
	Records []parsedSeedRecord
}

type parsedSeedRecord struct {
	ID      string
	Actor   string
	Payload map[string]any
	Match   []seedMatch
}

type seedParser struct {
	input string
	pos   int
	line  int
	col   int
}

func (p *seedParser) parse() (*parsedSeedFile, error) {
	result := &parsedSeedFile{}

	p.skipWhitespaceAndComments()

	for p.pos < len(p.input) {
		p.skipWhitespaceAndComments()
		if p.pos >= len(p.input) {
			break
		}

		// File-level annotation
		if p.peek() == '@' {
			ann, err := p.parseAnnotation()
			if err != nil {
				return nil, err
			}
			switch ann.name {
			case "actor":
				result.Actor = ann.text
			default:
				return nil, p.errorf("unknown file-level annotation @%s", ann.name)
			}
			p.skipWhitespaceAndComments()
			continue
		}

		// Expect "seed" keyword
		word := p.readWord()
		if word != "seed" {
			return nil, p.errorf("expected 'seed', got %q", word)
		}
		p.skipWhitespaceAndComments()

		record, err := p.parseSeedBlock()
		if err != nil {
			return nil, err
		}
		result.Records = append(result.Records, record)

		p.skipWhitespaceAndComments()
	}

	return result, nil
}

func (p *seedParser) parseSeedBlock() (parsedSeedRecord, error) {
	// Read ID (required string)
	if p.peek() != '"' {
		return parsedSeedRecord{}, p.errorf("expected quoted seed ID")
	}
	id, err := p.readString()
	if err != nil {
		return parsedSeedRecord{}, err
	}

	record := parsedSeedRecord{ID: id}
	p.skipWhitespaceAndComments()

	// Optional per-record annotations before the opening brace
	for p.peek() == '@' {
		ann, err := p.parseAnnotation()
		if err != nil {
			return parsedSeedRecord{}, err
		}
		switch ann.name {
		case "actor":
			record.Actor = ann.text
		case "match":
			field := ann.args["field"]
			value := ann.args["value"]
			if field == "" {
				return parsedSeedRecord{}, p.errorf("@match requires field argument")
			}
			record.Match = append(record.Match, seedMatch{Field: field, Value: value})
		default:
			return parsedSeedRecord{}, p.errorf("unknown seed annotation @%s", ann.name)
		}
		p.skipWhitespaceAndComments()
	}

	// Opening brace
	if p.peek() != '{' {
		return parsedSeedRecord{}, p.errorf("expected '{' after seed ID, got %q", string(p.peek()))
	}
	p.advance()
	p.skipWhitespaceAndComments()

	// Parse payload key-value pairs
	payload, err := p.parseObjectBody()
	if err != nil {
		return parsedSeedRecord{}, err
	}
	record.Payload = payload

	return record, nil
}

// parseObjectBody parses key-value pairs until closing '}'.
func (p *seedParser) parseObjectBody() (map[string]any, error) {
	result := make(map[string]any)

	for p.pos < len(p.input) && p.peek() != '}' {
		p.skipWhitespaceAndComments()
		if p.pos >= len(p.input) || p.peek() == '}' {
			break
		}

		key := p.readWord()
		if key == "" {
			return nil, p.errorf("expected property name")
		}
		p.skipInlineWhitespace()

		// Check for nested object block
		if p.peek() == '{' {
			p.advance()
			p.skipWhitespaceAndComments()
			nested, err := p.parseObjectBody()
			if err != nil {
				return nil, fmt.Errorf("nested object %q: %w", key, err)
			}
			result[key] = nested
		} else {
			value, err := p.parseValue()
			if err != nil {
				return nil, fmt.Errorf("property %q: %w", key, err)
			}
			result[key] = value
		}

		p.skipWhitespaceAndComments()
	}

	if p.peek() != '}' {
		return nil, p.errorf("expected '}'")
	}
	p.advance()

	return result, nil
}

// parseValue parses a single value: string, number, bool, null, or array.
func (p *seedParser) parseValue() (any, error) {
	p.skipInlineWhitespace()

	ch := p.peek()

	// String
	if ch == '"' {
		return p.readString()
	}

	// Array
	if ch == '[' {
		return p.parseArray()
	}

	// Keyword or number
	word := p.readValueWord()
	if word == "" {
		return nil, p.errorf("expected value, got %q", string(ch))
	}

	switch word {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	default:
		// Try integer
		if n, err := strconv.ParseInt(word, 10, 64); err == nil {
			return n, nil
		}
		// Try float
		if f, err := strconv.ParseFloat(word, 64); err == nil {
			return f, nil
		}
		return nil, p.errorf("unexpected value %q", word)
	}
}

func (p *seedParser) parseArray() ([]any, error) {
	if p.peek() != '[' {
		return nil, p.errorf("expected '['")
	}
	p.advance()
	p.skipWhitespaceAndComments()

	var items []any
	for p.pos < len(p.input) && p.peek() != ']' {
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		items = append(items, value)

		p.skipWhitespaceAndComments()
		if p.peek() == ',' {
			p.advance()
			p.skipWhitespaceAndComments()
		}
	}

	if p.peek() != ']' {
		return nil, p.errorf("expected ']'")
	}
	p.advance()

	return items, nil
}

// --- Annotation parsing (reused pattern from concept_parser) ---

type seedAnnotation struct {
	name string
	text string
	args map[string]string
}

func (p *seedParser) parseAnnotation() (seedAnnotation, error) {
	if p.peek() != '@' {
		return seedAnnotation{}, p.errorf("expected '@'")
	}
	p.advance()

	name := p.readWord()
	if name == "" {
		return seedAnnotation{}, p.errorf("expected annotation name after '@'")
	}

	ann := seedAnnotation{name: name, args: make(map[string]string)}
	p.skipInlineWhitespace()

	if p.peek() != '(' {
		return ann, nil
	}
	p.advance()
	p.skipWhitespaceAndComments()

	// Single string arg: @actor("system")
	if p.peek() == '"' {
		text, err := p.readString()
		if err != nil {
			return seedAnnotation{}, err
		}
		ann.text = text
		p.skipWhitespaceAndComments()
		if p.peek() != ')' {
			return seedAnnotation{}, p.errorf("expected ')' after annotation string")
		}
		p.advance()
		return ann, nil
	}

	// Key=value pairs: @match(field="id", value="something")
	for p.pos < len(p.input) && p.peek() != ')' {
		p.skipWhitespaceAndComments()
		key := p.readWord()
		if key == "" {
			return seedAnnotation{}, p.errorf("expected key in @%s", name)
		}
		p.skipInlineWhitespace()
		if p.peek() != '=' {
			return seedAnnotation{}, p.errorf("expected '=' after %q in @%s", key, name)
		}
		p.advance()
		p.skipInlineWhitespace()

		var value string
		if p.peek() == '"' {
			v, err := p.readString()
			if err != nil {
				return seedAnnotation{}, err
			}
			value = v
		} else {
			value = p.readUntil(func(ch byte) bool {
				return ch == ',' || ch == ')' || ch == ' ' || ch == '\t' || ch == '\n'
			})
		}
		ann.args[key] = value

		p.skipWhitespaceAndComments()
		if p.peek() == ',' {
			p.advance()
			p.skipWhitespaceAndComments()
		}
	}

	if p.peek() != ')' {
		return seedAnnotation{}, p.errorf("expected ')' at end of @%s", name)
	}
	p.advance()

	return ann, nil
}

// --- Lexical helpers ---

func (p *seedParser) peek() byte {
	if p.pos >= len(p.input) {
		return 0
	}
	return p.input[p.pos]
}

func (p *seedParser) advance() {
	if p.pos < len(p.input) {
		if p.input[p.pos] == '\n' {
			p.line++
			p.col = 1
		} else {
			p.col++
		}
		p.pos++
	}
}

func (p *seedParser) skipInlineWhitespace() {
	for p.pos < len(p.input) && (p.input[p.pos] == ' ' || p.input[p.pos] == '\t') {
		p.advance()
	}
}

func (p *seedParser) skipWhitespaceAndComments() {
	for p.pos < len(p.input) {
		ch := p.input[p.pos]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			p.advance()
			continue
		}
		if ch == '/' && p.pos+1 < len(p.input) && p.input[p.pos+1] == '/' {
			for p.pos < len(p.input) && p.input[p.pos] != '\n' {
				p.advance()
			}
			continue
		}
		if ch == '/' && p.pos+1 < len(p.input) && p.input[p.pos+1] == '*' {
			p.advance()
			p.advance()
			for p.pos+1 < len(p.input) {
				if p.input[p.pos] == '*' && p.input[p.pos+1] == '/' {
					p.advance()
					p.advance()
					break
				}
				p.advance()
			}
			continue
		}
		break
	}
}

func (p *seedParser) readWord() string {
	start := p.pos
	for p.pos < len(p.input) {
		ch := p.input[p.pos]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' {
			p.advance()
		} else {
			break
		}
	}
	return p.input[start:p.pos]
}

// readValueWord reads a word that could also contain dots, hyphens, colons (for unquoted values like dates).
func (p *seedParser) readValueWord() string {
	start := p.pos
	for p.pos < len(p.input) {
		ch := p.input[p.pos]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '.' || ch == '-' || ch == ':' {
			p.advance()
		} else {
			break
		}
	}
	return p.input[start:p.pos]
}

func (p *seedParser) readString() (string, error) {
	if p.peek() != '"' {
		return "", p.errorf("expected '\"'")
	}
	p.advance()

	var sb strings.Builder
	for p.pos < len(p.input) {
		ch := p.input[p.pos]
		if ch == '\\' && p.pos+1 < len(p.input) {
			p.advance()
			next := p.input[p.pos]
			switch next {
			case '"', '\\', '/':
				sb.WriteByte(next)
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			default:
				sb.WriteByte('\\')
				sb.WriteByte(next)
			}
			p.advance()
			continue
		}
		if ch == '"' {
			p.advance()
			return sb.String(), nil
		}
		sb.WriteByte(ch)
		p.advance()
	}
	return "", p.errorf("unterminated string")
}

func (p *seedParser) readUntil(stop func(byte) bool) string {
	start := p.pos
	for p.pos < len(p.input) && !stop(p.input[p.pos]) {
		p.advance()
	}
	return p.input[start:p.pos]
}

func (p *seedParser) errorf(format string, args ...any) error {
	return fmt.Errorf("seed.memql line %d, col %d: %s", p.line, p.col, fmt.Sprintf(format, args...))
}
