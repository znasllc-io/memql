// capability_result.go is the Go side of the capability-script contract
// (docs/internal/design/capability-script-contract.md, znasllc-io/memql#2221).
//
// A capability script -- the deterministic shell backend behind a DSL `action`
// -- writes its human logs to stderr and emits EXACTLY ONE JSON result envelope
// on stdout. This file is the canonical parser for that envelope. The action
// executor (#2218 / I8) and any in-process capability invocation parse a
// script's stdout through ParseCapabilityResult instead of scraping prose, so
// the Go effect seam (Executor, executor.go) and the DSL automation layer agree
// on one structured protocol.
//
// This is ADDITIVE: it does not change the Executor interface (which the deploy
// pack reuses, Epic 2 / #2095). It gives capability-script backends a typed
// result the way KubectlJSON already gives reads a typed body.
package deploycontrol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// CapabilityResult is the JSON envelope every capability script emits on stdout.
// See the contract for the field semantics.
type CapabilityResult struct {
	// OK is true on success; it always equals (exit code == 0).
	OK bool `json:"ok"`
	// Capability is the capability id the script declared via cap_init.
	Capability string `json:"capability"`
	// Changed reports whether this (idempotent) run actually mutated state.
	Changed bool `json:"changed"`
	// Result carries the capability-specific fields (digests, paths, counts).
	// Left raw so callers decode the shape they expect.
	Result json.RawMessage `json:"result"`
	// Error is populated only on failure.
	Error *CapabilityError `json:"error"`
}

// CapabilityError is the failure detail in a result envelope. Code mirrors the
// script's exit code (see the contract's standard codes).
type CapabilityError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ParseCapabilityResult extracts the single JSON result envelope from a
// capability script's stdout. Per the contract, the envelope is the only JSON
// object on stdout; to be robust against a stray trailing newline (and only
// that), it reads the LAST non-empty line and requires it to be the envelope.
// It returns an error when stdout carries no JSON envelope or it is malformed;
// a well-formed envelope with OK=false is returned WITHOUT a Go error so the
// caller can inspect Error -- use Err() for the convenience check.
func ParseCapabilityResult(stdout []byte) (CapabilityResult, error) {
	line := lastJSONObjectLine(string(stdout))
	if line == "" {
		return CapabilityResult{}, fmt.Errorf("no JSON result envelope on stdout (capability scripts must emit one; got %d bytes)", len(stdout))
	}
	var r CapabilityResult
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		return CapabilityResult{}, fmt.Errorf("malformed capability result envelope: %w", err)
	}
	return r, nil
}

// Err returns a non-nil error when the capability reported failure (OK=false),
// surfacing the script's structured error.message + code. Returns nil on success.
func (r CapabilityResult) Err() error {
	if r.OK {
		return nil
	}
	if r.Error != nil {
		return fmt.Errorf("capability %q failed (exit %d): %s", r.Capability, r.Error.Code, r.Error.Message)
	}
	return fmt.Errorf("capability %q failed", r.Capability)
}

// lastJSONObjectLine returns the last line of s that looks like a single JSON
// object (`{...}`). Capability stdout is the envelope alone, so this tolerates
// only a trailing newline -- it does not try to salvage interleaved prose,
// which the contract forbids on stdout in the first place.
func lastJSONObjectLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") && strings.HasSuffix(l, "}") {
			return l
		}
	}
	return ""
}
