package memql

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/visionarys-io/memql/component/memql/baseparser"
)

// parseAgentMemQL parses a `.memql` agent definition file and
// returns an agentDecl. Agents use the canonical struct form -- no
// receiver-function wrapping -- mirroring concept / shape / tool /
// provider / prompt:
//
//	@version("1.0.0")
//	@namespace("agents")
//	@scope("perUser")
//	@visibility("bff", "cognition", "agent")
//	@templateFile("templates/generalAssistant.tmpl")
//	@description("Per-user General Assistant.")
//	agent generalAssistant {
//	  role:        "general_assistant"
//	  roleSlug:    "general_assistant"
//	  name:        "General Assistant"
//	  description: "Designated fallback when no specialist fits."
//	  personality: "Friendly, capable, proactive."
//	  gender:      "female"
//
//	  providerConfig {
//	    llm {
//	      policyName:  "balancedChat"
//	      temperature: 0.7
//	      maxTokens:   4000
//	    }
//	  }
//
//	  capabilities {
//	    avatar:       true
//	    lipSync:      true
//	    vision:       true
//	    voiceToVoice: true
//	    claw:         false
//	    tools:    [tool("respondToUser"), tool("uiClick"),
//	               tool("uiNarrate"), tool("uiDescribe")]
//	    domains:  []
//	    keywords: []
//	  }
//
//	  knowledge: [knowledgeDomain("generalAssistantBaseline")]
//
//	  triggerBehavior {
//	    autoJoin:          true
//	    greetOnJoin:       true
//	    interruptionStyle: "polite"
//	    speakWhen:         "always"
//	  }
//
//	  audioControl: "mirror_user"
//	  videoControl: "mirror_user"
//	}
//
// The body is VALUE-shaped (key: value + nested blocks), distinct
// from the SCHEMA-shaped bodies of `concept`. Closest analog is
// provider's `auth { ... } params { ... }` blocks.
//
// Phase 1 of the agents-dsl-primitive feature (see
// docs/planning/agents-dsl-primitive.md): this parser produces an
// agentDecl. Compilation (cross-reference validation for tool() and
// knowledgeDomain() refs, @templateFile resolution) lands in Phase 2;
// row materialization in Phase 3; the `agent(name, args)` builtin
// in Phase 4.
func parseAgentMemQL(origin string, raw []byte) (*agentDecl, error) {
	p := &agentMemQLParser{}
	p.Init(string(raw), origin)
	return p.parse(origin)
}

// agentMemQLParser embeds baseparser.Base for scanning primitives.
type agentMemQLParser struct {
	baseparser.Base
}

// agentDecl is the parsed top-level agent declaration. Holds every
// baseline-subset field plus the annotation values. Compilation
// lowers this into the engine-internal AgentDefinition (Phase 2).
type agentDecl struct {
	// Annotation values
	name         string
	description  string
	namespace    string
	version      string
	scope        string // "global" | "perUser" -- empty defaults to "perUser"
	visibility   []string
	templateFile string

	// Body identity
	role        string
	roleSlug    string
	displayName string
	bodyDesc    string // body-level `description: "..."` (distinct from @description)
	personality string
	gender      string

	// providerConfig.llm.*
	llmProvider    string
	llmModel       string
	llmPolicyName  string
	llmTemperature float64
	llmTempSet     bool
	llmMaxTokens   int
	llmMaxTokSet   bool

	// capabilities.*
	capAvatar        bool
	capLipSync       bool
	capVision        bool
	capVoiceToVoice  bool
	capClaw          bool
	capClawWorkspace string
	capDomains       []string
	capKeywords      []string
	capTools         []agentRef // reference-typed collection

	// Top-level knowledge bindings (separate from capabilities.domains)
	knowledge []agentRef

	// triggerBehavior.*
	tbAutoJoin          bool
	tbGreetOnJoin       bool
	tbInterruptionStyle string
	tbSpeakWhen         string

	// Media control
	audioControl string
	videoControl string
}

// agentRef represents a reference-constructor call inside the body,
// e.g. `tool("respondToUser")` or `knowledgeDomain("baseline")`.
// Cross-reference resolution against the construct registry happens
// in Phase 2 (compiler).
type agentRef struct {
	Kind string // "tool" | "knowledgeDomain"
	Name string
}

func (p *agentMemQLParser) parse(origin string) (*agentDecl, error) {
	decl := &agentDecl{}

	for !p.EOF() {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			break
		}

		ch := p.Peek()

		if p.MatchWord("use") {
			p.SkipToEndOfLine()
			continue
		}

		if ch == '@' {
			p.Advance() // consume @
			if err := p.parseDecorator(decl); err != nil {
				return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, err)
			}
			continue
		}

		if p.MatchWord("agent") {
			p.SkipWhitespaceAndComments()
			decl.name = p.ReadWord()
			if decl.name == "" {
				return nil, fmt.Errorf("%s:%d:%d: expected agent name after 'agent'", origin, p.Line, p.Col)
			}
			if err := p.parseAgentBody(decl); err != nil {
				return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, err)
			}
			break
		}

		// `func (Agent)` form has never existed; if a stale file
		// ever tries it, fail loud with the same migration shape
		// other primitives use.
		if p.MatchWord("func") {
			return nil, fmt.Errorf("%s:%d:%d: no `func (Agent)` form exists -- use canonical struct form `agent name { ... }`", origin, p.Line, p.Col)
		}

		p.Advance()
	}

	if decl.name == "" {
		return nil, fmt.Errorf("%s: no agent definition found", origin)
	}
	return decl, nil
}

// -----------------------------------------------------------------
// Decorators
// -----------------------------------------------------------------

func (p *agentMemQLParser) parseDecorator(decl *agentDecl) error {
	name := p.ReadWord()
	if name == "" {
		return fmt.Errorf("expected decorator name after @")
	}

	switch name {
	case "enabled", "disabled":
		// Tolerated for parity with other primitives; no semantic effect on agent decls.
		return nil

	case "description":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.description = val
		return nil

	case "version":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.version = val
		return nil

	case "namespace":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.namespace = val
		return nil

	case "scope":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		switch val {
		case "global", "perUser":
			decl.scope = val
		default:
			return fmt.Errorf("@scope must be \"global\" or \"perUser\", got %q", val)
		}
		return nil

	case "visibility":
		vals, err := p.parseParenStringList()
		if err != nil {
			return err
		}
		decl.visibility = vals
		return nil

	case "templateFile":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.templateFile = val
		return nil

	default:
		// Unknown annotation: reject so typos surface immediately.
		// (Matches the strict-superset policy from the brainstorm:
		// unknown annotations should not be silently dropped.)
		return fmt.Errorf("unknown agent annotation @%s (allowed: @version, @namespace, @scope, @visibility, @description, @templateFile, @enabled, @disabled)", name)
	}
}

// parseParenStringList consumes a parenthesized comma-separated
// quoted-string list: ("a", "b", "c"). Returns the values in order.
// Used by @visibility.
func (p *agentMemQLParser) parseParenStringList() ([]string, error) {
	p.SkipWhitespaceInline()
	if p.EOF() || p.Peek() != '(' {
		return nil, fmt.Errorf("expected '(' after annotation")
	}
	p.Advance() // consume (
	var out []string
	for {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			return nil, fmt.Errorf("unexpected EOF inside annotation arg list")
		}
		if p.Peek() == ')' {
			p.Advance()
			return out, nil
		}
		if p.Peek() != '"' {
			return nil, fmt.Errorf("expected quoted string in annotation arg list")
		}
		s, err := p.readQuotedString()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		p.SkipWhitespaceAndComments()
		if !p.EOF() && p.Peek() == ',' {
			p.Advance()
			continue
		}
	}
}

// -----------------------------------------------------------------
// Body parsing
// -----------------------------------------------------------------

// parseAgentBody parses `{ ... }` containing top-level scalar
// assignments + nested blocks. Each top-level entry is either:
//
//	key: <value>            scalar assignment (string/bool/int/float)
//	key: [<refs>]           reference-typed array
//	key: [<strings>]        string array
//	key { ... }             nested block (providerConfig, capabilities,
//	                        triggerBehavior, providerConfig.llm)
func (p *agentMemQLParser) parseAgentBody(decl *agentDecl) error {
	p.SkipWhitespaceAndComments()
	if p.EOF() || p.Peek() != '{' {
		return fmt.Errorf("expected '{' to start agent body")
	}
	p.Advance() // {

	for !p.EOF() {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			return fmt.Errorf("unexpected EOF in agent body")
		}
		if p.Peek() == '}' {
			p.Advance()
			return nil
		}

		key := p.ReadWord()
		if key == "" {
			// Tolerate stray chars; advance to avoid infinite loop.
			p.Advance()
			continue
		}

		p.SkipWhitespaceInline()
		ch := p.Peek()

		switch ch {
		case ':':
			p.Advance() // :
			if err := p.parseTopLevelAssignment(decl, key); err != nil {
				return fmt.Errorf("field %q: %w", key, err)
			}
		case '{':
			if err := p.parseNestedBlock(decl, key); err != nil {
				return fmt.Errorf("block %q: %w", key, err)
			}
		default:
			return fmt.Errorf("expected ':' or '{' after %q, got %q", key, string(ch))
		}
	}
	return fmt.Errorf("unexpected EOF, missing '}' on agent body")
}

func (p *agentMemQLParser) parseTopLevelAssignment(decl *agentDecl, key string) error {
	p.SkipWhitespaceAndComments()
	if p.EOF() {
		return fmt.Errorf("unexpected EOF after ':'")
	}

	switch key {
	case "role":
		s, err := p.readQuotedString()
		if err != nil {
			return err
		}
		decl.role = s
	case "roleSlug":
		s, err := p.readQuotedString()
		if err != nil {
			return err
		}
		decl.roleSlug = s
	case "name":
		s, err := p.readQuotedString()
		if err != nil {
			return err
		}
		decl.displayName = s
	case "description":
		s, err := p.readQuotedString()
		if err != nil {
			return err
		}
		decl.bodyDesc = s
	case "personality":
		s, err := p.readQuotedString()
		if err != nil {
			return err
		}
		decl.personality = s
	case "gender":
		s, err := p.readQuotedString()
		if err != nil {
			return err
		}
		decl.gender = s
	case "audioControl":
		s, err := p.readQuotedString()
		if err != nil {
			return err
		}
		decl.audioControl = s
	case "videoControl":
		s, err := p.readQuotedString()
		if err != nil {
			return err
		}
		decl.videoControl = s
	case "knowledge":
		refs, err := p.parseRefArray("knowledgeDomain")
		if err != nil {
			return err
		}
		decl.knowledge = refs
	default:
		return fmt.Errorf("unknown top-level field %q on agent (allowed: role, roleSlug, name, description, personality, gender, audioControl, videoControl, knowledge)", key)
	}
	return nil
}

func (p *agentMemQLParser) parseNestedBlock(decl *agentDecl, key string) error {
	switch key {
	case "providerConfig":
		return p.parseProviderConfigBlock(decl)
	case "capabilities":
		return p.parseCapabilitiesBlock(decl)
	case "triggerBehavior":
		return p.parseTriggerBehaviorBlock(decl)
	default:
		return fmt.Errorf("unknown nested block %q on agent (allowed: providerConfig, capabilities, triggerBehavior)", key)
	}
}

// providerConfig { llm { policyName: "..." temperature: 0.7 maxTokens: 4000 } }
func (p *agentMemQLParser) parseProviderConfigBlock(decl *agentDecl) error {
	if err := p.expectOpenBrace(); err != nil {
		return err
	}
	for !p.EOF() {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			return fmt.Errorf("unexpected EOF in providerConfig block")
		}
		if p.Peek() == '}' {
			p.Advance()
			return nil
		}
		sub := p.ReadWord()
		if sub == "" {
			p.Advance()
			continue
		}
		switch sub {
		case "llm":
			if err := p.parseLLMBlock(decl); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown providerConfig sub-block %q (allowed: llm)", sub)
		}
	}
	return fmt.Errorf("unexpected EOF, missing '}' on providerConfig")
}

func (p *agentMemQLParser) parseLLMBlock(decl *agentDecl) error {
	if err := p.expectOpenBrace(); err != nil {
		return err
	}
	for !p.EOF() {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			return fmt.Errorf("unexpected EOF in llm block")
		}
		if p.Peek() == '}' {
			p.Advance()
			return nil
		}
		key := p.ReadWord()
		if key == "" {
			p.Advance()
			continue
		}
		p.SkipWhitespaceInline()
		if p.Peek() != ':' {
			return fmt.Errorf("expected ':' after llm.%s", key)
		}
		p.Advance()
		p.SkipWhitespaceAndComments()
		switch key {
		case "provider":
			s, err := p.readQuotedString()
			if err != nil {
				return err
			}
			decl.llmProvider = s
		case "model":
			s, err := p.readQuotedString()
			if err != nil {
				return err
			}
			decl.llmModel = s
		case "policyName":
			s, err := p.readQuotedString()
			if err != nil {
				return err
			}
			decl.llmPolicyName = s
		case "temperature":
			f, err := p.readFloat()
			if err != nil {
				return err
			}
			decl.llmTemperature = f
			decl.llmTempSet = true
		case "maxTokens":
			i, err := p.readInt()
			if err != nil {
				return err
			}
			decl.llmMaxTokens = i
			decl.llmMaxTokSet = true
		default:
			return fmt.Errorf("unknown llm field %q (allowed: provider, model, policyName, temperature, maxTokens)", key)
		}
	}
	return fmt.Errorf("unexpected EOF, missing '}' on llm")
}

func (p *agentMemQLParser) parseCapabilitiesBlock(decl *agentDecl) error {
	if err := p.expectOpenBrace(); err != nil {
		return err
	}
	for !p.EOF() {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			return fmt.Errorf("unexpected EOF in capabilities block")
		}
		if p.Peek() == '}' {
			p.Advance()
			return nil
		}
		key := p.ReadWord()
		if key == "" {
			p.Advance()
			continue
		}
		p.SkipWhitespaceInline()
		if p.Peek() != ':' {
			return fmt.Errorf("expected ':' after capabilities.%s", key)
		}
		p.Advance()
		p.SkipWhitespaceAndComments()
		switch key {
		case "avatar":
			b, err := p.readBool()
			if err != nil {
				return err
			}
			decl.capAvatar = b
		case "lipSync":
			b, err := p.readBool()
			if err != nil {
				return err
			}
			decl.capLipSync = b
		case "vision":
			b, err := p.readBool()
			if err != nil {
				return err
			}
			decl.capVision = b
		case "voiceToVoice":
			b, err := p.readBool()
			if err != nil {
				return err
			}
			decl.capVoiceToVoice = b
		case "claw":
			b, err := p.readBool()
			if err != nil {
				return err
			}
			decl.capClaw = b
		case "clawWorkspace":
			s, err := p.readQuotedString()
			if err != nil {
				return err
			}
			decl.capClawWorkspace = s
		case "domains":
			strs, err := p.parseStringArray()
			if err != nil {
				return err
			}
			decl.capDomains = strs
		case "keywords":
			strs, err := p.parseStringArray()
			if err != nil {
				return err
			}
			decl.capKeywords = strs
		case "tools":
			refs, err := p.parseRefArray("tool")
			if err != nil {
				return err
			}
			decl.capTools = refs
		default:
			return fmt.Errorf("unknown capability %q (allowed: avatar, lipSync, vision, voiceToVoice, claw, clawWorkspace, domains, keywords, tools)", key)
		}
	}
	return fmt.Errorf("unexpected EOF, missing '}' on capabilities")
}

func (p *agentMemQLParser) parseTriggerBehaviorBlock(decl *agentDecl) error {
	if err := p.expectOpenBrace(); err != nil {
		return err
	}
	for !p.EOF() {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			return fmt.Errorf("unexpected EOF in triggerBehavior block")
		}
		if p.Peek() == '}' {
			p.Advance()
			return nil
		}
		key := p.ReadWord()
		if key == "" {
			p.Advance()
			continue
		}
		p.SkipWhitespaceInline()
		if p.Peek() != ':' {
			return fmt.Errorf("expected ':' after triggerBehavior.%s", key)
		}
		p.Advance()
		p.SkipWhitespaceAndComments()
		switch key {
		case "autoJoin":
			b, err := p.readBool()
			if err != nil {
				return err
			}
			decl.tbAutoJoin = b
		case "greetOnJoin":
			b, err := p.readBool()
			if err != nil {
				return err
			}
			decl.tbGreetOnJoin = b
		case "interruptionStyle":
			s, err := p.readQuotedString()
			if err != nil {
				return err
			}
			decl.tbInterruptionStyle = s
		case "speakWhen":
			s, err := p.readQuotedString()
			if err != nil {
				return err
			}
			decl.tbSpeakWhen = s
		default:
			return fmt.Errorf("unknown triggerBehavior field %q (allowed: autoJoin, greetOnJoin, interruptionStyle, speakWhen)", key)
		}
	}
	return fmt.Errorf("unexpected EOF, missing '}' on triggerBehavior")
}

// -----------------------------------------------------------------
// Value-shape readers
// -----------------------------------------------------------------

// parseRefArray parses [kind("name"), kind("name"), ...]. The kind
// argument is the expected reference-constructor name ("tool" or
// "knowledgeDomain"). Cross-reference resolution against the
// construct registry happens at compile time in Phase 2.
func (p *agentMemQLParser) parseRefArray(expectedKind string) ([]agentRef, error) {
	p.SkipWhitespaceAndComments()
	if p.EOF() || p.Peek() != '[' {
		return nil, fmt.Errorf("expected '[' to start %s reference array", expectedKind)
	}
	p.Advance() // [
	var out []agentRef
	for {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			return nil, fmt.Errorf("unexpected EOF inside %s reference array", expectedKind)
		}
		if p.Peek() == ']' {
			p.Advance()
			return out, nil
		}
		kind := p.ReadWord()
		if kind == "" {
			return nil, fmt.Errorf("expected reference constructor (e.g. %s(\"name\")) inside array", expectedKind)
		}
		if kind != expectedKind {
			return nil, fmt.Errorf("expected %s(\"...\") references, got %s(...)", expectedKind, kind)
		}
		name, err := p.ParseParenString()
		if err != nil {
			return nil, fmt.Errorf("%s(\"...\"): %w", kind, err)
		}
		out = append(out, agentRef{Kind: kind, Name: name})
		p.SkipWhitespaceAndComments()
		if !p.EOF() && p.Peek() == ',' {
			p.Advance()
			continue
		}
	}
}

// parseStringArray parses ["a", "b", "c"] -- plain quoted strings.
func (p *agentMemQLParser) parseStringArray() ([]string, error) {
	p.SkipWhitespaceAndComments()
	if p.EOF() || p.Peek() != '[' {
		return nil, fmt.Errorf("expected '[' to start string array")
	}
	p.Advance() // [
	var out []string
	for {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			return nil, fmt.Errorf("unexpected EOF inside string array")
		}
		if p.Peek() == ']' {
			p.Advance()
			return out, nil
		}
		if p.Peek() != '"' {
			return nil, fmt.Errorf("expected quoted string inside array, got %q", string(p.Peek()))
		}
		s, err := p.readQuotedString()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		p.SkipWhitespaceAndComments()
		if !p.EOF() && p.Peek() == ',' {
			p.Advance()
			continue
		}
	}
}

// readQuotedString consumes a double-quoted string literal.
func (p *agentMemQLParser) readQuotedString() (string, error) {
	p.SkipWhitespaceAndComments()
	if p.EOF() || p.Peek() != '"' {
		return "", fmt.Errorf("expected '\"' to start string")
	}
	p.Advance() // "
	var sb strings.Builder
	for !p.EOF() {
		ch := p.Peek()
		if ch == '"' {
			p.Advance()
			return sb.String(), nil
		}
		if ch == '\\' {
			p.Advance()
			if p.EOF() {
				return "", fmt.Errorf("unterminated string escape")
			}
			esc := p.Peek()
			p.Advance()
			switch esc {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case '\\':
				sb.WriteByte('\\')
			case '"':
				sb.WriteByte('"')
			default:
				sb.WriteByte(esc)
			}
			continue
		}
		sb.WriteByte(ch)
		p.Advance()
	}
	return "", fmt.Errorf("unterminated string")
}

func (p *agentMemQLParser) readBool() (bool, error) {
	p.SkipWhitespaceAndComments()
	if p.MatchWord("true") {
		return true, nil
	}
	if p.MatchWord("false") {
		return false, nil
	}
	return false, fmt.Errorf("expected 'true' or 'false'")
}

func (p *agentMemQLParser) readInt() (int, error) {
	s := p.readNumericLiteral()
	if s == "" {
		return 0, fmt.Errorf("expected integer")
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("expected integer, got %q: %w", s, err)
	}
	return v, nil
}

func (p *agentMemQLParser) readFloat() (float64, error) {
	s := p.readNumericLiteral()
	if s == "" {
		return 0, fmt.Errorf("expected number")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("expected number, got %q: %w", s, err)
	}
	return v, nil
}

func (p *agentMemQLParser) readNumericLiteral() string {
	p.SkipWhitespaceAndComments()
	var sb strings.Builder
	if !p.EOF() && (p.Peek() == '-' || p.Peek() == '+') {
		sb.WriteByte(p.Peek())
		p.Advance()
	}
	for !p.EOF() {
		ch := p.Peek()
		if (ch >= '0' && ch <= '9') || ch == '.' {
			sb.WriteByte(ch)
			p.Advance()
			continue
		}
		break
	}
	return sb.String()
}

func (p *agentMemQLParser) expectOpenBrace() error {
	p.SkipWhitespaceAndComments()
	if p.EOF() || p.Peek() != '{' {
		return fmt.Errorf("expected '{'")
	}
	p.Advance()
	return nil
}
