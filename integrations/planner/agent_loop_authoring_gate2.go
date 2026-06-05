package planner

// agent_loop_authoring_gate2.go -- increment 3 of the planner authoring job
// (epic memql#954, issue #960): the GATE-2 HANDOFF.
//
// Increment 2 produced a Gate-1-clean bundle (emitAndRepairBundle). Gate 2
// (#958) is the behavioral dry-run: it executes the bundle's automation under
// the tiered side-effect interception layer -- reads are real + metered,
// mutations are isolated to an ephemeral sandbox partition, webhooks are
// recorded-and-blocked -- and returns the approval artifact the Gate-3 user
// review consumes.
//
// This file defines the handoff SEAM (authoringGate2) and the orchestration
// that drives it: pick the bundle's automation construct, build the
// DryRunRequest, and run it. The seam keeps the planner package decoupled from
// the concrete *MemQLEngine that Gate-2's RunBundleDryRun needs (the planner
// holds the narrow Engine interface); production wires the real entry point via
// an app/-level adapter, tests pass a fake.
//
// #958's RunBundleDryRun IS on main, so the handoff is wired to the real entry
// point through the adapter. When no dry-run runner is linked into the binary
// (an automations-free build), Gate 2 reports the run as unavailable rather
// than silently passing -- the handoff surfaces that as a clear "Gate 2
// unavailable" outcome, never a false green.

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
)

// authoringGate2 is the Gate-2 seam: run one validated bundle's automation
// behaviorally and return the dry-run approval artifact. Satisfied in
// production by an adapter over memql.RunBundleDryRun (#958); a fake in tests.
// Split from the Engine interface because RunBundleDryRun needs the concrete
// *MemQLEngine, which the planner package does not import.
type authoringGate2 interface {
	RunBundleDryRun(ctx context.Context, req memql.DryRunRequest) (memql.BundleDryRunReport, error)
}

// gate2Outcome is the result of handing a Gate-1-clean bundle to Gate 2: the
// dry-run report plus a Clean flag (the report ran to completion without a hard
// step failure). Available is false when no dry-run runner is linked into the
// binary -- the caller must NOT treat that as a pass.
type gate2Outcome struct {
	Available bool
	Clean     bool
	Report    memql.BundleDryRunReport
}

// handoffToGate2 drives the Gate-2 behavioral dry-run for a validated
// (Gate-1-clean) bundle. It resolves the bundle's automation construct, builds
// the DryRunRequest (isolated tier -- zero production side effects), and runs
// it through the seam. trigger is the synthetic event that drives the run; nil
// drives it as a schedule-/manual-style invocation.
//
// The CORE acceptance of #960 (Responsibility -> Gate-1-clean bundle within the
// repair budget) does NOT depend on this step -- it is the downstream review
// gate. So a Gate-2 runner that is unavailable (no automations linked) or that
// errors is reported as a non-fatal outcome the caller records, not a failure
// of the authoring pipeline.
func (l *PlannerAgentLoop) handoffToGate2(ctx context.Context, bundleId string, bundle authoringBundle, trigger *memql.DryRunTriggerEvent, gate2 authoringGate2) (gate2Outcome, error) {
	if gate2 == nil {
		// No Gate-2 seam wired on this binary. Not an error -- the bundle is
		// Gate-1-clean and can still be persisted for a later dry-run.
		l.logger.Info("authoring Gate-2 handoff: no runner wired; skipping dry-run",
			"bundleId", bundleId, "automation", bundle.AutomationName)
		return gate2Outcome{Available: false}, nil
	}

	automationSource, ok := bundleAutomationSource(bundle)
	if !ok {
		return gate2Outcome{}, fmt.Errorf("authoring Gate-2 handoff: bundle %q has no automation construct named %q to dry-run", bundleId, bundle.AutomationName)
	}

	req := memql.DryRunRequest{
		BundleId:         bundleId,
		AutomationName:   bundle.AutomationName,
		AutomationSource: automationSource,
		TriggerEvent:     trigger,
		Mode:             memql.DryRunModeIsolated, // zero production side effects
	}
	report, err := gate2.RunBundleDryRun(ctx, req)
	if err != nil {
		// Gate-2 unavailable (no runner linked) or a setup error. Surface as an
		// unavailable outcome -- the Gate-1-clean bundle stands; the dry-run can
		// be retried on a binary that carries the runner.
		l.logger.Warn("authoring Gate-2 handoff: dry-run could not run",
			"bundleId", bundleId, "automation", bundle.AutomationName, "error", err)
		return gate2Outcome{Available: false}, nil
	}
	l.logger.Info("authoring Gate-2 handoff: dry-run complete",
		"bundleId", bundleId, "automation", bundle.AutomationName,
		"ok", report.OK, "mutations", len(report.SideEffectManifest.Mutations),
		"aiCalls", len(report.SideEffectManifest.AiCalls))
	return gate2Outcome{Available: true, Clean: report.OK, Report: report}, nil
}

// bundleAutomationSource returns the .memql source of the bundle's headline
// automation construct (matched by AutomationName), or false when the bundle
// carries no such construct -- the emit pass is contracted to include exactly
// one automation, but a malformed bundle is caught here rather than handed a
// blank source to Gate 2.
func bundleAutomationSource(bundle authoringBundle) (string, bool) {
	for _, c := range bundle.Constructs {
		if c.Kind == "automation" && c.Name == bundle.AutomationName {
			return c.Source, true
		}
	}
	// Fall back to the sole automation construct if the name didn't match
	// exactly (the emit pass names it from the design plan, so a mismatch is a
	// model slip rather than a missing automation).
	var found string
	count := 0
	for _, c := range bundle.Constructs {
		if c.Kind == "automation" {
			found = c.Source
			count++
		}
	}
	if count == 1 {
		return found, true
	}
	return "", false
}
