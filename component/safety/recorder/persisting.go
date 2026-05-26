// Package recorder is the persistence side of the safety command-
// classifier observability layer (memql#234). PersistingRecorder
// implements safety.DecisionRecorder by writing each gated action to
// the v1:safety:classification concept via the
// mutationInsertSafetyClassification DSL mutation.
//
// Architectural seam: this package imports `component/safety` (for
// types) but NOT `component/memql` -- the engine is reached via the
// narrow MutationRunner interface, which app boot adapts from
// memql.MemQLEngine.Execute. Keeps the recorder testable against an
// in-memory stub and avoids a cycle with `component/memql/tool_execution.go`,
// which imports `component/safety`.
package recorder

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/safety"
)

// MutationRunner is the narrow surface PersistingRecorder needs to
// insert classification rows. One method, no engine types -- app
// boot adapts memql.MemQLEngine.Execute into this; tests pass an
// in-memory stub.
type MutationRunner interface {
	RunMutation(ctx context.Context, query string) error
}

// PersistingRecorder writes one v1:safety:classification row per
// Gate.Evaluate decision via the mutationInsertSafetyClassification
// DSL mutation.
//
// Errors from the runner are logged + swallowed -- a persistence
// failure must NOT block the dispatch path (the recorder contract
// is best-effort; the slog sink running alongside in a FanoutRecorder
// already captures the data for operator grep on the failed path).
//
// Concurrency: safe for concurrent use. The runner is invoked
// synchronously on the dispatch path; if persistence latency
// becomes a problem, future work can push the call into a buffered
// worker -- the recorder contract permits async sinks.
type PersistingRecorder struct {
	runner MutationRunner
	logger *slog.Logger
}

// New returns a PersistingRecorder. A nil runner makes Record a
// no-op (safe to fan into). A nil logger falls back to
// slog.Default.
func New(runner MutationRunner, logger *slog.Logger) *PersistingRecorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &PersistingRecorder{runner: runner, logger: logger}
}

// Record persists one classification decision. The payload is
// passed through safety.RedactedPayload BEFORE marshalling, so
// secrets in args / bodies never reach the persisted row -- mirrors
// the slog-sink redaction guarantee.
func (p *PersistingRecorder) Record(ctx context.Context, desc safety.ActionDescriptor, cls safety.Classification, decision safety.Decision, mode safety.Mode) {
	if p == nil || p.runner == nil {
		return
	}
	query := buildMutationQuery(buildArgs(desc, cls, decision, mode))
	if err := p.runner.RunMutation(ctx, query); err != nil {
		p.logger.Warn("safety/recorder: persistence failed",
			"component", "safety.persisting_recorder",
			"rule_id", cls.RuleID,
			"decision", string(decision),
			"surface", string(desc.Surface),
			"action", string(desc.Action),
			"error", err)
	}
}

// buildArgs translates recorder inputs into the args map for the
// mutationInsertSafetyClassification call. Categories collapse to a
// comma-separated string (matches the concept schema -- the array
// shape isn't worth the per-row overhead until the cockpit needs
// it). All string-typed fields stay strings; numeric fields stay
// numeric so the mutation arg-coercion path doesn't have to parse.
//
// Defensive credential scrub: safety.RedactedPayload's per-field
// guarantee is "Body + Args are scrubbed; Command/URL/Paths/Method/
// ToolName are pass-through." When the classifier flagged
// credential_access (URL with userinfo, env-var assignment in a
// shell command, etc.) those surface strings almost certainly carry
// the credential the rule fired on. We don't want to persist that
// even in shadow-mode telemetry, so we drop the surface strings
// from the recorded payload when credential_access is in the
// category set. The rule's `reason` field still ships, which is
// enough context for triage without leaking the credential. Other
// categories (destructive, etc.) leave Command intact -- the
// classifier didn't flag the command as credential-bearing.
func buildArgs(desc safety.ActionDescriptor, cls safety.Classification, decision safety.Decision, mode safety.Mode) map[string]any {
	red := safety.RedactedPayload(desc.Payload)
	if hasCategory(cls.Categories, safety.CategoryCredentialAccess) {
		red.Command = "[REDACTED:credential_access]"
		red.URL = "[REDACTED:credential_access]"
		red.Paths = nil
	}
	redactedJSON, _ := json.Marshal(red)
	cats := make([]string, 0, len(cls.Categories))
	for _, c := range cls.Categories {
		cats = append(cats, string(c))
	}
	return map[string]any{
		"surface":       string(desc.Surface),
		"action":        string(desc.Action),
		"argsRedacted":  string(redactedJSON),
		"tier":          cls.Tier.String(),
		"categories":    strings.Join(cats, ","),
		"decision":      string(decision),
		"source":        string(cls.Source),
		"ruleId":        cls.RuleID,
		"confidence":    cls.Confidence,
		"reason":        cls.Reason,
		"latencyMs":     cls.LatencyMs,
		"agentId":       desc.Caller.AgentID,
		"ownerUserId":   desc.Caller.OwnerUserID,
		"planId":        desc.Caller.PlanID,
		"correlationId": desc.Caller.CorrelationID,
		"mode":          string(mode),
	}
}

// hasCategory reports whether `target` appears in `cats`. Tiny
// helper so the credential-access defensive-scrub branch in
// buildArgs reads at the call site.
func hasCategory(cats []safety.Category, target safety.Category) bool {
	for _, c := range cats {
		if c == target {
			return true
		}
	}
	return false
}

// buildMutationQuery composes the memql mutation call string. Keys
// are emitted as bare identifiers (memql syntax), values as JSON
// (memql object literals share the JSON literal syntax for
// strings/numbers/bools/null). Keys are sorted so the query string
// is deterministic for tests + log readability.
func buildMutationQuery(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("mutationInsertSafetyClassification({")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString(": ")
		v, _ := json.Marshal(args[k])
		b.Write(v)
	}
	b.WriteString("})")
	return b.String()
}
