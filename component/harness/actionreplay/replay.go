// Package actionreplay implements Phase 1 literal replay for the action
// library (#1736, epic #1734): re-executing the concrete capability calls a
// minted action recorded, with no LLM, keyed on an input fingerprint and
// gated by a result fingerprint.
//
// It is deliberately pure of engine/DB types: the harness supplies a
// ToolInvoker (engine.ExecuteToolByName) and the recorded calls, and this
// package re-executes them and reports whether the replay reproduced the
// recorded result. Trust math (reinforce) lives here too so it is unit
// testable; the harness hands the computed values to reinforceAction.
package actionreplay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ReplayCall is one capability call to re-execute on replay, with the literal
// recorded args. Phase 1 replays args verbatim; automatic parameterization
// (binding args from the step input) lands in Phase 3 (#1738).
type ReplayCall struct {
	Index      int            `json:"index"`
	Capability string         `json:"capability"`
	Args       map[string]any `json:"args"`
}

// ToolInvoker is the no-LLM capability invocation surface. *memql.MemQLEngine
// satisfies it via ExecuteToolByName -- the lowest-level "run this tool with
// these args" call, which is exactly a single capability invocation on the
// default surface.
type ToolInvoker interface {
	ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error)
}

// reinforceGain is the fraction of the remaining gap to 1.0 that a verified
// replay adds to reliability (mirrors the semanticMemory reinforce shape).
const reinforceGain = 0.34

// Fingerprint returns a stable hex digest of v's canonical JSON. encoding/json
// sorts map keys, so two equal values hash equal regardless of construction
// order -- which is what makes the input fingerprint a reliable replay key and
// the result fingerprint a reliable verification gate.
func Fingerprint(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// A marshal failure is not security-sensitive here; fall back to a
		// deterministic-enough formatting so we still produce a stable key.
		b = []byte(fmt.Sprintf("%#v", v))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Replay re-executes each call in order via the invoker (no LLM) and returns
// the ordered results plus a fingerprint over them. The first call error
// aborts the replay with a non-nil error so the caller falls back to the LLM;
// a partial replay never returns a result fingerprint.
func Replay(ctx context.Context, invoker ToolInvoker, calls []ReplayCall) (results []any, resultFingerprint string, err error) {
	if invoker == nil {
		return nil, "", fmt.Errorf("actionreplay: nil invoker")
	}
	results = make([]any, 0, len(calls))
	for _, c := range calls {
		out, callErr := invoker.ExecuteToolByName(ctx, c.Capability, c.Args)
		if callErr != nil {
			return nil, "", fmt.Errorf("actionreplay: call %d (%s): %w", c.Index, c.Capability, callErr)
		}
		// Decode the tool result JSON so the fingerprint is structural rather
		// than string-formatting-sensitive; fall back to the raw string.
		var decoded any
		if json.Unmarshal([]byte(out), &decoded) == nil {
			results = append(results, decoded)
		} else {
			results = append(results, out)
		}
	}
	return results, Fingerprint(results), nil
}

// Verify reports whether a replay reproduced the recorded result. An empty
// recorded fingerprint is unverifiable (false) so we never reinforce an action
// that has no baseline to check against.
func Verify(recordedFingerprint, replayFingerprint string) bool {
	return recordedFingerprint != "" && recordedFingerprint == replayFingerprint
}

// Reinforce computes the next (reliability, reinforceCount) after a verified
// successful replay: reliability rises toward 1.0 by a fixed fraction of the
// remaining gap, reinforceCount increments. Pure; the harness persists the
// result via reinforceAction (the MemQL parser has no arithmetic).
func Reinforce(reliability float64, reinforceCount int) (float64, int) {
	next := reliability + (1.0-reliability)*reinforceGain
	if next > 1.0 {
		next = 1.0
	}
	if next < 0 {
		next = 0
	}
	return next, reinforceCount + 1
}
