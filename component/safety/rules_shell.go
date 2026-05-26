package safety

import (
	"fmt"
	"regexp"
)

// Shell-command rules. Each is high-precision: a match should be a
// near-certain block. False positives surface as user-visible
// refusals; false negatives surface in the LLM layer (#230) and the
// red-team corpus (#235). Patterns are scoped to ActionExec on any
// surface; non-exec descriptors return ErrNoClassifierOpinion.

// matchAny returns true if any of the regexps matches s. Cheap
// helper -- regex compilation happens once at init.
func matchAny(s string, res ...*regexp.Regexp) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// ----- destructive -----

var (
	// `rm -rf /`, `rm -fr /`, `rm -rf /*`, `rm -rf ~`, `rm -rf $HOME`,
	// `rm -Rf /Users/...`. The flag chunk requires both r/R and f/F to
	// appear (in either order, possibly mixed with other letters) so a
	// benign `rm -f file` doesn't match.
	rmRfRootRe = regexp.MustCompile(
		`\brm\s+(?:-[a-zA-Z]*[rR][a-zA-Z]*[fF][a-zA-Z]*|-[a-zA-Z]*[fF][a-zA-Z]*[rR][a-zA-Z]*)\b[^|;&\n]*?\s+(?:/(?:\s|$|\*)|~(?:/|\s|$)|\$\{?HOME\}?(?:/|\s|$)|/(?:bin|etc|usr|var|lib|opt|home|root|Users|System|Library)(?:/|\s|$))`,
	)
	// `dd of=/dev/sda`, `dd if=... of=/dev/...`. Any /dev/ target is
	// a near-certain disk-corruption op.
	ddDevRe = regexp.MustCompile(`\bdd\b[^|;&\n]*\bof=/dev/`)
	// `mkfs`, `mkfs.ext4`, `mkfs.xfs`, etc.
	mkfsRe = regexp.MustCompile(`\bmkfs(?:\.\w+)?\b`)
	// Canonical bash fork bomb -- exact-shape regex; matches the
	// literal `:(){ :|:& };:` with flexible whitespace.
	forkBombRe = regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`)
)

func ruleShellDestructive(desc ActionDescriptor) (Classification, error) {
	if desc.Action != ActionExec {
		return Classification{}, ErrNoClassifierOpinion
	}
	cmd := desc.Payload.Command
	switch {
	case rmRfRootRe.MatchString(cmd):
		return cls(TierCritical, "rm -rf of a root/home/system path", CategoryDestructive), nil
	case ddDevRe.MatchString(cmd):
		return cls(TierCritical, "dd writing to /dev/* (disk corruption)", CategoryDestructive), nil
	case mkfsRe.MatchString(cmd):
		return cls(TierCritical, "mkfs filesystem format", CategoryDestructive), nil
	case forkBombRe.MatchString(cmd):
		return cls(TierCritical, "fork bomb pattern", CategoryDestructive), nil
	}
	return Classification{}, ErrNoClassifierOpinion
}

// ----- remote_code -----

var (
	// `curl ... | sh|bash|...`, `wget ... | sudo bash`, etc.
	// Also catches `curl ... | sh -s --` and `eval "$(curl ...)"`.
	pipeToShellRe   = regexp.MustCompile(`\b(?:curl|wget|fetch)\b[^|;&\n]*\|\s*(?:sudo\s+)?(?:bash|sh|zsh|ksh|csh|tcsh|fish|dash|ash)\b`)
	evalCurlRe      = regexp.MustCompile(`\beval\b[^|;&\n]*\$\(\s*(?:curl|wget|fetch)\b`)
	bashCSubshellRe = regexp.MustCompile(`\b(?:bash|sh|zsh|ksh|dash)\s+(?:-[a-zA-Z]+\s+)*-c\b\s*['"]?\s*\$\(\s*(?:curl|wget|fetch)\b`)
)

func ruleShellRemoteCode(desc ActionDescriptor) (Classification, error) {
	if desc.Action != ActionExec {
		return Classification{}, ErrNoClassifierOpinion
	}
	cmd := desc.Payload.Command
	if matchAny(cmd, pipeToShellRe, evalCurlRe, bashCSubshellRe) {
		return cls(TierCritical,
			"network fetch piped to a shell (remote code execution)",
			CategoryRemoteCode), nil
	}
	return Classification{}, ErrNoClassifierOpinion
}

// ----- privilege_escalation -----

var (
	// Standalone `sudo` (any form). The presence of sudo means the
	// agent is asking for elevation -- always at least High.
	sudoRe = regexp.MustCompile(`(?:^|[\s;|&])sudo\b`)
	// `su -`, `su root`, `su - root` -- substitute-user invocation.
	suUserRe = regexp.MustCompile(`(?:^|[\s;|&])su\s+(?:-|root\b|[a-zA-Z_][a-zA-Z0-9_-]*\s*$)`)
	// `chown root` / `chown -R root:wheel /tmp/...` -- making
	// agent-owned files root-owned is a classic priv-esc setup.
	chownRootRe = regexp.MustCompile(`\bchown\b[^|;&\n]*\broot\b`)
)

func ruleShellSudo(desc ActionDescriptor) (Classification, error) {
	if desc.Action != ActionExec {
		return Classification{}, ErrNoClassifierOpinion
	}
	cmd := desc.Payload.Command
	switch {
	case sudoRe.MatchString(cmd):
		return cls(TierHigh, "sudo invocation (privilege elevation)", CategoryPrivilegeEscalation), nil
	case suUserRe.MatchString(cmd):
		return cls(TierHigh, "su user-switch", CategoryPrivilegeEscalation), nil
	case chownRootRe.MatchString(cmd):
		return cls(TierHigh, "chown to root", CategoryPrivilegeEscalation), nil
	}
	return Classification{}, ErrNoClassifierOpinion
}

// ----- persistence (shell-rc + ssh authorized_keys + cron) -----

var (
	// Redirection (`>`/`>>`/`&>`/`|&`) or `tee` writing to a
	// shell-rc / SSH-config / authorized_keys path. The redirection
	// detection requires a `>` followed by the path within the same
	// segment -- not perfect (a heredoc named `~/.bashrc` would
	// match) but high-precision in the common cases.
	shellRCWriteRe = regexp.MustCompile(`(?:>\s*|>>\s*|\btee\b\s+(?:-[a-zA-Z]+\s+)*)(?:~|\$\{?HOME\}?)(?:/\.(?:bash(?:rc|_profile|_login|_logout)|zshrc|zprofile|zlogin|profile|kshrc|cshrc|tcshrc|fishrc|inputrc|netrc|pgpass|aws/credentials)\b|/\.ssh/(?:authorized_keys|config|known_hosts)\b)`)
	// Writing to crontab / cron.d / cron.hourly etc.
	cronWriteRe = regexp.MustCompile(`(?:>\s*|>>\s*|\btee\b\s+(?:-[a-zA-Z]+\s+)*)(?:/etc/cron\.[a-z]+/|/var/spool/cron/)`)
	// `crontab -` (install crontab from stdin) or piping into it.
	crontabPipeRe = regexp.MustCompile(`\|\s*crontab\s+-`)
	// macOS persistence -- writes to LaunchAgents / LaunchDaemons
	// plist directories.
	launchPersistRe = regexp.MustCompile(`(?:>\s*|>>\s*|\btee\b\s+(?:-[a-zA-Z]+\s+)*|\bcp\b\s+[^|;&\n]+\s+)(?:~|\$\{?HOME\}?|/)(?:/Library/Launch(?:Agents|Daemons))/`)
)

func ruleShellShellRCWrite(desc ActionDescriptor) (Classification, error) {
	if desc.Action != ActionExec {
		return Classification{}, ErrNoClassifierOpinion
	}
	cmd := desc.Payload.Command
	switch {
	case shellRCWriteRe.MatchString(cmd):
		return cls(TierHigh,
			"write to shell-rc / SSH authorized_keys (persistence)",
			CategoryPersistence, CategoryCredentialAccess), nil
	case cronWriteRe.MatchString(cmd), crontabPipeRe.MatchString(cmd):
		return cls(TierHigh,
			"write to crontab / cron.d (persistence)",
			CategoryPersistence), nil
	case launchPersistRe.MatchString(cmd):
		return cls(TierHigh,
			"write to LaunchAgents / LaunchDaemons (persistence)",
			CategoryPersistence), nil
	}
	return Classification{}, ErrNoClassifierOpinion
}

// ----- system writes (/etc, /usr/bin, /System, /Library) -----

var (
	// Redirection or `cp` / `mv` / `tee` writing to a system path.
	// Note: `/etc/cron.*` and `/Library/Launch*` paths overlap with
	// the persistence rule -- ordering in DefaultRules() handles it
	// (persistence runs first), so we don't need negative lookahead
	// (which Go's RE2 doesn't support).
	systemWriteRe = regexp.MustCompile(`(?:>\s*|>>\s*|\btee\b\s+(?:-[a-zA-Z]+\s+)*|\b(?:cp|mv)\b\s+[^|;&\n]+\s+)(?:/(?:etc/|usr/bin/|usr/sbin/|usr/local/bin/|usr/local/sbin/|System/|Library/))`)
)

func ruleShellSystemWrite(desc ActionDescriptor) (Classification, error) {
	if desc.Action != ActionExec {
		return Classification{}, ErrNoClassifierOpinion
	}
	if systemWriteRe.MatchString(desc.Payload.Command) {
		return cls(TierHigh,
			"write to system path (/etc, /usr/bin, /System, /Library)",
			CategoryPrivilegeEscalation, CategoryPersistence), nil
	}
	return Classification{}, ErrNoClassifierOpinion
}

// ----- credential read -----

var (
	// `cat ~/.ssh/id_*`, `grep -r ... ~/.aws/credentials`, etc. We
	// match a read-y binary followed (within the same segment) by a
	// sensitive path. False positives on benign tooling that
	// references these paths in args (rare) are accepted.
	credReadRe  = regexp.MustCompile(`\b(?:cat|head|tail|less|more|grep|awk|sed|cp|mv|scp|rsync|tar|zip|base64|xxd|hexdump|od)\b[^|;&\n]*\s(?:~|\$\{?HOME\}?|/root|/home/[^/\s]+)/(?:\.ssh/id_|\.ssh/identity|\.aws/credentials|\.gnupg/|\.config/gh/hosts|\.docker/config\.json|\.npmrc|\.pypirc|\.netrc)`)
	etcShadowRe = regexp.MustCompile(`\b(?:cat|head|tail|less|more|grep|awk|sed|cp|mv|tar|base64|xxd|hexdump|od)\b[^|;&\n]*\s/etc/(?:shadow|gshadow|sudoers)\b`)
)

func ruleShellCredentialRead(desc ActionDescriptor) (Classification, error) {
	if desc.Action != ActionExec {
		return Classification{}, ErrNoClassifierOpinion
	}
	cmd := desc.Payload.Command
	if matchAny(cmd, credReadRe, etcShadowRe) {
		return cls(TierHigh,
			"reading credential-bearing file (~/.ssh, ~/.aws, /etc/shadow, …)",
			CategoryCredentialAccess), nil
	}
	return Classification{}, ErrNoClassifierOpinion
}

// ----- safe-binary fast-path -----

// ruleShellSafeBinary fires when ALL segment binaries are in the
// safe read-only set (see `safeReadOnlyBinaries`) AND the command
// has no subshell / heredoc. Returns tier=low so the LLM doesn't
// see the common case (`ls`, `cat`, `grep`, …). Conservative: a
// single non-safe segment skips this rule and falls through.
func ruleShellSafeBinary(desc ActionDescriptor) (Classification, error) {
	if desc.Action != ActionExec {
		return Classification{}, ErrNoClassifierOpinion
	}
	cmd := desc.Payload.Command
	if ContainsSubshell(cmd) {
		return Classification{}, ErrNoClassifierOpinion
	}
	bins := CommandBinaries(cmd)
	if len(bins) == 0 {
		return Classification{}, ErrNoClassifierOpinion
	}
	for _, b := range bins {
		if _, ok := safeReadOnlyBinaries[b]; !ok {
			return Classification{}, ErrNoClassifierOpinion
		}
	}
	return cls(TierLow, "command uses only read-only utilities"), nil
}

// ----- workbench strict allowlist -----

// ruleShellWorkbenchAllowlist enforces the workbench's allowlist as a
// classification verdict: any exec on Surface=workbench must
// segment cleanly (no subshells) and have every segment's leading
// binary in `WorkbenchExecAllowlist`. Subshells fail-deny here
// because the outer-binary check can't verify what an inner
// substitution would run -- the workbench can't risk smuggling.
//
// Note: this is the FUTURE enforcement path; until the gate is
// wired (#229), the legacy `integrations/workbench/exec_allowlist.go`
// helper still does the actual blocking. Both read from the same
// `WorkbenchExecAllowlist` set, so they agree.
func ruleShellWorkbenchAllowlist(desc ActionDescriptor) (Classification, error) {
	if desc.Action != ActionExec || desc.Surface != SurfaceWorkbench {
		return Classification{}, ErrNoClassifierOpinion
	}
	cmd := desc.Payload.Command
	if ContainsSubshell(cmd) {
		return cls(TierHigh,
			"workbench exec contains a subshell/heredoc -- cannot verify allowlist",
			CategorySandboxEscape), nil
	}
	bins := CommandBinaries(cmd)
	if len(bins) == 0 {
		return Classification{}, ErrNoClassifierOpinion
	}
	for _, b := range bins {
		if _, ok := WorkbenchExecAllowlist[b]; !ok {
			return cls(TierHigh,
				fmt.Sprintf("workbench exec binary %q is not on the workbench allowlist", b),
				CategorySandboxEscape), nil
		}
	}
	// All segments are in the allowlist + no subshell -> low tier
	// (covers the common case of allowlisted binaries doing benign
	// work). Dangerous-pattern rules above already caught any
	// abuse of allowlisted binaries (e.g. `rm -rf /`).
	return cls(TierLow, "workbench exec is within the allowlist"), nil
}

// cls fills in the always-set Classification fields (Source +
// Confidence) and lets per-rule sites focus on tier + reason + the
// categories that make this verdict steerable by the decision policy.
// RuleID is filled in by RuleClassifier on the way out.
func cls(tier RiskTier, reason string, cats ...Category) Classification {
	return Classification{
		Tier:       tier,
		Categories: cats,
		Reason:     reason,
		Confidence: 1.0,
		Source:     SourceRule,
	}
}
