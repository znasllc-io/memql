package memql

// authoring_dryrun.go -- Gate 2 of the planner-authored-automations pipeline
// (epic memql#954, issue #958): the TIERED BEHAVIORAL DRY-RUN sandbox.
//
// Gate 1 (authoring_sandbox*.go) proves a bundle's constructs compile + bind.
// Gate 2 takes a Gate-1-clean bundle and EXECUTES one of its automations
// behaviorally -- through the SAME compiled-step executors core uses -- against
// an EPHEMERAL SANDBOX PARTITION that isolates graph writes, with a tiered
// side-effect interception layer:
//
//   - reads execute REAL + METERED: ai(), similarTo, webSearch, fetchUrl
//   - writes are ISOLATED: mutations land in the sandbox partition; webhooks /
//     external POSTs are RECORDED-AND-BLOCKED.
//
// The run is driven by a synthetic or replayed trigger event, and captures a
// full trace + a side-effect manifest + a cost estimate as the Gate-3 approval
// artifact (persisted via recordBundleDryRun).
//
// Like the Gate-1 automation-compile seam (authoring_sandbox_automation.go),
// the actual execution lives in component/automations (which imports
// component/memql -> a direct memql -> automations edge is a cycle). So the
// dry-run runner is reached through a registered hook: the automations package
// calls SetSandboxDryRunner from an init(); the memql side invokes whatever was
// registered. A binary that does not link the automations package leaves the
// hook nil and RunBundleDryRun reports the run as unavailable rather than
// silently passing.
//
// This file (increment 1, memql#958) defines the dry-run REPORT shape, the hook
// seam, and the memql-side entry point. The executing bridge + side-effect
// interception live in component/automations/steps (dryrun.go +
// sandbox_registry.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// DryRunMode selects the side-effect tier the sandbox runs under.
type DryRunMode string

const (
	// DryRunModeIsolated is the default Gate-2 tier: reads are real + metered,
	// mutations are isolated to the sandbox partition, and webhooks / external
	// POSTs are recorded-and-blocked. Zero production side effects.
	DryRunModeIsolated DryRunMode = "isolated"

	// DryRunModeFullSandboxLive is the optional final staging tier (gated
	// SEPARATELY): everything runs real IN THE SANDBOX, with webhooks routed to
	// a capture sink instead of being blocked. Increment 3 (memql#958).
	DryRunModeFullSandboxLive DryRunMode = "fullSandboxLive"
)

// DryRunRequest drives one behavioral dry-run of a single authored automation.
type DryRunRequest struct {
	// BundleId is the bundle the automation belongs to (recorded on the report
	// so the Gate-3 approval artifact ties back to the bundle).
	BundleId string `json:"bundleId"`

	// AutomationName is the construct name of the automation to execute. It must
	// be one of the bundle's automation constructs (the caller resolves the
	// source and passes it as AutomationSource).
	AutomationName string `json:"automationName"`

	// AutomationSource is the .memql source for the automation, compiled fresh
	// against the bundle's overlay registry the same way Gate 1 compiled it.
	AutomationSource string `json:"automationSource"`

	// TriggerEvent is the synthetic or replayed event that drives the run. Its
	// topic / kind / payload are bound as the automation's `event` envelope --
	// exactly how a real graph-CDC event would be seen. Nil drives the run as a
	// schedule-/manual-style invocation (synthetic event envelope).
	TriggerEvent *DryRunTriggerEvent `json:"triggerEvent,omitempty"`

	// Mode selects the side-effect tier. Empty defaults to DryRunModeIsolated.
	Mode DryRunMode `json:"mode,omitempty"`

	// FullLiveApproved is the SEPARATE gate for DryRunModeFullSandboxLive. The
	// full-sandbox-live tier (webhooks delivered to a capture sink instead of
	// blocked) is a riskier final staging pass; RunBundleDryRun refuses to run
	// it unless this is explicitly set, so a caller can never fall into live
	// webhook delivery by default. Ignored for the isolated tier.
	FullLiveApproved bool `json:"fullLiveApproved,omitempty"`

	// CaptureSinkUrl is where webhooks are delivered under the full-sandbox-live
	// tier. When empty, full-live webhooks are recorded to the manifest with the
	// configured default sink marker but not dispatched (the sandbox never POSTs
	// to the automation's real webhook target -- only to the capture sink).
	CaptureSinkUrl string `json:"captureSinkUrl,omitempty"`
}

// DryRunTriggerEvent is the synthetic / replayed event that drives a dry-run.
// It mirrors the (topic, kind, payload) triple the automation executor binds as
// the `event` envelope so a replayed production event reproduces faithfully.
type DryRunTriggerEvent struct {
	Topic   string         `json:"topic"`
	Kind    string         `json:"kind,omitempty"`
	Payload map[string]any `json:"payload,omitempty"`
}

// DryRunStep is one captured step of the behavioral trace. It records the step
// id + type, status, duration, and -- for an isolated mutation or a blocked
// webhook -- a note of what the side-effect layer did. The trace is faithful to
// the order the executor ran the steps.
type DryRunStep struct {
	StepId     string `json:"stepId"`
	StepType   string `json:"stepType"`
	Status     string `json:"status"`
	DurationMs int64  `json:"durationMs"`
	// Intercepted is true when the side-effect layer rewrote / blocked this step
	// (a mutation isolated to the sandbox partition, or a webhook recorded and
	// blocked). False for reads that ran for real and pure compute steps.
	Intercepted bool `json:"intercepted,omitempty"`
	// Note carries the side-effect layer's annotation (e.g. "isolated to sandbox
	// partition" / "webhook recorded and blocked") or a step error message.
	Note string `json:"note,omitempty"`
}

// RecordedMutation is one graph write the automation attempted, captured by the
// side-effect interception layer. Under the isolated tier the write lands in the
// ephemeral sandbox partition (never the live partition); the manifest records
// what WOULD have been written so the approver can review it.
type RecordedMutation struct {
	StepId    string         `json:"stepId"`
	Concept   string         `json:"concept"`
	Id        string         `json:"id,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
	Partition string         `json:"partition"`
}

// RecordedAiCall is one metered AI read (ai()) the automation made. Reads run
// for REAL against the live providers; the manifest records the cost so the
// approver sees what the automation will spend in production.
type RecordedAiCall struct {
	StepId        string  `json:"stepId"`
	Function      string  `json:"function"`
	PromptTokens  int     `json:"promptTokens,omitempty"`
	OutputTokens  int     `json:"outputTokens,omitempty"`
	EstimatedCost float64 `json:"estimatedCostUsd,omitempty"`
}

// RecordedWebCall is one metered web read (similarTo / webSearch / fetchUrl).
// These run for real (they are reads) and are recorded so the approver sees the
// external surfaces the automation touches.
type RecordedWebCall struct {
	StepId   string `json:"stepId"`
	Function string `json:"function"`
	Target   string `json:"target,omitempty"`
}

// BlockedWebhook is one external POST (a webhook step) the side-effect layer
// captured. The request (method + url + body) is recorded so the approver sees
// exactly what would be sent. Under the isolated tier it is RECORDED-AND-BLOCKED
// (Sink empty). Under the full-sandbox-live tier it is delivered to a capture
// sink instead of the automation's real target, and Sink records where -- the
// automation's real webhook target is NEVER POSTed to.
type BlockedWebhook struct {
	StepId string         `json:"stepId"`
	Method string         `json:"method"`
	Url    string         `json:"url"`
	Body   map[string]any `json:"body,omitempty"`
	// Sink is set when the webhook was delivered to a capture sink under the
	// full-sandbox-live tier; empty means recorded-and-blocked (isolated tier).
	Sink string `json:"sink,omitempty"`
}

// SideEffectManifest is the tiered catalog of everything the automation did.
// It matches the v1:authoring:bundle.dryRunReport.sideEffectManifest shape.
type SideEffectManifest struct {
	Mutations       []RecordedMutation `json:"mutations"`
	AiCalls         []RecordedAiCall   `json:"aiCalls"`
	WebCalls        []RecordedWebCall  `json:"webCalls"`
	BlockedWebhooks []BlockedWebhook   `json:"blockedWebhooks"`
}

// CostEstimate is the aggregate metered cost of the run's real reads. It matches
// the v1:authoring:bundle.dryRunReport.costEstimate shape.
type CostEstimate struct {
	Tokens int     `json:"tokens"`
	Usd    float64 `json:"usd"`
}

// BundleDryRunReport is the Gate-2 result -- the approval artifact persisted via
// recordBundleDryRun. Its JSON matches the v1:authoring:bundle
// .dryRunReport field shape exactly: {ok, trace, sideEffectManifest,
// costEstimate}.
type BundleDryRunReport struct {
	// OK is true when the automation ran to completion without a hard step
	// failure. A blocked webhook or isolated mutation does NOT fail the run --
	// that is the expected, correct behaviour under the isolated tier.
	OK bool `json:"ok"`

	// Mode echoes the tier the run executed under.
	Mode DryRunMode `json:"mode"`

	// SandboxPartition is the ephemeral partition graph writes were isolated to.
	// Unique per run; nothing under it touches the live graph.
	SandboxPartition string `json:"sandboxPartition"`

	// AutomationName is the construct that was driven.
	AutomationName string `json:"automationName"`

	// Trace is the ordered behavioral trace of every step the executor ran.
	Trace []DryRunStep `json:"trace"`

	// SideEffectManifest is the tiered catalog of mutations / reads / blocked
	// webhooks.
	SideEffectManifest SideEffectManifest `json:"sideEffectManifest"`

	// CostEstimate is the aggregate metered cost of the run's real reads.
	CostEstimate CostEstimate `json:"costEstimate"`

	// FailureReason carries the headline when OK is false (a hard step failure
	// or a setup error). Empty on success.
	FailureReason string `json:"failureReason,omitempty"`
}

// SandboxDryRunner executes one authored automation behaviorally under the
// tiered side-effect interception layer and returns the captured report. It
// must not touch live graph state beyond the run's ephemeral sandbox partition
// and must never dispatch a real webhook under the isolated tier. Satisfied by
// the bridge in component/automations.
type SandboxDryRunner func(ctx context.Context, engine *MemQLEngine, req DryRunRequest) (BundleDryRunReport, error)

var (
	sandboxDryRunnerMu sync.RWMutex
	sandboxDryRunnerFn SandboxDryRunner
)

// SetSandboxDryRunner registers the behavioral dry-run hook the Gate-2 entry
// point uses. The component/automations package calls this from init() (it
// already depends on memql). Passing nil unregisters the hook (RunBundleDryRun
// then reports the run as unavailable).
func SetSandboxDryRunner(fn SandboxDryRunner) {
	sandboxDryRunnerMu.Lock()
	defer sandboxDryRunnerMu.Unlock()
	sandboxDryRunnerFn = fn
}

// sandboxDryRunner returns the registered hook, or nil when no dry-run runner is
// linked into this binary.
func sandboxDryRunner() SandboxDryRunner {
	sandboxDryRunnerMu.RLock()
	defer sandboxDryRunnerMu.RUnlock()
	return sandboxDryRunnerFn
}

// RunBundleDryRun is the Gate-2 entry point: it executes the requested
// automation behaviorally through the registered dry-run runner and returns the
// approval-artifact report. The engine is used for real reads (ai() + web reads
// run against its live providers) but graph writes are isolated to the run's
// ephemeral sandbox partition by the interception layer -- so it is safe to call
// against a running engine. When no runner is linked, it returns an error rather
// than silently passing (a binary that cannot execute automations cannot
// honestly produce a dry-run artifact).
func RunBundleDryRun(ctx context.Context, engine *MemQLEngine, req DryRunRequest) (BundleDryRunReport, error) {
	run := sandboxDryRunner()
	if run == nil {
		return BundleDryRunReport{}, fmt.Errorf("behavioral dry-run is unavailable: no dry-run runner is registered in this binary (link component/automations)")
	}
	if req.Mode == "" {
		req.Mode = DryRunModeIsolated
	}
	// Full-sandbox-live is gated SEPARATELY: it delivers webhooks to a capture
	// sink (real, in-sandbox) rather than blocking them, so it must be opted into
	// explicitly. Refuse it without FullLiveApproved so a caller never falls into
	// live webhook delivery by defaulting the mode.
	if req.Mode == DryRunModeFullSandboxLive && !req.FullLiveApproved {
		return BundleDryRunReport{}, fmt.Errorf("full-sandbox-live dry-run is gated separately: set DryRunRequest.FullLiveApproved to run it (it delivers webhooks to a capture sink instead of blocking them)")
	}
	return run(ctx, engine, req)
}

// dryRunBundleStatus maps a report to the bundle status recordBundleDryRun
// transitions to: dryRunPassed on a clean run, failed otherwise.
func dryRunBundleStatus(report BundleDryRunReport) string {
	if report.OK {
		return "dryRunPassed"
	}
	return "failed"
}

// BuildDryRunMutationCall renders the recordBundleDryRun(...) call that
// persists a dry-run report as the Gate-3 approval artifact. The report rides on
// the `dryRunReport` object arg; status is dryRunPassed / failed; failureReason
// carries the headline on a failed run. Exposed (and pure) so the report ->
// mutation-args mapping is testable without a live DB.
func BuildDryRunMutationCall(bundleId string, report BundleDryRunReport) (string, error) {
	if bundleId == "" {
		return "", fmt.Errorf("bundleId is required to persist a dry-run report")
	}
	reportObj, err := reportToObject(report)
	if err != nil {
		return "", err
	}
	args := map[string]any{
		"bundleId":     bundleId,
		"status":       dryRunBundleStatus(report),
		"dryRunReport": reportObj,
	}
	if report.FailureReason != "" {
		args["failureReason"] = report.FailureReason
	}
	return "recordBundleDryRun(" + renderDryRunMemQLValue(args) + ")", nil
}

// PersistDryRunReport records a dry-run report on its bundle via
// recordBundleDryRun (the Gate-2 persist path referenced by
// dsl/authoring/mutations.memql). It transitions the bundle to dryRunPassed /
// failed and stores the trace + side-effect manifest + cost estimate as the
// approval artifact. Requires a live engine DB (the mutation writes a row).
func PersistDryRunReport(ctx context.Context, engine *MemQLEngine, bundleId string, report BundleDryRunReport) error {
	if engine == nil {
		return fmt.Errorf("PersistDryRunReport requires an engine")
	}
	call, err := BuildDryRunMutationCall(bundleId, report)
	if err != nil {
		return err
	}
	if _, err := engine.Execute(ctx, call); err != nil {
		return fmt.Errorf("persist dry-run report for bundle %q: %w", bundleId, err)
	}
	return nil
}

// reportToObject round-trips the report through JSON into a map[string]any so it
// can be rendered as a MemQL object literal. Using the JSON tags keeps the
// persisted object shape identical to the v1:authoring:bundle.dryRunReport
// contract documented in dsl/authoring/concepts.memql.
func reportToObject(report BundleDryRunReport) (map[string]any, error) {
	b, err := json.Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("marshal dry-run report: %w", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, fmt.Errorf("unmarshal dry-run report: %w", err)
	}
	return obj, nil
}

// renderDryRunMemQLValue renders a Go value (from a JSON round-trip: string,
// float64, bool, nil, []any, map[string]any) as a MemQL literal. Self-contained
// so the persist path does not depend on the automations-side renderer.
//
// The string arm delegates to languageParser.QuoteString rather than calling
// json.Marshal itself. It was never a live parse defect -- json.Marshal emits
// exactly the escapes the lexer implements -- but it was a separate DEFINITION
// of the escape set feeding text that is then parsed, and a definition that is
// correct today and unowned drifts. memql#3035 is what that costs; memql#3192
// collapses this one into the definition that lives beside the lexer.
func renderDryRunMemQLValue(v any) string {
	switch val := v.(type) {
	case nil:
		return "null"
	case bool:
		if val {
			return "true"
		}
		return "false"
	case string:
		return languageParser.QuoteString(val)
	case float64:
		// JSON numbers decode to float64; render integers without a trailing .0.
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case []any:
		parts := make([]string, 0, len(val))
		for _, item := range val {
			parts = append(parts, renderDryRunMemQLValue(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			kb, _ := json.Marshal(k)
			parts = append(parts, string(kb)+": "+renderDryRunMemQLValue(val[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		b, _ := json.Marshal(val)
		return string(b)
	}
}
