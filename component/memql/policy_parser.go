package memql

import (
	"fmt"
	"strings"

	"github.com/visionarys-io/memql/component/memql/baseparser"
)

// PolicyConfig is the parsed shape of a policy .memql file. Mirrors the
// annotations: @primary (one), @fallback (many), @maxLatencyMs,
// @maxTimeToFirstTokenMs, @preferredRole (many). Used by Router to
// resolve a policy name to a concrete provider + ordered fallback chain.
type PolicyConfig struct {
	Name                  string
	Description           string
	Primary               string
	Fallbacks             []string
	MaxLatencyMs          int
	MaxTimeToFirstTokenMs int
	PreferredRoles        []string
}

// ProviderChain returns primary followed by fallbacks, which is the
// order the Router attempts providers in.
func (p PolicyConfig) ProviderChain() []string {
	chain := make([]string, 0, len(p.Fallbacks)+1)
	if strings.TrimSpace(p.Primary) != "" {
		chain = append(chain, p.Primary)
	}
	for _, f := range p.Fallbacks {
		f = strings.TrimSpace(f)
		if f != "" {
			chain = append(chain, f)
		}
	}
	return chain
}

// parsePolicyMemQL parses a .memql policy file into a PolicyConfig.
// The grammar is narrow:
//
//	@description("...")
//	@primary("providerName")
//	@fallback("providerName")        -- repeatable
//	@maxLatencyMs(8000)
//	@maxTimeToFirstTokenMs(500)
//	@preferredRole("roleName")       -- repeatable
//	policy <name> { }
//
// The `policy` block is empty today; keeping it in the grammar reserves
// syntactic space for future tuning knobs (e.g. per-vendor penalties).
func parsePolicyMemQL(origin string, raw []byte) (*PolicyConfig, error) {
	p := &policyMemQLParser{}
	p.Init(string(raw), origin)
	return p.parse(origin)
}

type policyMemQLParser struct {
	baseparser.Base
}

func (p *policyMemQLParser) parse(origin string) (*PolicyConfig, error) {
	cfg := &PolicyConfig{}

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
			p.Advance()
			if err := p.parseDecorator(cfg); err != nil {
				return nil, p.Errorf("%s", err)
			}
			continue
		}

		if p.MatchWord("policy") {
			p.SkipWhitespaceAndComments()
			cfg.Name = p.ReadWord()
			if cfg.Name == "" {
				return nil, p.Errorf("expected policy name")
			}
			p.SkipWhitespaceAndComments()
			if !p.EOF() && p.Peek() == '{' {
				p.SkipBalancedBraces()
			}
			break
		}

		p.Advance()
	}

	if cfg.Name == "" {
		return nil, fmt.Errorf("%s: no policy declaration found", origin)
	}
	if cfg.Primary == "" {
		return nil, fmt.Errorf("%s: policy %q missing @primary", origin, cfg.Name)
	}
	return cfg, nil
}

func (p *policyMemQLParser) parseDecorator(cfg *PolicyConfig) error {
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
		cfg.Description = val
	case "primary":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		cfg.Primary = strings.TrimSpace(val)
	case "fallback":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		cfg.Fallbacks = append(cfg.Fallbacks, strings.TrimSpace(val))
	case "maxLatencyMs":
		val, err := p.ParseParenInt()
		if err != nil {
			return err
		}
		cfg.MaxLatencyMs = int(val)
	case "maxTimeToFirstTokenMs":
		val, err := p.ParseParenInt()
		if err != nil {
			return err
		}
		cfg.MaxTimeToFirstTokenMs = int(val)
	case "preferredRole":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		cfg.PreferredRoles = append(cfg.PreferredRoles, strings.TrimSpace(val))
	default:
		// Unknown decorator -- skip its argument group if present, so
		// future annotations don't require an immediate parser update.
		p.SkipWhitespaceInline()
		if !p.EOF() && p.Peek() == '(' {
			p.SkipBalancedParens()
		}
	}
	return nil
}

