package recorder

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/safety"
)

// OutputPersistingRecorder writes one v1:safety:outputScreening
// row per OutputGate.Screen verdict via the DSL mutation
// `mutationInsertOutputScreening`. Mirrors PersistingRecorder
// (memql#234) but for the output-screening half of the system
// (memql#233).
//
// Why a parallel type rather than reusing PersistingRecorder:
// the two recorders implement different interfaces
// (DecisionRecorder vs ScreeningRecorder) -- the parameter shapes
// don't line up (one carries ActionDescriptor + Classification +
// Decision, the other carries ScreeningInput + ScreeningResult +
// OutputMode). Single shared implementation would need a translator
// at every call site; parallel structures stay decoupled and
// readable.
//
// Errors from the runner are logged + swallowed -- a persistence
// failure must NOT block the ingress path (the slog sink running
// alongside in a fanout already captures the data for operator
// grep on the failed path). Same posture as PersistingRecorder.
type OutputPersistingRecorder struct {
	runner MutationRunner
	logger *slog.Logger
}

// NewOutputPersistingRecorder returns a recorder bound to the
// mutation runner. A nil runner makes Record a no-op (safe to fan
// into); a nil logger falls back to slog.Default.
func NewOutputPersistingRecorder(runner MutationRunner, logger *slog.Logger) *OutputPersistingRecorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &OutputPersistingRecorder{runner: runner, logger: logger}
}

// MaxRedactedSampleBytes caps the persisted redactedSample so a
// poisoned 100 MB document doesn't blow up the row size. 4 KiB
// matches the concept @description; large enough to capture the
// triggering pattern + surrounding context, small enough that a
// few thousand rows still fit in operator memory.
const MaxRedactedSampleBytes = 4096

// Record persists one screening verdict. Mirrors PersistingRecorder.Record
// but with screening-specific inputs. RedactedSample is the FIRST
// MaxRedactedSampleBytes of content -- the caller doesn't
// pre-truncate because the recorder is the single point that
// knows the policy. Content is NOT passed through RedactArgs since
// it's free-form text (not a map of keyed fields); the truncation
// + the content-type label gives operators enough context to
// inspect what fired the verdict without copy-pasting a multi-MB
// poisoned blob.
func (p *OutputPersistingRecorder) Record(ctx context.Context, in safety.ScreeningInput, res safety.ScreeningResult, mode safety.OutputMode) {
	if p == nil || p.runner == nil {
		return
	}
	query := buildOutputMutationQuery(buildOutputArgs(in, res, mode))
	if err := p.runner.RunMutation(ctx, query); err != nil {
		p.logger.Warn("safety/recorder: output-screening persistence failed",
			"component", "safety.output_persisting_recorder",
			"rule_id", res.RuleID,
			"verdict", string(res.Verdict),
			"content_type", string(in.ContentType),
			"error", err)
	}
}

// buildOutputArgs translates the screening inputs into the
// mutationInsertOutputScreening args map. The redactedSample is
// truncated here (single source of truth for the cap policy).
func buildOutputArgs(in safety.ScreeningInput, res safety.ScreeningResult, mode safety.OutputMode) map[string]any {
	sample := in.Content
	if len(sample) > MaxRedactedSampleBytes {
		sample = sample[:MaxRedactedSampleBytes]
	}
	cats := make([]string, 0, len(res.Categories))
	for _, c := range res.Categories {
		cats = append(cats, string(c))
	}
	return map[string]any{
		"contentType":    string(in.ContentType),
		"contentLength":  float64(len(in.Content)),
		"redactedSample": sample,
		"tier":           res.Tier.String(),
		"categories":     strings.Join(cats, ","),
		"verdict":        string(res.Verdict),
		"screener":       string(res.Source),
		"ruleId":         res.RuleID,
		"confidence":     res.Confidence,
		"reason":         res.Reason,
		"latencyMs":      res.LatencyMs,
		"agentId":        in.Caller.AgentID,
		"ownerUserId":    in.Caller.OwnerUserID,
		"planId":         in.Caller.PlanID,
		"correlationId":  in.Caller.CorrelationID,
		"mode":           string(mode),
	}
}

// buildOutputMutationQuery composes the memql call string. Same
// sorted-key + bare-identifier + JSON-marshalled-value pattern as
// buildMutationQuery in persisting.go.
func buildOutputMutationQuery(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("mutationInsertOutputScreening({")
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
