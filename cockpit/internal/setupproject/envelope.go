package setupproject

import (
	"encoding/json"
	"fmt"
	"strings"
)

// envelope.go is the cockpit-side reader for the capability-script result
// envelope that scripts/bootstrap.sh emits on stdout.
//
// The canonical contract and the reference parser live in the engine repo:
//   - memql/docs/internal/design/capability-script-contract.md
//   - memql/component/deploycontrol/capability_result.go (ParseCapabilityResult)
//
// We keep a small local copy of the type + parser rather than importing
// deploycontrol.ParseCapabilityResult: that function lives in a package that
// drags the engine's whole runtime graph (component/memql, grpc/gen, identity,
// ...) into its importers. This scaffolding command only needs os/exec + git,
// so a ~15-line local parser keeps it decoupled and trivially testable. The
// shape below MUST stay byte-compatible with the contract.

// Envelope is the JSON result object every capability script emits on stdout.
type Envelope struct {
	// OK is true on success; it always equals (exit code == 0).
	OK bool `json:"ok"`
	// Capability is the capability id the script declared via cap_init.
	Capability string `json:"capability"`
	// Changed reports whether this (idempotent) run actually mutated state.
	Changed bool `json:"changed"`
	// Result carries the capability-specific fields. Left raw so callers decode
	// the shape they expect (BootstrapResult here).
	Result json.RawMessage `json:"result"`
	// Error is populated only on failure.
	Error *EnvelopeError `json:"error"`
}

// EnvelopeError is the failure detail in a result envelope. Code mirrors the
// script's exit code (see the contract's standard codes).
type EnvelopeError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// BootstrapResult is the capability-specific result payload scripts/bootstrap.sh
// puts in Envelope.Result. Fields mirror emit_result() in the template repo's
// bootstrap.sh.
type BootstrapResult struct {
	Product       string   `json:"product"`
	ProductOrg    string   `json:"productOrg"`
	Domain        string   `json:"domain"`
	EngineVersion string   `json:"engineVersion"`
	WorkspaceRoot string   `json:"workspaceRoot"`
	StampedRepos  []string `json:"stampedRepos"`
	DryRun        bool     `json:"dryRun"`
}

// ParseEnvelope extracts the single JSON result envelope from a capability
// script's stdout. Per the contract the envelope is the only JSON object on
// stdout; to be robust against a trailing newline (and only that) it reads the
// LAST line that looks like a JSON object. It returns an error when stdout
// carries no envelope or it is malformed; a well-formed envelope with OK=false
// is returned WITHOUT a Go error so the caller can inspect Error (use Err()).
func ParseEnvelope(stdout []byte) (Envelope, error) {
	line := lastJSONObjectLine(string(stdout))
	if line == "" {
		return Envelope{}, fmt.Errorf("no JSON result envelope on stdout (capability scripts must emit one; got %d bytes)", len(stdout))
	}
	var e Envelope
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return Envelope{}, fmt.Errorf("malformed capability result envelope: %w", err)
	}
	return e, nil
}

// DecodeResult decodes the capability-specific result payload. A zero value is
// returned (no error) when Result is absent.
func (e Envelope) DecodeResult() (BootstrapResult, error) {
	var r BootstrapResult
	if len(e.Result) == 0 {
		return r, nil
	}
	if err := json.Unmarshal(e.Result, &r); err != nil {
		return r, fmt.Errorf("decode bootstrap result payload: %w", err)
	}
	return r, nil
}

// Err returns a non-nil error when the envelope reported failure (OK=false),
// surfacing the script's structured error.message + code. Returns nil on
// success.
func (e Envelope) Err() error {
	if e.OK {
		return nil
	}
	if e.Error != nil {
		return fmt.Errorf("capability %q failed (exit %d): %s", e.Capability, e.Error.Code, e.Error.Message)
	}
	return fmt.Errorf("capability %q failed", e.Capability)
}

// lastJSONObjectLine returns the last line of s that looks like a single JSON
// object (`{...}`). Capability stdout is the envelope alone, so this tolerates
// only a trailing newline -- it does not salvage interleaved prose, which the
// contract forbids on stdout in the first place.
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
