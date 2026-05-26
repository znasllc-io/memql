package safety

import "regexp"

// rules_shell_extra.go ships rules surfaced by the memql#235
// red-team corpus that weren't covered by #228's initial set.
// Each rule is here (not in rules_shell.go) so the corpus-driven
// additions stay greppable + cleanly attributable to the rollout
// pass.

var (
	// chmod against a system path. Mirrors systemWriteRe but covers
	// mode-bit changes against /etc, /usr/bin, /System, /Library --
	// `chmod 777 /etc/passwd` is the classic priv-esc opener that
	// systemWriteRe (which only covers redirects + tee + cp/mv)
	// misses.
	chmodSystemRe = regexp.MustCompile(`\bchmod\s+(?:-[a-zA-Z]+\s+)*(?:[0-7]{3,4}|[ugoa]*[+=-][rwxst]+)\s+/(?:etc/|usr/bin/|usr/sbin/|usr/local/bin/|usr/local/sbin/|System/|Library/)`)

	// history -c clears the shell audit trail. Any agent that runs
	// this is almost certainly trying to hide what came next.
	// Single-purpose pattern; high confidence.
	historyClearRe = regexp.MustCompile(`\bhistory\s+-c\b`)

	// Subshell-bearing commands ($(...), backticks, <(...) process
	// subst, heredoc <<). The contained command is unanalysable by
	// the rule engine -- could exec anything. Tag Medium-tier so the
	// #231 matrix routes to Ask (not Deny -- some legitimate uses
	// exist; user can approve via #232). Distinct from the
	// workbench-allowlist's subshell rejection (which fires only on
	// the workbench surface).
	subshellRe = regexp.MustCompile(`(?:\$\(|` + "`" + `[^` + "`" + `]+` + "`" + `|<\()`)
)

// ruleShellChmodSystem fires on chmod against /etc, /usr/bin, etc.
func ruleShellChmodSystem(desc ActionDescriptor) (Classification, error) {
	if desc.Action != ActionExec {
		return Classification{}, ErrNoClassifierOpinion
	}
	if chmodSystemRe.MatchString(desc.Payload.Command) {
		return cls(TierHigh,
			"chmod against system path (/etc, /usr/bin, /System, /Library)",
			CategoryPrivilegeEscalation), nil
	}
	return Classification{}, ErrNoClassifierOpinion
}

// ruleShellHistoryClear fires on `history -c`.
func ruleShellHistoryClear(desc ActionDescriptor) (Classification, error) {
	if desc.Action != ActionExec {
		return Classification{}, ErrNoClassifierOpinion
	}
	if historyClearRe.MatchString(desc.Payload.Command) {
		return cls(TierHigh,
			"history -c clears the shell audit trail",
			CategoryPersistence), nil
	}
	return Classification{}, ErrNoClassifierOpinion
}

// ruleShellSubshellInject fires when a command contains a subshell
// the rule engine can't analyse. Routes to Ask via the #231 matrix
// (Medium tier) -- some legitimate use exists (a power user
// shell-scripting in the workbench), so default-deny would block
// too much. Operator approves via the #232 approval flow when
// they trust the source.
//
// CATEGORY CHOICE: deliberately NOT CategoryRemoteCode here even
// though a subshell can carry remote-code. CategoryRemoteCode is a
// FLOOR category in decision_policy.go::effectiveTier -- it would
// promote our TierMedium to TierCritical, which the
// DefaultDecisionPolicy maps to DecisionDeny, bypassing the
// approval flow this rule docstring promises. CategoryUnknown
// keeps the rule at TierMedium so the matrix routes to Ask
// (computer-use surface) / Allow-with-interact-scope (workbench),
// honouring the intent: 'we can't classify this -- escalate to
// the human + approval flow rather than blanket deny.'
func ruleShellSubshellInject(desc ActionDescriptor) (Classification, error) {
	if desc.Action != ActionExec {
		return Classification{}, ErrNoClassifierOpinion
	}
	if subshellRe.MatchString(desc.Payload.Command) {
		return cls(TierMedium,
			"command contains a subshell ($(...), backticks, or <(...)) that bypasses rule-based analysis",
			CategoryUnknown), nil
	}
	return Classification{}, ErrNoClassifierOpinion
}
