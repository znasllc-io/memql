package planner

// agent_loop_authoring_phases.go -- multi-phase / nested-automation
// composition for the authoring emit (epic memql#1160, issue #1163).
//
// A long, multi-step Responsibility decomposes into ordered PHASES (phase 0
// -> phase 1 -> ...). The design pass (agent_loop_authoring.go) resolves each
// phase's dependency closure; this file turns that into a bundle whose
// HEADLINE automation chains one sub-automation PER PHASE, in sequence.
//
// Division of labor:
//   - the LLM emits each phase's BEHAVIOR -- one sub-automation + its authored
//     deps -- via the SAME authoringEmit prompt the single-phase path uses
//     (a phase is just "one automation + its closure"). So no new prompt.
//   - Go DETERMINISTICALLY synthesizes the headline that triggers the phase
//     sub-automations in order. The sequence is real: each step after the
//     first declares a condition referencing the prior step's result, so the
//     automation scheduler's dependency-topo-sort runs them phase 0 -> 1 ->
//     ... (never concurrently). Determinism here means the sequencing can't be
//     fumbled by the model, and it's unit-testable without an engine.
//
// The phase sub-automations + the headline are all trigger-less: they are
// invoked via TriggerAutomation / the `step { automation ... }` kind, not by a
// real-world event. That fits the capture path (a one-off task has no
// recurring trigger) -- the captured bundle is a replayable record, run on
// demand. A future Responsibility-path bundle can stamp the headline's trigger
// from the standing directive; the chaining structure is identical.
//
// Gate 1 / repair, capture persistence, and bundleAutomationSource all operate
// on the flat construct list, so they need no change: the multi-phase bundle
// is just "more constructs, one of which is the headline".

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

// emitMultiPhaseBundle emits a phased bundle: one sub-automation (+ its
// authored closure) per phase via the per-phase authoringEmit call, plus the
// Go-synthesized headline that chains them in order. Reuse edges union across
// phases. Called by emitBundle when the design plan isMultiPhase().
func (l *PlannerAgentLoop) emitMultiPhaseBundle(ctx context.Context, statement string, plan designPlan) (authoringBundle, error) {
	constructs := make([]memql.SandboxConstruct, 0, len(plan.Phases)+4)
	edges := make([]reuseEdge, 0)
	phaseAutomations := make([]string, 0, len(plan.Phases))
	seenAutomation := map[string]bool{}

	for i, ph := range plan.Phases {
		// A phase is "one automation + its closure" -- exactly the shape the
		// single-phase emit handles. Reuse it by projecting the phase into a
		// per-phase designPlan whose headline is the phase sub-automation.
		sub := designPlan{
			AutomationName:    ph.Name,
			AutomationPurpose: ph.Purpose,
			Dependencies:      ph.Dependencies,
		}
		phaseBundle, err := l.emitBundle(ctx, statement, sub)
		if err != nil {
			return authoringBundle{}, fmt.Errorf("phase %d (%s): %w", i, ph.Name, err)
		}
		for _, c := range phaseBundle.Constructs {
			// The model may name the phase automation slightly differently than
			// requested; normalize it to the design's phase name so the headline
			// chains a real symbol.
			if c.Kind == "automation" {
				c.Name = ph.Name
				if seenAutomation[c.Name] {
					continue // one automation per phase
				}
				seenAutomation[c.Name] = true
			}
			constructs = append(constructs, c)
		}
		edges = append(edges, phaseBundle.ReuseEdges...)
		phaseAutomations = append(phaseAutomations, ph.Name)
	}

	headline := synthesizeHeadlineAutomation(plan.AutomationName, plan.AutomationPurpose, phaseAutomations)
	constructs = append(constructs, headline)

	return authoringBundle{
		AutomationName: plan.AutomationName,
		Constructs:     constructs,
		ReuseEdges:     dedupeReuseEdges(edges),
	}, nil
}

// synthesizeHeadlineAutomation builds the headline automation source that
// invokes the phase sub-automations in sequence. Sequencing is enforced by
// making each step after the first carry a condition that references the prior
// step's result -- the automation scheduler topologically sorts steps by their
// inter-step references, so the chain runs phase 0 -> phase 1 -> ... and never
// concurrently. The headline is trigger-less (invoked on demand via
// TriggerAutomation); the leading `@description` documents the chain.
func synthesizeHeadlineAutomation(headline, purpose string, phaseAutomations []string) memql.SandboxConstruct {
	var b strings.Builder
	if strings.TrimSpace(purpose) != "" {
		fmt.Fprintf(&b, "@description(%q)\n", purpose)
	} else {
		fmt.Fprintf(&b, "@description(%q)\n", fmt.Sprintf("Headline automation chaining %d phases in sequence.", len(phaseAutomations)))
	}
	fmt.Fprintf(&b, "automation %s {\n", headline)
	prevStep := ""
	for i, name := range phaseAutomations {
		stepName := fmt.Sprintf("phase%d", i)
		if i == 0 {
			// First phase runs unconditionally.
			fmt.Fprintf(&b, "  step %s {\n    automation %s { }\n  }\n", stepName, name)
		} else {
			// Gate phase i on phase i-1 having produced a result -- this
			// inter-step reference is what forces the sequential ordering.
			fmt.Fprintf(&b, "  step %s {\n    if steps.%s.result {\n      automation %s { }\n    }\n  }\n", stepName, prevStep, name)
		}
		prevStep = stepName
	}
	b.WriteString("}\n")
	return memql.SandboxConstruct{Kind: "automation", Name: headline, Source: b.String()}
}

// dedupeReuseEdges collapses duplicate compose-first edges that several phases
// may each reference (e.g. two phases reuse the same cataloged spec).
func dedupeReuseEdges(edges []reuseEdge) []reuseEdge {
	seen := map[string]bool{}
	out := make([]reuseEdge, 0, len(edges))
	for _, e := range edges {
		key := e.Kind + "/" + e.Name + "/" + e.Namespace
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	return out
}
