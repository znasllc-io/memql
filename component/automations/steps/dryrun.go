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
// What it does: takes a single authored automation source and compiles it
// against the supplied registry exactly as Gate 1 did -- or, asked by NAME with
// no source, takes the tree loader's own copy of that automation -- then
// EXECUTES it through the REAL automation Executor + step registry -- the same
// compiled-step executors core uses -- but with the side-effect interception
// layer wired in via a SANDBOX STEP REGISTRY (sandbox_registry.go) that wraps
// the real registry:
//
//   - queries and classified read/pure builtins delegate to the real
//     executors and are metered. Integration web builtins remain unclassified.
//   - WRITE-BEARING statements are ISOLATED: the would-be write is evaluated +
//     recorded into the manifest under the run's ephemeral sandbox partition,
//     and never reaches engine.Execute, so zero rows land in the live graph.
//     That covers a `mutation` call, a `logic` call (its statements run
//     through the same sandbox), a publish, an action and a sub-automation.
//   - a step type with NO classification is REFUSED, not forwarded.
//   - builtin executors must be classified as side-effect free. Others are
//     refused, including calls nested inside queries. A refused call makes
//     the preview incomplete instead of fabricating a successful result.
//
// That last point is load-bearing, and was not true until memql#2943. This
// comment claimed "zero rows land in the live graph" while the sandbox
// registry's default arm forwarded every unclassified step to the production
// executors -- so a publish and an action wrote for real, and a mutation
// nested inside a loop or a parallel did too, because those containers
// resolved children against the concrete registry rather than the sandbox.
// The claim is a security argument other issues cite (#2932, #2908), so it now
// fails closed: an unclassified step type stops the dry-run rather than
// quietly escaping. See sandbox_registry.go and child_dispatch.go.
//
// Increment 1 (this file + sandbox_registry.go) covers the ephemeral sandbox
// partition + write isolation + the behavioral trace. AI/web metering, the
// cost estimate, and the recordBundleDryRun persist path are increment 2.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
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

// runBundleDryRun is the registered memql.SandboxDryRunner. It takes the
// automation to run -- the tree loader's own copy when the request names it
// and carries no source, the request's source compiled against the engine's
// live registry (read-only) otherwise -- then drives one behavioral run through
// the real Executor wired to a sandbox step registry, and assembles the report
// (trace + side-effect manifest + who the run acted as). The engine is
// consulted for real reads only; the sandbox registry guarantees no write or
// side effect escapes.
//
// WHO THE RUN ACTS AS FOLLOWS WHAT RUNS, as it does for every other run. The
// loader's copy carries the origin the loader recorded, so a run of it holds
// the principal a live run of it holds: the automation's own when ctx carries
// no caller's identity, the caller's otherwise. A compiled source has no
// recorded origin, so it runs as its caller, or as the ordinary automation
// actor when ctx carries no identity -- never with the engine's authority,
// whatever name it gives its automation. The report says who ran
// (BundleDryRunReport.RanAs, Authority).
func runBundleDryRun(ctx context.Context, engine *memql.MemQLEngine, req memql.DryRunRequest) (memql.BundleDryRunReport, error) {
	if engine == nil {
		return memql.BundleDryRunReport{}, fmt.Errorf("dry-run requires an engine")
	}

	partition := newSandboxPartition()
	report := memql.BundleDryRunReport{
		SandboxPartition: partition,
		AutomationName:   req.AutomationName,
		Trace:            []memql.DryRunStep{},
		SideEffectManifest: memql.SideEffectManifest{
			Mutations: []memql.RecordedMutation{},
			AiCalls:   []memql.RecordedAiCall{},
			WebCalls:  []memql.RecordedWebCall{},
		},
	}

	loader := automations.NewLoader(automations.LoaderOptions{
		Logger:   discardLogger(),
		Registry: engine.Concepts(),
	})
	byName := strings.TrimSpace(req.AutomationSource) == ""
	var automation *automations.Automation
	if byName {
		// BY NAME: the loader's own copy, resolved as every run by name
		// resolves it (Loader.LoadByName). Nothing is compiled from the
		// request: what runs is the automation the tree holds, with the
		// origin the loader recorded for it. A name the tree does not hold is
		// a refusal of the request, not a report of a run.
		loaded, err := loadForDryRunByName(loader, req.AutomationName)
		if err != nil {
			return memql.BundleDryRunReport{}, err
		}
		automation = loaded
		report.AutomationName = automation.Name
	} else {
		// Compile the candidate automation against the live registry,
		// read-only -- the same single-source compile path Gate 1 uses. A
		// bundle reaching Gate 2 is already Gate-1-clean, so a compile failure
		// here is a setup error.
		compiled, err := loader.CompileSource(req.AutomationSource, fmt.Sprintf("dryrun:automation:%s", req.AutomationName))
		if err != nil {
			report.OK = false
			report.FailureReason = fmt.Sprintf("dryrun:automation:%s: compile failed: %v", req.AutomationName, err)
			return report, nil
		}
		automation = compiled
	}
	origin := fmt.Sprintf("dryrun:automation:%s", report.AutomationName)

	// The sandbox registry wraps the real one: it intercepts write-bearing
	// steps and delegates reads. It collects the manifest as it runs.
	sandbox := newSandboxStepRegistry(NewRegistry(), engine, partition)

	// Drive the run through the real Executor. Chain tracking + dedup are off:
	// a dry-run is a one-shot, and dedup would skip a re-run of the same
	// automation. Concurrency is irrelevant for a single run.
	executor := automations.NewExecutor(automations.ExecutorOptions{
		// No journal from a preview (memql#2932): nothing resumes a dry-run,
		// and the journal writer bypasses the sandbox registry to reach the
		// live graph -- the only escaping write that is a RESUMABLE token.
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
	// fields, and the second is PERSISTED onto the v1:work:run row the
	// journal opens (component/automations/journal.go):
	//
	//	exec.SourceTrusted         = automation.Trusted && !callerSuppliedPayload
	//	exec.CallerSuppliedPayload = callerSuppliedPayload
	//
	// and resume.go recomputes trust from that row:
	//
	//	exec.SourceTrusted = automation.Trusted && !journal.CallerSuppliedPayload
	//
	// So with ExecuteWithEvent the preview stamped CallerSuppliedPayload=false
	// onto a caller-chosen payload, and a later POST /automations/resume of
	// that run re-dispatched the attacker's payload at INTERNAL origin
	// against the tree-loaded (Trusted=true) automation. That is exactly the
	// replay #2888's comment describes -- with the dry-run as leg 1, which
	// #2888 fixed only on the live leg.
	//
	// The refusal is what mints the token: the journal writes a step receipt
	// on failure and closes the run at `failed`, so the @serverOnly refusal a
	// preview is SUPPOSED to report is the thing that leaves a resumable run.
	// Which is why a sandboxed executor holds no journal at all
	// (NewExecutor: `if !opts.SandboxRun && opts.Engine != nil`) -- nothing
	// resumes a preview, and the write would escape the sandbox.
	//
	// For a compiled source SourceTrusted is false either way (CompileSource
	// leaves Trusted false), and this call marks the run caller-supplied. For
	// the loader's copy, taken by name, Trusted is true, and this call is what
	// holds its steps at client origin: internal origin requires a trusted
	// source AND a trigger payload the caller did not supply, so the preview
	// is no more permissive than the live run by name it predicts
	// (TestADryRunByNameRunsAtClientOrigin).
	exec, runErr := executor.ExecuteWithClientEvent(ctx, automation, "dryrun", triggerEvent)

	// Assemble the trace from the execution's step results + the sandbox
	// registry's interception annotations.
	report.Trace = sandbox.buildTrace(automation, exec)
	report.SideEffectManifest = sandbox.manifest()
	report.CostEstimate = sandbox.costEstimate()
	if exec != nil {
		// Who the run acted as, as the executor recorded it.
		report.RanAs = exec.Actor.UserId
		report.Authority = authorityStatement(byName, automation.Name, exec.Actor)
	}

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

// loadForDryRunByName resolves the automation a dry run by name runs: the tree
// loader's own copy, by the resolution every run by name uses.
func loadForDryRunByName(loader *automations.Loader, name string) (*automations.Automation, error) {
	const rule = "A dry run by name runs the tree's own copy of an automation; to dry-run an automation the tree does not hold, " +
		"supply its text, and it runs as its caller"
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("dry run refused: the request names no automation and carries no text. %s", rule)
	}
	automation, err := loader.LoadByName(name)
	if err == nil && automation == nil {
		err = fmt.Errorf("automation %q not found", name)
	}
	if err != nil {
		return nil, fmt.Errorf("dry run by name of %q refused: %w. %s", name, err, rule)
	}
	// No run executes a @disabled automation -- it is not scheduled, not
	// subscribed, and refused on every run by name -- so there is no live run
	// for a dry run of it to predict.
	if !automation.IsEnabled() {
		return nil, fmt.Errorf("dry run by name of %q refused: the automation is @disabled, and no run executes a disabled automation; "+
			"remove the @disabled annotation to re-enable it", automation.Name)
	}
	return automation, nil
}

// authorityStatement is BundleDryRunReport.Authority: empty unless the engine
// runs the automation of this name under the cluster's maintenance principal
// and this run did not hold it. Then it says what ran, as whom, and that the
// engine's own run acts with the engine's authority and this one did not.
//
// The list is read by name here because the answer is a sentence and not a
// grant (auth.MaintenanceAutomationNames): whether a run holds the principal is
// decided by the executor, from the origin the loader recorded, and arrives
// here as actor.MaintenancePrincipal.
func authorityStatement(byName bool, name string, actor automations.RunActor) string {
	if actor.UserId == "" || actor.MaintenancePrincipal || !slices.Contains(auth.MaintenanceAutomationNames(), name) {
		return ""
	}
	what := fmt.Sprintf("a supplied text of an automation named %s", name)
	who := fmt.Sprintf("%s, the ordinary automation actor (role %s)", actor.UserId, actor.Role)
	why := "a supplied text has no recorded origin, so it never runs with the engine's authority"
	switch {
	case actor.Inherited && byName:
		what = fmt.Sprintf("the engine's own automation %s", name)
		who = fmt.Sprintf("its caller %s (role %s)", actor.UserId, actor.Role)
		why = "a run started under a caller's identity acts as that caller"
	case actor.Inherited:
		who = fmt.Sprintf("its caller %s (role %s)", actor.UserId, actor.Role)
		why = "a supplied text runs as its caller, never with the engine's authority"
	case byName:
		// Every listed name loads from the engine's own tree (the maintenance
		// gate holds that), so the loader's copy of one, run with no caller's
		// identity, holds the principal. Said plainly should that ever fail.
		what = fmt.Sprintf("the tree's automation %s", name)
		why = "the automation the tree holds under that name was not loaded from the engine's own tree"
	}
	return fmt.Sprintf("This dry run executed %s as %s. The engine's own run of %s acts under the cluster's maintenance principal, "+
		"and this run did not hold it: %s. What this run read is what that actor may read, so a result with nothing to do here "+
		"does not say the engine's own run has nothing to do. The principal is held only by the engine's own automation, in a run that carries no caller's identity.",
		what, who, name, why)
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
