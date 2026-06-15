package planner

// agent_loop_sectionable_generate.go -- the loop-side orchestration that turns
// a SECTIONABLE classification into a generated, Gate-1-validated, persisted
// parallel plan-automation (memql#1394).
//
// triageAndMaybeShortcut (agent_loop_triage.go) calls maybeGenerateSectionable
// AFTER the cheap classifier runs: when the deliverable is sectionable (and not
// the trivial single-turn case), the planner synthesizes the parallel
// plan-automation (agent_loop_sectionable.go), compiles it through the SAME
// Gate-1 sandbox the authoring pipeline uses, persists it through the
// authoring-bundle pipeline as the generated plan-automation for this Plan, and
// stamps the Plan so the generated automation is the execution vehicle. On any
// miss (not sectionable, no Gate-1 seam linked, compile failure, persist error)
// it declines (handled=false) so the Plan falls through to the normal decompose
// loop -- generation is an OPTIMIZATION, never a hard requirement.

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// maybeGenerateSectionable generates + validates + persists a parallel
// plan-automation for a sectionable deliverable. It returns handled=true only
// when the generated automation was produced AND persisted, so the caller can
// route the Plan to direct execution of it instead of the serial decompose
// loop. Every decline path returns (false, nil) so the Plan falls through to
// the normal loop -- this never strands a Plan.
//
// plan is the already-loaded Plan row; dec is the sectionable shape parsed off
// the triage response (one call, shared with the complexity verdict).
func (l *PlannerAgentLoop) maybeGenerateSectionable(ctx context.Context, planId string, plan map[string]any, dec sectionableDecision) (handled bool, err error) {
	if !sectionableGenerationEnabled() {
		return false, nil
	}
	if !dec.Sectionable {
		return false, nil
	}
	goal := getString(plan, "goal")
	ownerUserId := getString(plan, "requestedBy")
	if goal == "" || ownerUserId == "" {
		return false, nil
	}

	// The Gate-1 sandbox lives behind the captureEngine seam (the same one the
	// authoring-capture pipeline uses). When the running binary didn't link the
	// authoring seams, we can't validate the generated automation -- decline so
	// the Plan routes normally rather than persist an unvalidated automation.
	sandbox, ok := l.engine.(authoringSandbox)
	if !ok {
		l.logger.Info("planner sectionable: no Gate-1 sandbox seam on this binary; falling through to decompose",
			"planId", planId)
		return false, nil
	}

	headline := sectionablePlanAutomationName(planId)
	bundle, ok := synthesizeSectionableBundle(headline, goal, dec)
	if !ok {
		// Fewer than minSectionsForFanout usable sections -- not worth the
		// fan-out; the decompose loop (or the direct turn) handles it.
		return false, nil
	}
	bundle = withSectionableLogic(bundle)

	report := sandbox.CompileBundle(bundle.Constructs)
	if !report.OK {
		l.logger.Warn("planner sectionable: generated parallel automation failed Gate-1; falling through to decompose",
			"planId", planId, "headline", bundle.AutomationName,
			"firstFailure", firstFailureHeadline(report))
		return false, nil
	}

	bundleId := id.NewShortId()
	if perr := l.persistSectionableBundle(ctx, ownerUserId, bundleId, planId, goal, bundle, report); perr != nil {
		l.logger.Warn("planner sectionable: persist failed; falling through to decompose",
			"planId", planId, "error", perr)
		return false, nil
	}

	l.logger.Info("planner sectionable: generated parallel plan-automation",
		"planId", planId, "bundleId", bundleId, "headline", bundle.AutomationName,
		"sections", len(bundle.Constructs))
	return true, nil
}

// persistSectionableBundle writes the generated parallel automation through the
// authoring-bundle pipeline: the bundle row (sourcePlanId set, marked as a
// generated sectionable plan-automation), one construct row per member, and a
// validated Gate-1 report. Owned writes run under ownerActorContext so every
// row is owned by the task owner, never the planner system actor.
func (l *PlannerAgentLoop) persistSectionableBundle(ctx context.Context, ownerUserId, bundleId, planId, goal string, bundle authoringBundle, report memql.SandboxReport) error {
	args := map[string]any{
		"bundleId":     bundleId,
		"title":        truncate(goal, 120),
		"summary":      fmt.Sprintf("Generated parallel plan-automation: %d sections fan out then assemble (memql#1394).", sectionCount(bundle)),
		"sourcePlanId": planId,
	}
	if _, err := l.engine.Execute(ownerActorContext(ctx, ownerUserId), fmt.Sprintf(`mutationCreateAuthoringBundle(%s)`, encodeArgs(args))); err != nil {
		return fmt.Errorf("create bundle: %w", err)
	}
	for _, c := range bundle.Constructs {
		cargs := map[string]any{
			"constructId":     id.NewShortId(),
			"bundleId":        bundleId,
			"kind":            c.Kind,
			"name":            c.Name,
			"targetNamespace": authoredTargetNamespace,
			"source":          c.Source,
		}
		if _, err := l.engine.Execute(ownerActorContext(ctx, ownerUserId), fmt.Sprintf(`mutationCreateAuthoringConstruct(%s)`, encodeArgs(cargs))); err != nil {
			return fmt.Errorf("create construct %s/%s: %w", c.Kind, c.Name, err)
		}
	}
	vargs := map[string]any{
		"bundleId":         bundleId,
		"validationReport": structToObject(report),
		"status":           "validated",
	}
	if _, err := l.engine.Execute(ownerActorContext(ctx, ownerUserId), fmt.Sprintf(`mutationRecordBundleValidation(%s)`, encodeArgs(vargs))); err != nil {
		return fmt.Errorf("record validation: %w", err)
	}
	return nil
}

// sectionCount returns the number of per-section production automations in the
// bundle (every automation that isn't the headline or the assemble step).
func sectionCount(bundle authoringBundle) int {
	n := 0
	for _, c := range bundle.Constructs {
		if c.Kind != "automation" {
			continue
		}
		if c.Name == bundle.AutomationName || c.Name == assembleAutomationName(bundle.AutomationName) {
			continue
		}
		n++
	}
	return n
}

// sectionablePlanAutomationName derives the generated automation's headline
// name from the Plan id, sanitized to a DSL identifier.
func sectionablePlanAutomationName(planId string) string {
	short := planId
	if i := lastColon(planId); i >= 0 && i+1 < len(planId) {
		short = planId[i+1:]
	}
	n := sanitizeIdent(short)
	if n == "" {
		n = "plan"
	}
	return "sectionable_" + n
}

// lastColon returns the index of the last ':' in s, or -1. Plan ids are
// colon-segmented (v1:planner:plan:<short>); we want the short tail.
func lastColon(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}
