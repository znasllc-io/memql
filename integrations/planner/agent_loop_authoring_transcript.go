package planner

// agent_loop_authoring_transcript.go -- DETERMINISTIC transcription capture
// (epic memql#1160, issue #1188).
//
// The original capture (agent_loop_authoring_capture.go) asked an LLM to
// RE-AUTHOR an automation from the task's goal (design -> emit -> Gate-1 ->
// repair). A live test proved that brittle: the model returned prose / `{ ... }`
// placeholders, over-decomposed, couldn't produce compiling MemQL, and burned
// ~$0.27/session producing ZERO bundles.
//
// This is the replacement the owner asked for: capture the LITERAL MemQL of
// what ACTUALLY ran. Every tool call an agent makes is already recorded as a
// v1:planner:task row (category='toolInvocation', carrying toolName + toolArgs)
// by the taskstamp Stamper. So on a completed task we just READ those rows and
// render them as a MemQL automation -- one step per call. No LLM: it's
// transcription, not generation. Reliable, free, and it is genuinely "the MemQL
// that ran", which is exactly what the task surface (#1162 cockpit / #1187
// frontend) should show.
//
// The rendered automation is stored as the same v1:authoring:bundle +
// v1:authoring:construct the viewers already read (linked by sourcePlanId), so
// the surfaces work unchanged. Status is recorded 'validated' with a
// transcript marker -- it is a faithful record, not a Gate-1-compiled artifact.
//
// The LLM author path is kept behind MEMQL_AUTHORING_CAPTURE_MODE=author (off
// by default) for anyone who wants to experiment with re-authoring.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// captureMode selects the capture engine: "transcript" (default -- record the
// literal calls that ran) or "author" (the legacy LLM re-authoring path).
func captureMode() string {
	if strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_AUTHORING_CAPTURE_MODE"))) == "author" {
		return "author"
	}
	return "transcript"
}

// toolCall is one recorded call: the tool name + its argument object as a JSON
// string (already valid MemQL object-literal syntax) + ordering key.
type toolCall struct {
	Name string
	Args string // raw JSON args object, rendered verbatim into the MemQL call
	Seq  int
}

// runCaptureTranscript records the literal MemQL of what a completed task ran.
// It loads the plan, reads its succeeded toolInvocation rows, renders them as a
// MemQL automation, and persists the bundle + construct (sourcePlanId-linked).
// Owned writes run under the task owner's envelope. Best-effort: any failure
// logs and returns; it never affects the delivered result.
func (d *AuthoringCaptureDispatcher) runCaptureTranscript(ctx context.Context, planId, kind string) error {
	plan, err := d.loop.loadPlan(ctx, planId)
	if err != nil {
		return fmt.Errorf("load plan: %w", err)
	}
	ownerUserId := getString(plan, "requestedBy")
	goal := strings.TrimSpace(getString(plan, "goal"))
	if ownerUserId == "" {
		d.logger.Info("authoring transcript: plan missing owner; skipping", "planId", planId)
		return nil
	}

	if existing, err := d.existingBundleForPlan(ctx, ownerUserId, planId); err != nil {
		d.logger.Warn("authoring transcript: idempotency lookup failed; proceeding", "planId", planId, "error", err)
	} else if existing != "" {
		d.logger.Info("authoring transcript: bundle already exists for plan; skipping", "planId", planId, "bundleId", existing)
		return nil
	}

	calls, err := d.loadToolCalls(ctx, planId)
	if err != nil {
		return fmt.Errorf("load tool calls: %w", err)
	}
	if len(calls) == 0 {
		// Nothing concrete ran (no recorded tool calls) -- nothing to transcribe.
		d.logger.Info("authoring transcript: no tool calls recorded for plan; skipping",
			"planId", planId, "kind", kind)
		return nil
	}

	autoName := transcriptAutomationName(planId)
	source := renderTranscriptAutomation(autoName, goal, calls)

	bundleId := id.NewShortId()
	title := goal
	if title == "" {
		title = fmt.Sprintf("Transcript of %s", kind)
	}
	if err := d.persistTranscriptBundle(ctx, ownerUserId, bundleId, planId, title, len(calls)); err != nil {
		return fmt.Errorf("persist transcript bundle: %w", err)
	}
	construct := memql.SandboxConstruct{Kind: "automation", Name: autoName, Source: source}
	if err := d.persistConstructs(ctx, ownerUserId, bundleId, []memql.SandboxConstruct{construct}); err != nil {
		return fmt.Errorf("persist transcript construct: %w", err)
	}

	// Gate 1 (re-runnability check, #1195): compile + bind the rendered
	// automation through the SAME sandbox the author path uses. A transcript is
	// a faithful RECORD regardless; this answers whether it is also genuinely
	// RE-RUNNABLE -- compiles + binds -- versus a record whose literal calls
	// (run-scoped ids, non-automation tool calls) don't form a runnable
	// automation. Best-effort: a binary without the authoring seams linked
	// still stores the transcript as a record, just without the Gate-1 verdict.
	var (
		report     memql.SandboxReport
		gate1Ran   bool
		reRunnable bool
	)
	if ae, ok := d.engine.(captureEngine); ok {
		report = ae.CompileBundle([]memql.SandboxConstruct{construct})
		gate1Ran = true
		reRunnable = report.OK
	}
	if err := d.recordTranscriptValidated(ctx, ownerUserId, bundleId, report, gate1Ran, reRunnable, len(calls)); err != nil {
		return fmt.Errorf("record transcript status: %w", err)
	}

	d.logger.Info("authoring transcript: captured", "planId", planId, "bundleId", bundleId,
		"calls", len(calls), "gate1Ran", gate1Ran, "reRunnable", reRunnable)
	return nil
}

// loadToolCalls reads the plan's succeeded toolInvocation tasks (the literal
// calls the agent made) in seq order.
func (d *AuthoringCaptureDispatcher) loadToolCalls(ctx context.Context, planId string) ([]toolCall, error) {
	q := fmt.Sprintf(`query tasksForPlan(planId:%q)`, planId)
	res, err := d.engine.Execute(systemActorContext(ctx), q)
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(res)
	calls := make([]toolCall, 0, len(rows))
	for _, r := range rows {
		if getString(r, "category") != "toolInvocation" {
			continue
		}
		if st := getString(r, "status"); st != "succeeded" && st != "" {
			continue // a failed/running call isn't part of the reproducible path
		}
		name := getString(r, "toolName")
		if name == "" {
			name = getString(r, "kind")
		}
		if name == "" {
			continue
		}
		calls = append(calls, toolCall{Name: name, Args: toolArgsJSON(r), Seq: intFromAny(r["seq"])})
	}
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].Seq < calls[j].Seq })
	return calls, nil
}

// renderTranscriptAutomation builds the MemQL automation that reproduces the
// recorded calls: one step per call, in order, each invoking the tool with the
// exact args it ran with. Deterministic + verbatim -- this is what actually
// happened, expressed as MemQL.
func renderTranscriptAutomation(name, goal string, calls []toolCall) string {
	var b strings.Builder
	desc := goal
	if desc == "" {
		desc = fmt.Sprintf("Transcript of %d call(s) this task ran.", len(calls))
	} else {
		desc = "Transcript of the calls this task ran for: " + desc
	}
	fmt.Fprintf(&b, "@description(%q)\n", truncate(desc, 200))
	fmt.Fprintf(&b, "automation %s {\n", name)
	for i, c := range calls {
		// Story 9 (#2335): emit the named-args invocation form `name(k: v, ...)`,
		// not the legacy object-literal wrapper `name({...})`. c.Args is the raw
		// recorded JSON args object; lower it to named args (nested values keep
		// their JSON braces). Unparseable / empty args render `name()`.
		var argsMap map[string]any
		_ = json.Unmarshal([]byte(strings.TrimSpace(c.Args)), &argsMap)
		fmt.Fprintf(&b, "  step call%d {\n    %s(%s)\n  }\n", i, c.Name, encodeArgs(argsMap))
	}
	b.WriteString("}\n")
	return b.String()
}

// --- persistence (transcript-specific) ------------------------------------

func (d *AuthoringCaptureDispatcher) persistTranscriptBundle(ctx context.Context, ownerUserId, bundleId, planId, title string, callCount int) error {
	args := map[string]any{
		"bundleId":     bundleId,
		"title":        truncate(title, 120),
		"summary":      fmt.Sprintf("Verbatim transcript of the %d call(s) this task ran.", callCount),
		"sourcePlanId": planId,
	}
	q := fmt.Sprintf(`createAuthoringBundle(%s)`, encodeArgs(args))
	_, err := d.engine.Execute(ownerActorContext(ctx, ownerUserId), q)
	return err
}

// recordTranscriptValidated records the bundle's validation result. The
// transcript is always stored as a 'validated' RECORD -- a clean, non-draft
// status the viewers render well -- carrying a transcript marker. When the
// Gate-1 compiler is linked (gate1Ran), the report also carries the REAL
// compile/bind verdict plus a reRunnable flag (#1195): true when the rendered
// automation actually compiles + binds, false when it is a record whose literal
// calls don't form a runnable automation (diagnostics say why). The bundle is
// never auto-activated into the authored-construct runtime -- that stays a
// separate Gate-3 approval step -- so 'validated' here is a record status, not
// a live registration.
func (d *AuthoringCaptureDispatcher) recordTranscriptValidated(ctx context.Context, ownerUserId, bundleId string, report memql.SandboxReport, gate1Ran, reRunnable bool, callCount int) error {
	vr := map[string]any{
		"transcript": true,
		"callCount":  callCount,
		"reRunnable": reRunnable,
	}
	if gate1Ran {
		vr["ok"] = report.OK
		vr["gate1"] = "ran"
		if obj := structToObject(report); obj["diagnostics"] != nil {
			vr["diagnostics"] = obj["diagnostics"]
		}
	} else {
		// No Gate-1 seam in this binary: record the transcript without a
		// re-runnability verdict rather than asserting a compile that never ran.
		vr["ok"] = true
		vr["gate1"] = "unavailable"
	}
	args := map[string]any{
		"bundleId":         bundleId,
		"status":           "validated",
		"validationReport": vr,
	}
	q := fmt.Sprintf(`recordBundleValidation(%s)`, encodeArgs(args))
	_, err := d.engine.Execute(ownerActorContext(ctx, ownerUserId), q)
	return err
}

// --- helpers --------------------------------------------------------------

// transcriptAutomationName derives a valid camelCase automation name from the
// plan id suffix so the construct is uniquely + deterministically named.
func transcriptAutomationName(planId string) string {
	short := planId
	if i := strings.LastIndex(planId, ":"); i >= 0 {
		short = planId[i+1:]
	}
	var b strings.Builder
	b.WriteString("reproduce")
	up := true
	for _, r := range short {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			if up {
				b.WriteRune(upper(r))
				up = false
			} else {
				b.WriteRune(r)
			}
		default:
			up = true // collapse separators into camelCase humps
		}
	}
	return b.String()
}

func upper(r rune) rune {
	if r >= 'a' && r <= 'z' {
		return r - 32
	}
	return r
}

// toolArgsJSON extracts the recorded toolArgs object as a JSON string suitable
// for verbatim rendering into a MemQL call. Falls back to "{}" when absent.
func toolArgsJSON(row map[string]any) string {
	v, ok := row["toolArgs"]
	if !ok || v == nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
