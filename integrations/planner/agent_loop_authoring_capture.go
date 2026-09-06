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
// Flow on a RUN transitioning to succeeded (memql#5050 -- it was a Plan until
// the tool calls it reads moved onto the work spine):
//
//   1. Claim the run once (CAS guard -- a created/updated double-fire or a
//      multi-node race must author once). The claim is never released: a run
//      id is minted once and never reused, so once-ever is the right
//      guarantee.
//   2. Idempotency: skip if a bundle already exists for this run
//      (authoringBundleForRun) -- belt-and-suspenders with the in-process
//      claim across restarts / cross-node re-delivery (memql#1155).
//   3. Transcribe: read the run's tool_result observations and render them as
//      a MemQL automation, one step per call. No model is involved.
//   4. Persist: createAuthoringBundle (sourceRunId set) + a
//      createAuthoringConstruct + recordBundleValidation, carrying Gate 1's
//      re-runnability verdict when the compiler is linked in this binary.
//
// # The LLM re-authoring mode is GONE
//
// There used to be a second mode behind MEMQL_AUTHORING_CAPTURE_MODE=author
// that asked a model to RE-AUTHOR an automation from the task's goal. It was
// off by default because it was measured and found not to work: the live test
// recorded prose and `{ ... }` placeholders instead of MemQL, over-decomposed,
// and burned roughly $0.27 a session producing ZERO bundles.
//
// It is deleted rather than carried, and not only on those grounds: it was the
// one remaining reason this file loaded a Plan, and keeping a measured-dead
// path alive through a migration means porting it, testing it, and reviewing
// it, for a feature whose own comment says it produced nothing.
//
// Cost safety: the whole capture is still gated by
// MEMQL_AUTHORING_CAPTURE_ENABLED (default on, an ops kill-switch). It is now
// free by construction -- transcription reaches no model at all.

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
	langparser "github.com/znasllc-io/memql/component/language/parser"
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

// HandleRunUpdated is the event-bus subscriber for
// graph.node.updated.v1:work:run. A run reaches its terminal succeeded status
// via an update, so the updated topic is the trigger.
func (d *AuthoringCaptureDispatcher) HandleRunUpdated(ev events.Event) {
	d.handle(ev)
}

func (d *AuthoringCaptureDispatcher) handle(ev events.Event) {
	if d == nil || d.loop == nil {
		return
	}
	if !captureEnabled() {
		return
	}
	runId, ownerUserId, goal, status, ok := extractRunFields(ev)
	if !ok {
		return
	}
	// Only a run that ran to COMPLETION is worth transcribing -- a half-run or
	// a failure is not a reproducible path. The kind allow-list that used to
	// sit here is gone with the Plan kinds it named: a run's template already
	// says what it is, and a run nobody's goal asked for has no owner, which
	// runCaptureTranscript refuses on.
	if status != "succeeded" {
		return
	}
	if !d.claim(runId) {
		return
	}
	d.logger.Info("authoring capture: claimed completed run", "runId", runId)

	// Detached goroutine: the bus dispatches subscribers synchronously and the
	// capture is a multi-second multi-pass LLM job (design + emit + repair +
	// dry-run). It is strictly best-effort -- any failure logs and returns; it
	// must NEVER affect the user's already-delivered result.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				d.logger.Error("authoring capture: PANIC",
					"runId", runId, "recover", fmt.Sprintf("%v", r))
			}
		}()
		if rerr := d.runCaptureTranscript(context.Background(), runId, ownerUserId, goal); rerr != nil {
			d.logger.Warn("authoring capture: transcription failed",
				"runId", runId, "error", rerr)
		}
		// Claim intentionally NOT released -- once-ever per run id.
	}()
}

// extractRunFields pulls what the capture subscriber decides on out of a
// v1:work:run graph event: the row id at the top level, the rest under
// "payload".
//
// The goal STATEMENT is not on the run row -- it belongs to v1:work:goal --
// so the automation name stands in as the transcript's title. That is a
// deliberate downgrade rather than an oversight: reading the goal would need a
// second query per event on a subscriber that fires for every run transition,
// and the automation name is what a reader of an authored bundle actually
// wants to see anyway.
func extractRunFields(ev events.Event) (runId, ownerUserId, goal, status string, ok bool) {
	if ev.Payload == nil {
		return "", "", "", "", false
	}
	runId, _ = ev.Payload["id"].(string)
	if runId == "" {
		return "", "", "", "", false
	}
	payload, _ := ev.Payload["payload"].(map[string]any)
	if payload == nil {
		return runId, "", "", "", false
	}
	status = getString(payload, "status")
	ownerUserId = getString(payload, "ownerUserId")
	goal = getString(payload, "automationName")
	return runId, ownerUserId, goal, status, true
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

// existingBundleForRun returns the id of a bundle already captured for this
// run (idempotency), or "" if none. Read under ownerActorContext so the
// owner-scoped query filter resolves to the run's owner.
func (d *AuthoringCaptureDispatcher) existingBundleForRun(ctx context.Context, ownerUserId, runId string) (string, error) {
	q := fmt.Sprintf(`query authoringBundleForRun(sourceRunId:%s)`, langparser.QuoteString(runId))
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
