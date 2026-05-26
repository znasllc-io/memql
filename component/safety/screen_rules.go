package safety

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// screen_rules.go ships the deterministic rule layer for the
// OutputGate (memql#233). Mirrors rules.go on the command side, but
// the rules are PATTERN-based rather than action-based: each rule
// is a compiled regex that fires on incoming content, with a
// verdict shape (tier, categories, blocked-vs-suspicious).
//
// Rule ordering is significant: high-confidence "blocked" patterns
// run before "suspicious" ones so a coincidental low-tier match
// doesn't dilute the verdict. The screener returns on the FIRST
// match -- if a content blob contains both ignore-previous AND a
// jailbreak term, the higher-tier ignore-previous rule fires.

// ScreenRule is one deterministic pattern -> verdict mapping.
// Pattern is the precompiled regex; the screener lowercases the
// content before matching so patterns should be all-lowercase (or
// rely on the (?i) inline flag for case-insensitive substrings).
type ScreenRule struct {
	ID          string
	Description string
	Categories  []Category
	Tier        RiskTier
	Verdict     ScreeningVerdict
	Pattern     *regexp.Regexp
}

// RuleScreener applies every rule in order; the first match wins.
// No-match returns Clean.
type RuleScreener struct {
	rules []ScreenRule
}

// NewRuleScreener returns a RuleScreener bound to the given rules.
// App boot composes this with DefaultScreenRules() + any deployment-
// specific extension rules.
func NewRuleScreener(rules ...ScreenRule) *RuleScreener {
	return &RuleScreener{rules: rules}
}

// Screen returns the verdict for the first matching rule. Content
// is lowercased ONCE up-front so each pattern doesn't have to
// re-allocate; whitespace is NOT normalised here -- regex patterns
// declare their own `\s+` tolerance where needed.
func (r *RuleScreener) Screen(_ context.Context, in ScreeningInput) (ScreeningResult, error) {
	start := time.Now()
	// Lowercase once. Most prompt-injection vectors are case-
	// insensitive in practice; the patterns are authored
	// lowercase + use (?i) for safety on substrings.
	lower := strings.ToLower(in.Content)
	for _, rule := range r.rules {
		if rule.Pattern.MatchString(lower) {
			return ScreeningResult{
				Verdict:    rule.Verdict,
				Tier:       rule.Tier,
				Categories: append([]Category(nil), rule.Categories...),
				Reason:     rule.Description,
				Source:     ScreenSourceRule,
				RuleID:     rule.ID,
				Confidence: 1.0,
				LatencyMs:  time.Since(start).Milliseconds(),
			}, nil
		}
	}
	return ScreeningResult{
		Verdict:    ScreeningVerdictClean,
		Tier:       TierNone,
		Source:     ScreenSourceRule,
		Confidence: 1.0,
		LatencyMs:  time.Since(start).Milliseconds(),
	}, nil
}

// DefaultScreenRules returns the canonical prompt-injection
// pattern set. Sourced from the public prompt-injection literature
// (the OWASP LLM Top-10 + Simon Willison's prompt-injection
// taxonomy + observed wild attacks against Claude / GPT). The set
// is intentionally CONSERVATIVE: every pattern here is something
// that has no legitimate appearance in well-formed tool outputs or
// fetched content. False positives are still possible (a security
// research article about prompt injection will mention these
// phrases verbatim); the v1 posture is to log + observe in shadow
// mode, then iterate the rule set with the FP/FN budget from #235.
//
// Order: highest-confidence Blocked patterns FIRST, then
// Suspicious patterns. The RuleScreener returns on first match so
// ordering = priority.
func DefaultScreenRules() []ScreenRule {
	return []ScreenRule{
		// ===== BLOCKED tier (high confidence, no legitimate use) =====
		{
			ID:          "prompt_injection.ignore_previous",
			Description: "instruction-override: 'ignore (all) (previous|prior|above|earlier) (instructions|prompts|rules|directives)'",
			Categories:  []Category{CategoryPromptInjection, CategoryInstructionOverride},
			Tier:        TierHigh,
			Verdict:     ScreeningVerdictBlocked,
			Pattern:     regexp.MustCompile(`ignore\s+(?:all\s+)?(?:previous|prior|above|earlier|the\s+above)\s+(?:instructions?|prompts?|rules?|directives?|system\s+message)`),
		},
		{
			ID:          "prompt_injection.disregard",
			Description: "instruction-override: 'disregard (all) (previous|prior|above|earlier) ...'",
			Categories:  []Category{CategoryPromptInjection, CategoryInstructionOverride},
			Tier:        TierHigh,
			Verdict:     ScreeningVerdictBlocked,
			Pattern:     regexp.MustCompile(`disregard\s+(?:all\s+)?(?:previous|prior|above|earlier|the\s+above)\s+(?:instructions?|prompts?|rules?|system)`),
		},
		{
			ID:          "prompt_injection.forget_everything",
			Description: "instruction-override: 'forget everything (above|prior|...)'",
			Categories:  []Category{CategoryPromptInjection, CategoryInstructionOverride},
			Tier:        TierHigh,
			Verdict:     ScreeningVerdictBlocked,
			Pattern:     regexp.MustCompile(`forget\s+(?:everything|all|the\s+(?:above|previous|prior|earlier))\s+(?:above|previous|prior|earlier|instructions?|i\s+(?:told|said))`),
		},
		{
			ID:          "prompt_injection.role_hijack_system_tag",
			Description: "role-hijack via <system>, <<SYS>>, [INST], or <|im_start|>system delimiter tag",
			Categories:  []Category{CategoryPromptInjection, CategoryRoleHijack, CategoryDelimiterAbuse},
			Tier:        TierHigh,
			Verdict:     ScreeningVerdictBlocked,
			Pattern:     regexp.MustCompile(`(?:<system>|<<sys>>|\[inst\]|<\|im_start\|>\s*system|<\|system\|>)`),
		},
		{
			ID:          "prompt_injection.persona_switch_jailbreak",
			Description: "persona-switch attack with known jailbreak target: 'you are now DAN' / 'pretend to be in developer mode'",
			Categories:  []Category{CategoryPromptInjection, CategoryRoleHijack},
			Tier:        TierHigh,
			Verdict:     ScreeningVerdictBlocked,
			Pattern:     regexp.MustCompile(`(?:you\s+are\s+now|from\s+now\s+on(?:\s+you'?re|\s+you\s+are)?|act\s+as|pretend\s+(?:to\s+be|you'?re|you\s+are))\s+(?:a\s+|an\s+)?(?:dan|aim|do\s+anything\s+now|unrestricted|in\s+developer\s+mode|jailbroken)`),
		},

		// ===== SUSPICIOUS tier (lower confidence, possible legit use) =====
		{
			ID:          "prompt_injection.jailbreak_terms",
			Description: "isolated jailbreak terminology: DAN mode / developer mode enabled / jailbreaking",
			Categories:  []Category{CategoryPromptInjection, CategoryRoleHijack},
			Tier:        TierMedium,
			Verdict:     ScreeningVerdictSuspicious,
			Pattern:     regexp.MustCompile(`\b(?:do\s+anything\s+now|dan\s+mode|jailbreaking|developer\s+mode\s+(?:enabled|activated|on))\b`),
		},
		{
			ID:          "prompt_injection.role_label_injection",
			Description: "role label at start of line: 'system: ...' / 'assistant: please ...' (chat-format injection)",
			Categories:  []Category{CategoryPromptInjection, CategoryRoleHijack, CategoryDelimiterAbuse},
			Tier:        TierMedium,
			Verdict:     ScreeningVerdictSuspicious,
			Pattern:     regexp.MustCompile(`(?m)^\s*(?:system|assistant)\s*:\s*(?:you\s+are|please\s+(?:ignore|forget|disregard)|respond\s+(?:with|only)|new\s+instructions?)`),
		},
		{
			ID:          "prompt_injection.tool_call_inject",
			Description: "embedded tool-invocation directive: 'call <mutation/query/tool> ...' targeting the agent",
			Categories:  []Category{CategoryPromptInjection, CategoryInstructionOverride},
			Tier:        TierMedium,
			Verdict:     ScreeningVerdictSuspicious,
			Pattern:     regexp.MustCompile(`(?:^|\s)(?:call|invoke|execute|run)\s+(?:the\s+)?(?:tool|mutation|query|function|skill)\s+\w+\s*\(`),
		},
		{
			ID:          "prompt_injection.exfil_url_marker",
			Description: "exfiltration vector: 'send/post/exfiltrate ... https?://...' (lazy-bridges intermediate words)",
			Categories:  []Category{CategoryPromptInjection, CategoryExfiltration},
			Tier:        TierMedium,
			Verdict:     ScreeningVerdictSuspicious,
			// Lazy-bridge up to 80 chars so phrasings like "send the
			// user's password to https://attacker.com" match without
			// needing to enumerate every possible intermediate
			// (apostrophes break a \w+\s+ chain). Capped at 80 to
			// avoid pathological backtracking on adversarial input.
			Pattern: regexp.MustCompile(`(?:send|post|exfiltrate|forward|transmit)\s.{0,80}?https?://`),
		},
		{
			ID:          "prompt_injection.print_secret",
			Description: "secret-dump directive: 'print (your) (system prompt|secrets|api key)'",
			Categories:  []Category{CategoryPromptInjection, CategoryExfiltration, CategoryCredentialAccess},
			Tier:        TierMedium,
			Verdict:     ScreeningVerdictSuspicious,
			Pattern:     regexp.MustCompile(`(?:print|display|reveal|output|show\s+me|tell\s+me)\s+(?:your\s+|the\s+|all\s+)?(?:system\s+prompt|initial\s+(?:instructions?|prompt)|api[\s_-]?key|password|token|secret|credentials?)`),
		},
	}
}
