package safety

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// output_gate.go is the OutputGate -- parallel to gate.go for the
// output-screening half (memql#233). Surfaces (workbench http_fetch,
// agent tool-loop, etc.) call Screen() with incoming content
// BEFORE the model re-ingests it. The Gate's mode controls whether
// a Suspicious/Blocked verdict actually blocks (enforce) or just
// records (shadow; the default during the #235 rollout).

// OutputMode mirrors Mode for the command side. Same three states,
// same env-var-driven default of shadow. Separate type so an
// operator can't accidentally apply the command-classifier mode to
// the output gate (different rollout cadence is expected; output
// screening usually goes to enforce earlier because its FP cost is
// lower -- a blocked tool output is less disruptive than a blocked
// command).
type OutputMode string

const (
	OutputModeOff     OutputMode = "off"
	OutputModeShadow  OutputMode = "shadow"
	OutputModeEnforce OutputMode = "enforce"
)

// EnvOutputMode is the env var that selects the output-gate mode.
// Read at gate construction (NewOutputGate) so changing it requires
// a restart.
const EnvOutputMode = "MEMQL_SAFETY_OUTPUT_SCREEN_MODE"

// OutputModeFromEnv reads EnvOutputMode and returns the corresponding
// mode, defaulting to shadow on empty / unrecognized.
func OutputModeFromEnv() OutputMode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvOutputMode))) {
	case "off":
		return OutputModeOff
	case "enforce":
		return OutputModeEnforce
	default:
		return OutputModeShadow
	}
}

// ScreeningRecorder receives every Screen verdict. The slog-backed
// default ships in this PR; #233's persistence path adds a recorder
// that writes v1:safety:outputScreening rows, fan'd alongside via
// the existing FanoutRecorder shape. Errors MUST NOT block ingress.
type ScreeningRecorder interface {
	Record(ctx context.Context, in ScreeningInput, res ScreeningResult, mode OutputMode)
}

// SlogScreeningRecorder is the default: one structured INFO log
// line per screening verdict. RedactedSample is truncated to a
// short prefix so a 100MB document doesn't blow the log line up.
type SlogScreeningRecorder struct {
	Logger *slog.Logger
}

// Record writes the verdict to the recorder's logger. The content
// sample is truncated to keep log lines bounded; the full
// (truncated + redacted) sample lands on the persisted row.
func (r SlogScreeningRecorder) Record(ctx context.Context, in ScreeningInput, res ScreeningResult, mode OutputMode) {
	if r.Logger == nil {
		return
	}
	cats := make([]string, 0, len(res.Categories))
	for _, c := range res.Categories {
		cats = append(cats, string(c))
	}
	sample := truncateForLog(in.Content, 200)
	r.Logger.LogAttrs(ctx, slog.LevelInfo, "safety: output screened",
		slog.String("component", "safety.output_gate"),
		slog.String("mode", string(mode)),
		slog.String("content_type", string(in.ContentType)),
		slog.Int("content_length", len(in.Content)),
		slog.String("sample", sample),
		slog.String("agent_id", in.Caller.AgentID),
		slog.String("owner_user_id", in.Caller.OwnerUserID),
		slog.String("plan_id", in.Caller.PlanID),
		slog.String("correlation_id", in.Caller.CorrelationID),
		slog.String("verdict", string(res.Verdict)),
		slog.String("tier", res.Tier.String()),
		slog.Any("categories", cats),
		slog.String("source", string(res.Source)),
		slog.String("rule_id", res.RuleID),
		slog.Float64("confidence", res.Confidence),
		slog.Int64("screener_ms", res.LatencyMs),
		slog.String("reason", res.Reason),
	)
}

// truncateForLog returns the first n runes of s with an ellipsis
// suffix when truncated. n is in BYTES (cheap); we don't UTF-8-
// safe the split because the slog line is for ops eyeballing,
// not for downstream parsing.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// OutputGate is the screening choke point. Surfaces (workbench
// http_fetch, agent tool loop, file readers, knowledge seed
// indexer) all funnel through Gate.Screen before letting incoming
// content reach the model.
//
// Mode semantics mirror the command Gate:
//   - OutputModeOff: zero-cost bypass. Returns Clean without
//     calling the screener.
//   - OutputModeShadow: screen + record, but ALWAYS return Clean
//     to the surface. Safe for the rollout -- shadow mode never
//     blocks ingress. This is the default.
//   - OutputModeEnforce: screen + record + honour the verdict.
//     Surfaces consult Verdict and decide what to do (typically:
//     pass-through for Clean / Suspicious, sanitise for Blocked).
type OutputGate struct {
	screener OutputScreener
	recorder ScreeningRecorder
	mode     OutputMode
}

// OutputGateOptions configures NewOutputGate. All fields are
// optional; sensible defaults are applied.
type OutputGateOptions struct {
	Screener OutputScreener
	Recorder ScreeningRecorder
	Mode     OutputMode
	Logger   *slog.Logger
}

// NewOutputGate constructs an OutputGate. Nil fields are defaulted:
// NoopScreener, SlogScreeningRecorder with Logger (default
// slog.Default), OutputModeFromEnv.
func NewOutputGate(opts OutputGateOptions) *OutputGate {
	screener := opts.Screener
	if screener == nil {
		screener = NoopScreener{}
	}
	recorder := opts.Recorder
	if recorder == nil {
		logger := opts.Logger
		if logger == nil {
			logger = slog.Default()
		}
		recorder = SlogScreeningRecorder{Logger: logger}
	}
	mode := opts.Mode
	if mode == "" {
		mode = OutputModeFromEnv()
	}
	return &OutputGate{screener: screener, recorder: recorder, mode: mode}
}

// Mode returns the gate's configured mode. Useful for tests + for
// surfaces that want to short-circuit when the gate is off.
func (g *OutputGate) Mode() OutputMode { return g.mode }

// Screen is the entry every surface calls. Returns:
//
//   - verdict: what the gate ultimately decided.
//     * In OutputModeOff: always Clean.
//     * In OutputModeShadow: always Clean (the underlying screener
//       result is recorded but not honoured).
//     * In OutputModeEnforce: the screener's verdict, verbatim.
//   - result: the full ScreeningResult the screener produced, for
//     callers that want to log/audit the underlying classification
//     even when the verdict was overridden to Clean by shadow.
//   - err: the screener's error if any. Surfaces decide fail-open
//     vs fail-closed per their threat model.
func (g *OutputGate) Screen(ctx context.Context, in ScreeningInput) (ScreeningVerdict, ScreeningResult, error) {
	if g.mode == OutputModeOff {
		clean := ScreeningResult{
			Verdict:    ScreeningVerdictClean,
			Tier:       TierNone,
			Source:     ScreenSourceDisabled,
			Reason:     "output screener off (MEMQL_SAFETY_OUTPUT_SCREEN_MODE=off)",
			Confidence: 1.0,
		}
		return ScreeningVerdictClean, clean, nil
	}
	res, err := g.screener.Screen(ctx, in)
	if err != nil {
		// Record the error so it shows up in audit; return Clean
		// (fail-open) to the surface in shadow + enforce. Surfaces
		// that want fail-closed wrap Screen and check err.
		errRes := ScreeningResult{
			Verdict:   ScreeningVerdictClean,
			Source:    res.Source,
			LatencyMs: res.LatencyMs,
			Reason:    "screener error: " + err.Error(),
		}
		g.recorder.Record(ctx, in, errRes, g.mode)
		return ScreeningVerdictClean, errRes, err
	}
	if g.mode == OutputModeShadow {
		// Record what we WOULD have done, but always return Clean.
		// Mirrors the command-side shadow invariant.
		g.recorder.Record(ctx, in, res, g.mode)
		return ScreeningVerdictClean, res, nil
	}
	g.recorder.Record(ctx, in, res, g.mode)
	return res.Verdict, res, nil
}

// SanitisedReplacement is the recommended drop-in text for a
// surface that needs to sanitise a Blocked verdict. The model
// still sees that SOMETHING happened (so it doesn't silently fail
// + retry) but can't follow the embedded instructions.
func SanitisedReplacement(in ScreeningInput, res ScreeningResult) string {
	var b strings.Builder
	b.WriteString("[CONTENT BLOCKED BY OUTPUT SCREENER")
	if res.RuleID != "" {
		b.WriteString(": ")
		b.WriteString(res.RuleID)
	}
	if res.Reason != "" {
		b.WriteString(" -- ")
		b.WriteString(res.Reason)
	}
	b.WriteString(". ")
	b.WriteString(string(in.ContentType))
	b.WriteString(" of ")
	b.WriteString(humanByteCount(len(in.Content)))
	b.WriteString(" suppressed.]")
	return b.String()
}

// humanByteCount renders byte counts as "12 B" / "3.4 KiB" /
// "12.5 MiB". Used in the SanitisedReplacement so the model sees
// the blast-radius of what got suppressed.
func humanByteCount(n int) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%d B", n)
	case n < k*k:
		return fmt.Sprintf("%.1f KiB", float64(n)/k)
	default:
		return fmt.Sprintf("%.1f MiB", float64(n)/(k*k))
	}
}

// Default OutputGate is the process-wide instance surfaces reach
// for. Same double-checked-locking pattern as DefaultGate.
var (
	defaultOutputGate   atomic.Pointer[OutputGate]
	defaultOutputGateMu sync.Mutex
)

// DefaultOutputGate returns the process-wide OutputGate. First
// call lazily constructs one with NoopScreener + SlogScreeningRecorder
// + OutputModeFromEnv. App boot overrides via SetDefaultOutputGate.
func DefaultOutputGate() *OutputGate {
	if g := defaultOutputGate.Load(); g != nil {
		return g
	}
	defaultOutputGateMu.Lock()
	defer defaultOutputGateMu.Unlock()
	if g := defaultOutputGate.Load(); g != nil {
		return g
	}
	g := NewOutputGate(OutputGateOptions{})
	defaultOutputGate.Store(g)
	return g
}

// SetDefaultOutputGate replaces the process-wide OutputGate.
// Intended for app boot + tests; mirrors SetDefaultGate.
func SetDefaultOutputGate(g *OutputGate) {
	defaultOutputGate.Store(g)
}
