// agent_loop_authoring_capture.go
//
// The everyday-task CAPTURE orchestrator (epic memql#1160, issue #1161).
//
// This is the live SPINE that finally chains the authoring building blocks
// into an end-to-end job on a real trigger. Until now runDesignPass
// (agent_loop_authoring.go), emitAndRepairBundle (..._emit.go), and
// handoffToGate2 (..._gate2.go) existed + were unit-tested but had ZERO
// non-test callers -- nothing ran the full Responsibility/task -> bundle flow.
// This file connects them: every user-facing one-off task, on completion,
// asynchronously authors a stored, versioned v1:authoring:bundle that CAPTURES
// what ran -- the reproducible, inspectable, editable, exportable MemQL the
// product intent calls for (the #1162 surface renders it).
//
// Design decision (owner): POST-HOC capture, not author-and-run. The task runs
// exactly as it does today (the deliverable is never at risk); the bundle is
// authored AFTER the fact, in a detached goroutine, and is NOT activated --
// capture records the task, it does not replace its execution. Promoting a
// captured bundle to a live, executing automation (author-and-run) is the
// documented follow-up once the spine is proven.
//
// Flow on a capturable Plan transitioning to succeeded:
//
//   1. Claim the Plan once (CAS guard -- a created/updated double-fire or a
//      multi-node race must author once). The claim is never released: a Plan
//      id is minted once and never reused, so once-ever is the right guarantee.
//   2. Idempotency: skip if a bundle already exists for this Plan
//      (authoringBundleForPlan) -- belt-and-suspenders with the in-process
//      claim across restarts / cross-node re-delivery (memql#1155).
//   3. runDesignPass: the task goal becomes the design statement -> a
//      catalog-aware reuse-or-author dependency plan.
//   4. emitAndRepairBundle: emit the .memql + run Gate 1 (sandbox compile+bind)
//      + the bounded repair loop.
//   5. Persist: createAuthoringBundle (sourcePlanId set) + a
//      createAuthoringConstruct per authored construct +
//      recordBundleValidation (validated | failed).
//   6. On a Gate-1-clean bundle, handoffToGate2 (behavioral dry-run) ->
//      recordBundleDryRun (dryRunPassed | failed). Terminal state is
//      dryRunPassed; Gate 3 (user approval) + activation are a later user
//      action via the #1162 surface, never automatic.
//
// Cost safety (default-on for EVERY task -- owner's call): the whole capture is
// gated by MEMQL_AUTHORING_CAPTURE_ENABLED (default on, an ops kill-switch),
// runs under the same process-wide LLM rate ceiling + latching kill-switch as
// every other model call (si_guard, memql#1141), and the emit/repair loop
// already checks the planner's cumulative per-plan LLM budget (#819). A future
// dedicated authoring budget is tracked as a follow-up.

package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// authoredTargetNamespace is the namespace authored constructs register under.
// Owner scoping comes from the construct row's ownerUserId (the runtime
// registers per-owner); the namespace itself is the flat "authored" bucket,
// matching the resolver precedence core (sealed) -> owner-catalog -> bundle-local
// and the activation runtime's expectation (#959).
const authoredTargetNamespace = "authored"

// captureEngine is the optional capability surface the capture orchestrator
// needs beyond the planner's narrow Engine interface: the three authoring Gate
// seams (near-match retrieval, Gate-1 sandbox compile, Gate-2 dry-run). The
// live CognitionEngineAdapter satisfies it; a fake satisfies it in tests. It is
// obtained from the loop's engine via a type assertion so the narrow Engine
// interface -- and every existing test that builds the loop with a bare engine
// fake -- stays untouched. When the running binary's engine does NOT satisfy it
// (no authoring seams linked), capture is skipped, never a hard failure.
type captureEngine interface {
	authoringNearMatcher // CatalogNearMatches
	authoringSandbox     // CompileBundle (Gate 1)
	authoringGate2       // RunBundleDryRun (Gate 2)
}

// AuthoringCaptureDispatcher claims completed user-facing one-off Plans and
// authors a capture bundle for each. It subscribes to the same plan
// created/updated topics as the Planner Agent loop but acts ONLY on a
// capturable kind transitioning to succeeded, so it never races the loop's own
// execution dispatch.
type AuthoringCaptureDispatcher struct {
	loop   *PlannerAgentLoop
	engine Engine
	logger *slog.Logger

	// claimed is a once-ever, atomic check-and-set per Plan id (mirrors the
	// trainSpecialist dispatcher): the FIRST event to reach claim() for a Plan
	// authors the bundle; every later event for that id is dropped. Never
	// released -- a Plan id is minted once and never reused, so a captured Plan
	// has nothing to re-pick-up. The DB idempotency check (step 2) backs this
	// across process restarts where the in-memory set is empty.
	mu      sync.Mutex
	claimed map[string]struct{}
}

// NewAuthoringCaptureDispatcher constructs a capture dispatcher over the
// planner integration's agent loop (for the design/emit/gate2 passes) + its
// engine adapter + logger.
func NewAuthoringCaptureDispatcher(loop *PlannerAgentLoop, engine Engine, logger *slog.Logger) *AuthoringCaptureDispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &AuthoringCaptureDispatcher{
		loop:    loop,
		engine:  engine,
		logger:  logger,
		claimed: make(map[string]struct{}),
	}
}

// HandlePlanUpdated is the event-bus subscriber for
// graph.node.updated.v1:planner:plan. A capturable one-off Plan reaches its
// terminal succeeded status via an update (produceArtifact: startPlanDirect ->
// running -> succeeded), so the updated topic is the trigger.
func (d *AuthoringCaptureDispatcher) HandlePlanUpdated(ev events.Event) {
	d.handle(ev)
}

func (d *AuthoringCaptureDispatcher) handle(ev events.Event) {
	if d == nil || d.loop == nil {
		return
	}
	if !captureEnabled() {
		return
	}
	planId, kind, status, ok := extractPlanFields(ev)
	if !ok {
		return
	}
	// Only capture user-facing one-off task kinds that just SUCCEEDED. Internal
	// machinery (trainSpecialist / embedDomainItems / refineAnalysis / ...) is
	// excluded by the allow-list; a non-terminal or failed status is skipped
	// (we capture what ran to completion, not a half-run or a failure).
	if !capturablePlanKinds[kind] || status != "succeeded" {
		return
	}
	if !d.claim(planId) {
		return
	}
	d.logger.Info("authoring capture: claimed completed plan",
		"planId", planId, "kind", kind)

	// Detached goroutine: the bus dispatches subscribers synchronously and the
	// capture is a multi-second multi-pass LLM job (design + emit + repair +
	// dry-run). It is strictly best-effort -- any failure logs and returns; it
	// must NEVER affect the user's already-delivered result.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				d.logger.Error("authoring capture: PANIC",
					"planId", planId, "recover", fmt.Sprintf("%v", r))
			}
		}()
		// Default: DETERMINISTIC transcription -- record the literal MemQL of
		// what ran (#1188). The legacy LLM re-author path is kept behind
		// MEMQL_AUTHORING_CAPTURE_MODE=author for experimentation; the live test
		// proved it unreliable + costly, so it is no longer the default.
		var rerr error
		if captureMode() == "author" {
			rerr = d.runCapture(context.Background(), planId, kind)
		} else {
			rerr = d.runCaptureTranscript(context.Background(), planId, kind)
		}
		if rerr != nil {
			d.logger.Warn("authoring capture: run failed",
				"planId", planId, "mode", captureMode(), "error", rerr)
		}
		// Claim intentionally NOT released -- once-ever per Plan id.
	}()
}

func (d *AuthoringCaptureDispatcher) claim(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.claimed[id]; ok {
		return false
	}
	d.claimed[id] = struct{}{}
	return true
}

// runCapture is the orchestration body: load the completed Plan, run the
// authoring passes over its goal, and persist the resulting bundle + Gate
// reports. Owned writes run under ownerActorContext(ownerUserId) so every row
// is owned by the task's owner (the mutations stamp ownerUserId from
// actor.userId), never the planner system actor -- no privilege leak.
func (d *AuthoringCaptureDispatcher) runCapture(ctx context.Context, planId, kind string) error {
	plan, err := d.loop.loadPlan(ctx, planId)
	if err != nil {
		return fmt.Errorf("load plan: %w", err)
	}
	ownerUserId := getString(plan, "requestedBy")
	goal := strings.TrimSpace(getString(plan, "goal"))
	if ownerUserId == "" || goal == "" {
		// Nothing to ground the design pass on -- skip quietly.
		d.logger.Info("authoring capture: plan missing owner or goal; skipping",
			"planId", planId, "hasOwner", ownerUserId != "", "hasGoal", goal != "")
		return nil
	}

	// Idempotency across restarts / cross-node re-delivery: if this Plan was
	// already captured, do nothing.
	if existing, err := d.existingBundleForPlan(ctx, ownerUserId, planId); err != nil {
		d.logger.Warn("authoring capture: idempotency lookup failed; proceeding",
			"planId", planId, "error", err)
	} else if existing != "" {
		d.logger.Info("authoring capture: bundle already exists for plan; skipping",
			"planId", planId, "bundleId", existing)
		return nil
	}

	ae, ok := d.engine.(captureEngine)
	if !ok {
		// No authoring Gate seams linked into this binary -- can't compile or
		// dry-run a bundle. Skip rather than persist an unvalidated row.
		d.logger.Info("authoring capture: engine has no authoring seams; skipping",
			"planId", planId)
		return nil
	}

	statement := goal // a one-off goal is the design statement (its standing form)

	designPlan, err := d.loop.runDesignPass(ctx, statement, ownerUserId, ae)
	if err != nil {
		return fmt.Errorf("design pass: %w", err)
	}
	// A captured one-off task is a SINGLE deliverable -- don't let the design
	// over-decompose it into sequential phases (the live test saw a simple list
	// become a 2-phase, 5-failure bundle). Flatten to one automation; phased
	// composition stays for the genuinely-staged Responsibility path. (#1185)
	designPlan = flattenToSingleDeliverable(designPlan)

	bundle, report, clean, err := d.loop.emitAndRepairBundle(ctx, planId, statement, designPlan, ae)
	if err != nil {
		return fmt.Errorf("emit/repair: %w", err)
	}

	bundleId := id.NewShortId()
	if err := d.persistBundle(ctx, ownerUserId, bundleId, planId, goal, designPlan, bundle); err != nil {
		return fmt.Errorf("persist bundle: %w", err)
	}
	if err := d.persistConstructs(ctx, ownerUserId, bundleId, bundle.Constructs); err != nil {
		return fmt.Errorf("persist constructs: %w", err)
	}
	if err := d.recordValidation(ctx, ownerUserId, bundleId, report, clean); err != nil {
		return fmt.Errorf("record validation: %w", err)
	}

	if !clean {
		d.logger.Info("authoring capture: bundle persisted (Gate-1 unclean); not dry-running",
			"planId", planId, "bundleId", bundleId)
		return nil
	}

	// Gate 2: behavioral dry-run on the Gate-1-clean bundle. nil trigger drives
	// it as a manual/schedule-style invocation. Unavailable (no runner linked)
	// is non-fatal -- the validated bundle stands; the dry-run can run later.
	outcome, err := d.loop.handoffToGate2(ctx, bundleId, bundle, nil, ae)
	if err != nil {
		d.logger.Warn("authoring capture: Gate-2 handoff errored; leaving bundle validated",
			"planId", planId, "bundleId", bundleId, "error", err)
		return nil
	}
	if !outcome.Available {
		d.logger.Info("authoring capture: Gate-2 unavailable; leaving bundle validated",
			"planId", planId, "bundleId", bundleId)
		return nil
	}
	if err := d.recordDryRun(ctx, ownerUserId, bundleId, outcome); err != nil {
		return fmt.Errorf("record dry-run: %w", err)
	}
	d.logger.Info("authoring capture: complete",
		"planId", planId, "bundleId", bundleId,
		"constructs", len(bundle.Constructs), "dryRunOK", outcome.Clean)
	return nil
}

// existingBundleForPlan returns the id of a bundle already captured for this
// Plan (idempotency), or "" if none. Read under ownerActorContext so the
// owner-scoped query filter resolves to the task owner.
func (d *AuthoringCaptureDispatcher) existingBundleForPlan(ctx context.Context, ownerUserId, planId string) (string, error) {
	q := fmt.Sprintf(`authoringBundleForPlan({sourcePlanId:%q})`, planId)
	res, err := d.engine.Execute(ownerActorContext(ctx, ownerUserId), q)
	if err != nil {
		return "", err
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return "", nil
	}
	return getString(rows[0], "id"), nil
}

// persistBundle writes the draft bundle row, stamping sourcePlanId (the capture
// provenance + idempotency key) and the compose-first reuse edges.
func (d *AuthoringCaptureDispatcher) persistBundle(ctx context.Context, ownerUserId, bundleId, planId, goal string, plan designPlan, bundle authoringBundle) error {
	summary := strings.TrimSpace(plan.AutomationPurpose)
	if summary == "" {
		summary = goal
	}
	args := map[string]any{
		"bundleId":            bundleId,
		"title":               truncate(goal, 120),
		"summary":             summary,
		"sourcePlanId":        planId,
		"reusedConstructRefs": reuseRefsForBundle(bundle),
	}
	q := fmt.Sprintf(`createAuthoringBundle(%s)`, encodeArgs(args))
	_, err := d.engine.Execute(ownerActorContext(ctx, ownerUserId), q)
	return err
}

// persistConstructs writes one row per authored member construct.
func (d *AuthoringCaptureDispatcher) persistConstructs(ctx context.Context, ownerUserId, bundleId string, constructs []memql.SandboxConstruct) error {
	for _, c := range constructs {
		args := map[string]any{
			"constructId":     id.NewShortId(),
			"bundleId":        bundleId,
			"kind":            c.Kind,
			"name":            c.Name,
			"targetNamespace": authoredTargetNamespace,
			"source":          c.Source,
		}
		q := fmt.Sprintf(`createAuthoringConstruct(%s)`, encodeArgs(args))
		if _, err := d.engine.Execute(ownerActorContext(ctx, ownerUserId), q); err != nil {
			return fmt.Errorf("construct %s/%s: %w", c.Kind, c.Name, err)
		}
	}
	return nil
}

// recordValidation records the Gate-1 report + transitions the bundle to
// validated (clean) or failed (with the headline diagnostic as failureReason).
func (d *AuthoringCaptureDispatcher) recordValidation(ctx context.Context, ownerUserId, bundleId string, report memql.SandboxReport, clean bool) error {
	args := map[string]any{
		"bundleId":         bundleId,
		"validationReport": structToObject(report),
	}
	if clean {
		args["status"] = "validated"
	} else {
		args["status"] = "failed"
		args["failureReason"] = "gate1: " + firstFailureHeadline(report)
	}
	q := fmt.Sprintf(`recordBundleValidation(%s)`, encodeArgs(args))
	_, err := d.engine.Execute(ownerActorContext(ctx, ownerUserId), q)
	return err
}

// recordDryRun records the Gate-2 report + transitions the bundle to
// dryRunPassed (clean) or failed.
func (d *AuthoringCaptureDispatcher) recordDryRun(ctx context.Context, ownerUserId, bundleId string, outcome gate2Outcome) error {
	args := map[string]any{
		"bundleId":     bundleId,
		"dryRunReport": structToObject(outcome.Report),
	}
	if outcome.Clean {
		args["status"] = "dryRunPassed"
	} else {
		args["status"] = "failed"
		args["failureReason"] = "gate2: behavioral dry-run reported a hard step failure"
	}
	q := fmt.Sprintf(`recordBundleDryRun(%s)`, encodeArgs(args))
	_, err := d.engine.Execute(ownerActorContext(ctx, ownerUserId), q)
	return err
}

// --- helpers --------------------------------------------------------------

// capturablePlanKinds is the allow-list of user-facing one-off task kinds the
// capture orchestrator authors a bundle for. Reuses oneOffPlanKinds
// (produceArtifact / adHocAction) -- the single-deliverable task surface -- so
// internal machinery Plans (trainSpecialist / embedDomainItems / refineAnalysis
// / analyzeFile / scopeElevation / agentProactive) are never captured.
var capturablePlanKinds = oneOffPlanKinds

// captureEnabled gates the whole capture path. Defaults ON (owner's call:
// default-on for every task); an operator can disable it cluster-wide via
// MEMQL_AUTHORING_CAPTURE_ENABLED=0 as a cost/incident kill-switch.
func captureEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_AUTHORING_CAPTURE_ENABLED"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// reuseRefsForBundle projects the bundle's compose-first reuse edges to the
// {name, kind, namespace, source} objects the bundle's reusedConstructRefs
// expects. Captured reuse comes from the owner's catalog, so source = "catalog".
func reuseRefsForBundle(bundle authoringBundle) []map[string]any {
	out := make([]map[string]any, 0, len(bundle.ReuseEdges))
	for _, e := range bundle.ReuseEdges {
		out = append(out, map[string]any{
			"name":      e.Name,
			"kind":      e.Kind,
			"namespace": e.Namespace,
			"source":    "catalog",
		})
	}
	return out
}

// firstFailureHeadline returns the first hard-failure diagnostic's error text
// for the bundle's failureReason, or a generic headline if none is itemized.
func firstFailureHeadline(report memql.SandboxReport) string {
	for _, dg := range report.Diagnostics {
		if !dg.OK && !dg.Skipped {
			if dg.Error != "" {
				return fmt.Sprintf("%s %q: %s", dg.Kind, dg.Name, dg.Error)
			}
			return fmt.Sprintf("%s %q failed to compile", dg.Kind, dg.Name)
		}
	}
	return "bundle failed Gate-1 compile+bind"
}

// structToObject marshals a report struct to the generic object the
// validationReport / dryRunReport mutation args expect. A marshal failure
// yields an empty object rather than aborting the capture -- the report is
// informational, not load-bearing.
func structToObject(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}
	return m
}
