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
			Description: "instruction-override: 'forget everything (above|previous|...)' (bare form too)",
			Categories:  []Category{CategoryPromptInjection, CategoryInstructionOverride},
			Tier:        TierHigh,
			Verdict:     ScreeningVerdictBlocked,
			// Two shapes: (1) "forget everything" or "forget all" in
			// imperative position (line-start or after . / : / newline)
			// catches the bare jailbreak directive; (2) "forget ... above
			// / previous / prior / earlier / instructions" catches the
			// rich form. (?m) so ^ can anchor mid-blob.
			Pattern: regexp.MustCompile(`(?m)(?:^|[.!?;:]\s+|\n)\s*forget\s+(?:everything|all\s+(?:of\s+)?(?:that|this)?|the\s+(?:above|previous|prior|earlier)|prior\s+instructions?|all\s+prior)\b|forget\s+(?:everything|all|the\s+(?:above|previous|prior|earlier))\s+(?:above|previous|prior|earlier|instructions?|i\s+(?:told|said))`),
		},
		{
			ID:          "prompt_injection.role_hijack_system_tag",
			Description: "role-hijack via system/instruction delimiter tags (system/sys/inst, im_start, Llama variants)",
			Categories:  []Category{CategoryPromptInjection, CategoryRoleHijack, CategoryDelimiterAbuse},
			Tier:        TierHigh,
			Verdict:     ScreeningVerdictBlocked,
			// Cover the common-in-the-wild variants: vanilla <system>,
			// Llama <<SYS>> / <</SYS>> (terminated OR newline-truncated),
			// [INST] / [/INST], ChatML <|im_start|>system / <|im_end|>,
			// model-specific <|system|> / <|system_start|>.
			Pattern: regexp.MustCompile(`(?:</?system>|<</?sys(?:>>|\b)|\[/?inst\]|<\|im_(?:start|end)\|>\s*system|<\|/?system(?:_start)?\|>)`),
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
			Description: "role label at start of line with high-signal injection directive ('system: ignore previous', 'assistant: you are now')",
			Categories:  []Category{CategoryPromptInjection, CategoryRoleHijack, CategoryDelimiterAbuse},
			Tier:        TierMedium,
			Verdict:     ScreeningVerdictSuspicious,
			// Tightened to require a HIGH-SIGNAL directive after the
			// label so forwarded chat logs / mail-list archives /
			// auto-replies containing benign 'system: maintenance'
			// or 'system: please ignore this auto-reply' don't trip.
			// Must be: 'ignore (all|previous|prior)' / 'you are
			// (now|a|an)' / 'disregard (all|prior|previous)' /
			// 'respond only with' / 'new instructions'.
			Pattern: regexp.MustCompile(`(?m)^\s*(?:system|assistant)\s*:\s*(?:ignore\s+(?:all|previous|prior)|you\s+are\s+(?:now|a|an)|disregard\s+(?:all|prior|previous)|respond\s+only\s+with|new\s+instructions?\s*:)`),
		},
		{
			ID:          "prompt_injection.tool_call_inject",
			Description: "embedded tool-invocation DIRECTIVE (imperative): 'please/you must call <tool/mutation/...> ...'",
			Categories:  []Category{CategoryPromptInjection, CategoryInstructionOverride},
			Tier:        TierMedium,
			Verdict:     ScreeningVerdictSuspicious,
			// Tightened from the prior `(?:^|\s)(?:call|...)` which
			// matched every benign "call the function X()" reference
			// in docs / READMEs / code samples the agent fetches.
			// Now requires an imperative-directive context (please /
			// you must / now / immediately / go ahead) so the rule
			// fires only on instruction-shaped phrasings that
			// implicate the agent.
			Pattern: regexp.MustCompile(`(?:please|you\s+(?:must|should)|now|immediately|go\s+ahead\s+and|next,?\s+(?:please\s+)?)\s+(?:call|invoke|execute|run)\s+(?:the\s+)?(?:tool|mutation|query|function|skill|builtin)\s+\w+\s*\(`),
		},
		{
			ID:          "prompt_injection.exfil_url_marker",
			Description: "exfiltration vector: 'send/post/exfiltrate ... https?://...' (newline-tolerant bridge)",
			Categories:  []Category{CategoryPromptInjection, CategoryExfiltration},
			Tier:        TierMedium,
			Verdict:     ScreeningVerdictSuspicious,
			// (?s) flag makes `.` match newlines so attacks that
			// split the directive across lines ('send X to:\n
			// https://attacker.com') still fire. Cap at 80 chars to
			// avoid pathological backtracking on adversarial input.
			// Tightened to also require the URL host to look like a
			// domain (rules out 'send your code to https://example
			// in markdown' false-positives on URL-shaped markdown).
			Pattern: regexp.MustCompile(`(?s)(?:send|post|exfiltrate|forward|transmit)\s.{0,80}?https?://[a-z0-9-]+\.[a-z]`),
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
