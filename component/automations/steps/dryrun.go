package steps

// dryrun.go -- the executing bridge for the Gate-2 behavioral dry-run sandbox
// (epic memql#954, issue #958).
//
// component/memql defines the dry-run REPORT shape + the hook seam
// (authoring_dryrun.go) but cannot import the executor (automations imports
// memql -> cycle). This file lives in component/automations/steps -- which
// already depends on both component/automations (the Executor, Loader, Step
// types) and component/memql (the engine, FunctionRegistry) -- and fills the
// hook from init(). It is the dry-run counterpart to the Gate-1
// automation-compile bridge (component/automations/sandbox_bridge.go).
//
// What it does: takes a single authored automation source, compiles it against
// the supplied registry exactly as Gate 1 did, then EXECUTES it through the
// REAL automation Executor + step registry -- the same compiled-step executors
// core uses -- but with the side-effect interception layer wired in via a
// SANDBOX STEP REGISTRY (sandbox_registry.go) that wraps the real registry:
//
//   - reads (queries, ai(), similarTo, webSearch, fetchUrl) DELEGATE to the
//     real executors -> real engine.Execute -> real + metered.
//   - mutations (a direct mutation step, or a function step whose function is a
//     mutation) are ISOLATED: the would-be write is evaluated + recorded into
//     the manifest under the run's ephemeral sandbox partition, and never
//     reaches engine.Execute, so zero rows land in the live graph.
//   - webhooks / external POSTs are RECORDED-AND-BLOCKED.
//
// Increment 1 (this file + sandbox_registry.go) covers the ephemeral sandbox
// partition + mutation isolation + webhook block + the behavioral trace. AI/web
// metering, the cost estimate, and the recordBundleDryRun persist path
// are increment 2; full-sandbox-live mode is increment 3.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

func init() {
	memql.SetSandboxDryRunner(runBundleDryRun)
}

// newSandboxPartition mints the ephemeral partition graph writes are isolated
// to for one dry-run. It is unique per run and namespaced under
// "sandbox:dryrun:" so nothing the run records can ever collide with a live
// partition; the isolation is enforced by the sandbox step registry never
// forwarding a write to the engine, the partition id is the recorded boundary.
func newSandboxPartition() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read failing is effectively impossible on supported platforms;
		// fall back to a fixed suffix rather than panicking inside a dry-run.
		return "sandbox:dryrun:fixed"
	}
	return "sandbox:dryrun:" + hex.EncodeToString(b[:])
}

// runBundleDryRun is the registered memql.SandboxDryRunner. It compiles the
// requested automation against the engine's live registry (read-only), then
// drives one behavioral run through the real Executor wired to a sandbox step
// registry, and assembles the report (trace + side-effect manifest). The engine
// is consulted for real reads only; the sandbox registry guarantees no write or
// webhook escapes.
func runBundleDryRun(ctx context.Context, engine *memql.MemQLEngine, req memql.DryRunRequest) (memql.BundleDryRunReport, error) {
	if engine == nil {
		return memql.BundleDryRunReport{}, fmt.Errorf("dry-run requires an engine")
	}

	partition := newSandboxPartition()
	report := memql.BundleDryRunReport{
		Mode:             req.Mode,
		SandboxPartition: partition,
		AutomationName:   req.AutomationName,
		Trace:            []memql.DryRunStep{},
		SideEffectManifest: memql.SideEffectManifest{
			Mutations:       []memql.RecordedMutation{},
			AiCalls:         []memql.RecordedAiCall{},
			WebCalls:        []memql.RecordedWebCall{},
			BlockedWebhooks: []memql.BlockedWebhook{},
		},
	}

	// Compile the candidate automation against the live registry, read-only --
	// the same single-source compile path Gate 1 uses. A bundle reaching Gate 2
	// is already Gate-1-clean, so a compile failure here is a setup error.
	loader := automations.NewLoader(automations.LoaderOptions{
		Logger:   discardLogger(),
		Registry: engine.Concepts(),
	})
	origin := fmt.Sprintf("dryrun:automation:%s", req.AutomationName)
	automation, err := loader.CompileSource(req.AutomationSource, origin)
	if err != nil {
		report.OK = false
		report.FailureReason = fmt.Sprintf("%s: compile failed: %v", origin, err)
		return report, nil
	}

	// The sandbox registry wraps the real one: it intercepts write-bearing +
	// webhook steps and delegates reads. It collects the manifest as it runs.
	sandbox := newSandboxStepRegistry(NewRegistry(), engine, partition, req.Mode, req.CaptureSinkUrl)

	// Drive the run through the real Executor. Chain tracking + dedup are off:
	// a dry-run is a one-shot, and dedup would skip a re-run of the same
	// automation. Concurrency is irrelevant for a single run.
	executor := automations.NewExecutor(automations.ExecutorOptions{
		// No checkpoint from a preview (memql#2932): nothing resumes a
		// dry-run, and SaveCheckpoint bypasses the sandbox registry to reach
		// the live graph -- the only escaping write that is a RESUMABLE token.
		SandboxRun:   true,
		Logger:       discardLogger(),
		Engine:       engine,
		EventBus:     engine.EventBus(),
		StepRegistry: sandbox,
	})
	defer executor.Close()

	triggerEvent := toTriggerEvent(req.TriggerEvent)
	// ExecuteWithClientEvent, NOT ExecuteWithEvent (memql#2890, mirroring
	// memql#2888 on the preview side).
	//
	// A dry-run's trigger payload is caller-chosen: run_automation binds the
	// MCP caller's `input`, and RunInlineAutomation takes the automation NAME
	// from caller-submitted source. (The planner's Gate-2 path passes no
	// trigger at all -- what is caller-influenced there is the SOURCE, and an
	// LLM-authored bundle is exactly what should not mint a promotable token.)
	// None of them is a payload the server produced.
	//
	// This matters beyond the in-run origin. executeWithEvent stamps TWO
	// fields, and the second is PERSISTED onto any checkpoint the run mints:
	//
	//	exec.SourceTrusted         = automation.Trusted && !callerSuppliedPayload
	//	exec.CallerSuppliedPayload = callerSuppliedPayload
	//
	// and resume.go recomputes trust from the checkpoint:
	//
	//	exec.SourceTrusted = automation.Trusted && !checkpoint.CallerSuppliedPayload
	//
	// So with ExecuteWithEvent the preview stamped CallerSuppliedPayload=false
	// onto a caller-chosen payload, and a later POST /automations/resume of
	// that checkpoint re-dispatched the attacker's payload at INTERNAL origin
	// against the tree-loaded (Trusted=true) automation. That is exactly the
	// replay #2888's comment describes -- with the dry-run as leg 1, which
	// #2888 fixed only on the live leg.
	//
	// The refusal is what mints the token: saveCheckpointOnFailure fires on
	// step failure, so the @serverOnly refusal a preview is SUPPOSED to report
	// is the thing that writes the resumable checkpoint.
	//
	// SourceTrusted itself is unchanged by this (CompileSource already leaves
	// Trusted false, so it was false either way). The change is strictly more
	// restrictive: it marks the checkpoint caller-supplied so resume cannot
	// promote it.
	exec, runErr := executor.ExecuteWithClientEvent(ctx, automation, "dryrun", triggerEvent)

	// Assemble the trace from the execution's step results + the sandbox
	// registry's interception annotations.
	report.Trace = sandbox.buildTrace(automation, exec)
	report.SideEffectManifest = sandbox.manifest()
	report.CostEstimate = sandbox.costEstimate()

	if runErr != nil {
		report.OK = false
		report.FailureReason = fmt.Sprintf("%s: run failed: %v", origin, runErr)
		return report, nil
	}
	if exec != nil && exec.Status == "failed" {
		report.OK = false
		report.FailureReason = fmt.Sprintf("%s: automation reported failure: %s", origin, exec.Error)
		return report, nil
	}

	report.OK = true
	return report, nil
}

// toTriggerEvent converts the request's synthetic / replayed trigger into the
// executor's events.Event. Nil yields nil (the executor then builds a synthetic
// schedule-/manual-style envelope), matching how a non-event run is driven.
func toTriggerEvent(t *memql.DryRunTriggerEvent) *events.Event {
	if t == nil {
		return nil
	}
	return &events.Event{
		Topic:   t.Topic,
		Kind:    events.KindUnspecified,
		Payload: t.Payload,
	}
}

// discardLogger returns a logger that discards everything -- the dry-run path
// should not pollute the host logs with the candidate automation's step chatter.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
