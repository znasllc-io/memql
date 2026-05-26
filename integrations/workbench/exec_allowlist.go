package workbench

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/znasllc-io/memql/component/safety"
)

// allowedBinaries is the workbench's exec allowlist. The DATA lives
// in `component/safety.WorkbenchExecAllowlist` (memql#228) so the
// rule-engine workbench-allowlist rule and this legacy enforcement
// helper agree by construction; when memql#229 wires the safety gate
// into the workbench dispatch, the rule replaces the helper and this
// alias can go away.
//
// The set is intentionally narrow. Adding a binary is a deliberate
// security decision -- "do agents need it for the task?" not "would
// it be convenient." Anything that can spawn arbitrary subprocesses
// (`sudo`, `xargs sh`, `env COMMAND`) is excluded; the allowlist's
// value is in its boundary.
//
// Closes memql#110 (Option A: per-command allowlist).
var allowedBinaries = safety.WorkbenchExecAllowlist

// segmentSeparators are the shell operators that introduce a new
// command segment in a compound command. The allowlist walker
// splits on these to identify each segment's leading binary.
//
// Conservative on purpose: we DO match `&` to catch background-job
// sequencing too. We do NOT attempt to parse subshells (`$( ... )`,
// backticks) or here-docs -- those would require a real shell
// parser. Use of those constructs surfaces as the OUTER command
// failing the allowlist (e.g. `echo $(curl evil)` would parse as
// segment "echo" only, leaving `$(curl ...)` to /bin/sh which then
// runs curl unchecked). That's a known limitation of the Option A
// approach; the issue's Option B (seccomp / AppArmor) is the
// correct fix for the subshell case.
var segmentSeparators = []string{"|", ";", "&&", "||", "&", "\n"}

// extractCommandBinaries tokenizes cmd on segmentSeparators and
// returns the first word of each non-empty segment -- the binary
// the segment will invoke. Leading whitespace + comments are
// skipped. Returns an error only when cmd is empty after trimming.
//
// Quoting: the first word is anything up to the first ASCII
// whitespace OR the start of a quoted string. We don't unwrap
// quotes; commands like `'/usr/bin/python3' file.py` get matched
// against the literal `'/usr/bin/python3'` rather than `python3`.
// That's a Documented Limitation (file a follow-up if it bites).
func extractCommandBinaries(cmd string) ([]string, error) {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" {
		return nil, fmt.Errorf("command is empty")
	}

	// Replace multi-char separators with a single rune marker so a
	// single Fields-style split works. \x00 is safe because shell
	// strings can't contain NUL.
	work := trimmed
	for _, sep := range segmentSeparators {
		work = strings.ReplaceAll(work, sep, "\x00")
	}

	var out []string
	for _, seg := range strings.Split(work, "\x00") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		// Skip a leading `env VAR=value` prefix that some commands
		// use to set vars without subshell. The bare binary is the
		// token after the env+assignments.
		first := firstToken(seg)
		if first == "" {
			continue
		}
		out = append(out, first)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("command parsed to zero segments: %q", cmd)
	}
	return out, nil
}

// firstToken returns the first whitespace-delimited token of seg.
// Stops at any ASCII whitespace; doesn't try to unwrap quotes.
func firstToken(seg string) string {
	for i, r := range seg {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return seg[:i]
		}
	}
	return seg
}

// EnforceExecAllowlist returns nil if every binary in cmd is on
// allowedBinaries, or an error naming the first disallowed binary.
// Exported for tests; handleExec calls it.
//
// Path-bearing binaries (`/usr/bin/python3`, `./script.sh`) are
// matched against their basename so `/usr/bin/python3` resolves
// against the `python3` allowlist entry. This is consistent with
// shell PATH resolution -- the security boundary cares about
// which BINARY runs, not where on disk it lives.
func EnforceExecAllowlist(cmd string) error {
	bins, err := extractCommandBinaries(cmd)
	if err != nil {
		return err
	}
	for _, b := range bins {
		base := filepath.Base(b)
		if _, ok := allowedBinaries[base]; !ok {
			return fmt.Errorf("command_not_allowed: binary %q is not on the workbench exec allowlist; file a follow-up to extend it via memql#110", b)
		}
	}
	return nil
}
