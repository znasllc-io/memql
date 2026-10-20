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

// extractCommandBinaries uses the shared shell segmentation rules so quoted
// program text is never mistaken for another command. Leading binary tokens
// retain the existing restrictions: no quote unwrapping or assignment skipping.
func extractCommandBinaries(cmd string) ([]string, error) {
	if strings.TrimSpace(cmd) == "" {
		return nil, fmt.Errorf("command is empty")
	}
	if safety.ContainsSubshell(cmd) {
		return nil, fmt.Errorf("command_not_allowed: subshells and heredocs cannot be verified by the workbench exec allowlist")
	}
	var out []string
	for _, seg := range safety.SegmentCommand(cmd) {
		if first := firstToken(seg); first != "" {
			out = append(out, first)
		}
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
