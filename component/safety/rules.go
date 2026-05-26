package safety

import (
	"context"
	"errors"
	"time"
)

// Rule is one curated check that may produce a Classification for a
// proposed action, or escalate (`ErrNoClassifierOpinion`) when it
// has no opinion. Rules are walked in priority order by
// `RuleClassifier`; the first non-escalation verdict wins.
//
// `ID` lands in audit + telemetry as the rule that fired -- pick
// something stable + greppable (e.g. `shell.destructive.rm_rf_root`).
//
// `Fn` is the predicate. It runs synchronously on the dispatch hot
// path, so keep it cheap -- regex match + a couple string ops, no
// I/O. Returning anything other than `nil` or `ErrNoClassifierOpinion`
// is treated as a real classifier error and propagated.
type Rule struct {
	ID string
	Fn func(ActionDescriptor) (Classification, error)
}

// RuleClassifier is a Classifier that walks an ordered list of
// Rules. Ordering encodes precedence:
//
//  1. Dangerous-pattern rules first -- any match short-circuits with
//     a high/critical-tier deny verdict.
//  2. Allowlist / safe-binary rules last -- they fire only when no
//     dangerous-pattern rule did, giving a low-tier "this is fine"
//     verdict that skips the LLM layer (#230).
//  3. If every rule returns `ErrNoClassifierOpinion`, the classifier
//     itself returns `ErrNoClassifierOpinion` so the enclosing
//     `ChainClassifier` advances to the next link (typically the LLM
//     in #230). This is the "no opinion, escalate" path -- NOT an
//     error.
//
// Source is stamped `SourceRule`; if a rule omits Reason, the rule
// ID is used so audit always has SOMETHING to grep on.
type RuleClassifier struct {
	rules []Rule
}

// NewRuleClassifier returns a RuleClassifier over the given rules in
// priority order. A zero-rule classifier always escalates.
func NewRuleClassifier(rules ...Rule) *RuleClassifier {
	return &RuleClassifier{rules: rules}
}

// Classify walks the rules. Latency on the returned classification
// covers the whole walk (so observability sees total rule-layer
// cost, not per-rule).
func (c *RuleClassifier) Classify(_ context.Context, desc ActionDescriptor) (Classification, error) {
	start := time.Now()
	for _, r := range c.rules {
		cls, err := r.Fn(desc)
		if errors.Is(err, ErrNoClassifierOpinion) {
			continue
		}
		if err != nil {
			return Classification{LatencyMs: since(start)}, err
		}
		cls.Source = SourceRule
		cls.RuleID = r.ID
		if cls.Reason == "" {
			cls.Reason = r.ID
		}
		cls.LatencyMs = since(start)
		return cls, nil
	}
	return Classification{LatencyMs: since(start)}, ErrNoClassifierOpinion
}

// DefaultRules returns the curated ruleset shipped with the package.
// Order is significant -- dangerous-pattern rules MUST come before
// the safe-binary / workbench-allowlist rules so a dangerous
// `rm -rf /` doesn't get fast-pathed by a benign leading-binary
// check on a multi-segment command.
//
// The webhook-host allowlist rule is NOT included here -- it's
// parameterized by engine config (the allowed hosts) and gets added
// at gate-construction time via `NewWebhookHostAllowlistRule`. See
// #229 for the wiring.
func DefaultRules() []Rule {
	return []Rule{
		// Shell dangerous-pattern rules. Match on the raw command
		// string so subshells / backticks don't smuggle inner
		// commands past us.
		{ID: "shell.destructive", Fn: ruleShellDestructive},
		{ID: "shell.remote_code", Fn: ruleShellRemoteCode},
		{ID: "shell.privesc", Fn: ruleShellSudo},
		{ID: "shell.chmod_system", Fn: ruleShellChmodSystem},
		{ID: "shell.persistence", Fn: ruleShellShellRCWrite},
		{ID: "shell.system_write", Fn: ruleShellSystemWrite},
		{ID: "shell.credential_access", Fn: ruleShellCredentialRead},
		{ID: "shell.history_clear", Fn: ruleShellHistoryClear},
		{ID: "shell.subshell_inject", Fn: ruleShellSubshellInject},
		// URL rules (http_fetch + webhook).
		{ID: "http.userinfo", Fn: ruleHTTPUserinfo},
		{ID: "http.local_network", Fn: ruleHTTPLocalNetwork},
		// Path rules (fs_read / fs_write).
		{ID: "fs.read_sensitive", Fn: ruleFSReadSensitive},
		{ID: "fs.write_system", Fn: ruleFSWriteSystem},
		// Workbench strict allowlist + safe-binary fast-path. Last
		// so a dangerous pattern above wins on the same command.
		{ID: "shell.workbench_allowlist", Fn: ruleShellWorkbenchAllowlist},
		{ID: "shell.safe_binary", Fn: ruleShellSafeBinary},
	}
}
