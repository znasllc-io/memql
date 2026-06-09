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
	phaseAutomations := make([]phaseNode, 0, len(plan.Phases))
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
		phaseAutomations = append(phaseAutomations, phaseNode{Name: ph.Name, DependsOn: ph.DependsOn})
	}

	headline := synthesizePhasedHeadline(plan.AutomationName, plan.AutomationPurpose, phaseAutomations)
	constructs = append(constructs, headline)

	return authoringBundle{
		AutomationName: plan.AutomationName,
		Constructs:     constructs,
		ReuseEdges:     dedupeReuseEdges(edges),
	}, nil
}

// phaseNode is one phase in the headline's dependency DAG: the phase's
// sub-automation name plus the phases that must finish before it starts.
type phaseNode struct {
	Name      string
	DependsOn []string
}

// synthesizeHeadlineAutomation is the back-compat entry for a name-only phase
// list (the #1163 callers): it converts names to nodes and delegates. With no
// dependsOn the DAG synthesizer defaults to a strict sequential chain, so this
// yields exactly the original one-per-layer sequence.
func synthesizeHeadlineAutomation(headline, purpose string, phaseAutomations []string) memql.SandboxConstruct {
	nodes := make([]phaseNode, len(phaseAutomations))
	for i, name := range phaseAutomations {
		nodes[i] = phaseNode{Name: name}
	}
	return synthesizePhasedHeadline(headline, purpose, nodes)
}

// synthesizePhasedHeadline builds the headline automation that invokes the
// phase sub-automations honoring their dependency DAG (epic memql#1160, issues
// #1163 + #1164):
//
//   - The phases are topologically LAYERED: layer 0 is every phase with no
//     unmet dependency; layer k is every phase whose deps all sit in earlier
//     layers. Phases in the same layer are mutually independent.
//   - A layer with ONE phase emits a sequential `step` invoking it. A layer
//     with 2+ independent phases emits a `parallel { branches: [...] }` step so
//     they run CONCURRENTLY (#1164).
//   - Layer k>0 is gated on layer k-1's result (`if steps.layerN.result { ... }`),
//     so the layers run in order while phases WITHIN a layer fan out. The
//     scheduler's inter-step dependency topo-sort makes the gating real.
//
// If the DAG can't be layered (a cycle, or an unknown dependsOn), the phases
// fall back to a strict by-index sequential chain so the bundle is still valid.
// The headline is trigger-less (invoked on demand via TriggerAutomation).
func synthesizePhasedHeadline(headline, purpose string, phases []phaseNode) memql.SandboxConstruct {
	// Default to a STRICT sequential chain when NO phase declares a dependency
	// (the #1163 shape + safe default): without explicit edges we can't know two
	// phases are independent, so we must not parallelize them. Inject implicit
	// i-depends-on-(i-1) edges -> one phase per layer. Explicit dependsOn on any
	// phase switches to the real DAG (which parallelizes independent layers).
	if !anyDependsOn(phases) {
		for i := 1; i < len(phases); i++ {
			phases[i].DependsOn = []string{phases[i-1].Name}
		}
	}
	layers := buildPhaseLayers(phases)

	var b strings.Builder
	if strings.TrimSpace(purpose) != "" {
		fmt.Fprintf(&b, "@description(%q)\n", purpose)
	} else {
		fmt.Fprintf(&b, "@description(%q)\n", fmt.Sprintf("Headline automation running %d phases across %d ordered layers.", len(phases), len(layers)))
	}
	fmt.Fprintf(&b, "automation %s {\n", headline)
	prevStep := ""
	for li, layer := range layers {
		stepName := fmt.Sprintf("layer%d", li)
		closeStr := ""
		indent := "    "
		if li > 0 {
			// Gate this layer on the prior layer's result -- forces layer order.
			fmt.Fprintf(&b, "  step %s {\n    if steps.%s.result {\n", stepName, prevStep)
			closeStr, indent = "    }\n", "      "
		} else {
			fmt.Fprintf(&b, "  step %s {\n", stepName)
		}
		if len(layer) == 1 {
			fmt.Fprintf(&b, "%sautomation %s { }\n", indent, phases[layer[0]].Name)
		} else {
			// Independent phases in this layer -> run concurrently.
			fmt.Fprintf(&b, "%sparallel {\n%s  branches: [\n", indent, indent)
			for bi, idx := range layer {
				fmt.Fprintf(&b, "%s    step branch%d { automation %s { } }", indent, bi, phases[idx].Name)
				if bi < len(layer)-1 {
					b.WriteString(",")
				}
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "%s  ],\n%s  wait: \"all\"\n%s}\n", indent, indent, indent)
		}
		b.WriteString(closeStr)
		b.WriteString("  }\n")
		prevStep = stepName
	}
	b.WriteString("}\n")
	return memql.SandboxConstruct{Kind: "automation", Name: headline, Source: b.String()}
}

// anyDependsOn reports whether at least one phase declares a dependency edge.
func anyDependsOn(phases []phaseNode) bool {
	for _, p := range phases {
		if len(p.DependsOn) > 0 {
			return true
		}
	}
	return false
}

// buildPhaseLayers topologically layers the phases by their dependsOn DAG via
// Kahn's algorithm: layer 0 = phases with no (resolvable) dependency, layer k =
// phases whose deps are all satisfied by layers < k. Within a layer the
// original phase order is preserved. On a cycle or any unresolved state the
// remaining phases collapse into a final by-index sequence of singleton layers,
// so the result always covers every phase exactly once and never deadlocks.
func buildPhaseLayers(phases []phaseNode) [][]int {
	idxByName := make(map[string]int, len(phases))
	for i, p := range phases {
		idxByName[p.Name] = i
	}
	// Resolve deps to indices, dropping self / unknown references.
	deps := make([][]int, len(phases))
	for i, p := range phases {
		for _, d := range p.DependsOn {
			if j, ok := idxByName[d]; ok && j != i {
				deps[i] = append(deps[i], j)
			}
		}
	}
	placed := make([]bool, len(phases))
	var layers [][]int
	for remaining := len(phases); remaining > 0; {
		var layer []int
		for i := range phases {
			if placed[i] {
				continue
			}
			ready := true
			for _, j := range deps[i] {
				if !placed[j] {
					ready = false
					break
				}
			}
			if ready {
				layer = append(layer, i)
			}
		}
		if len(layer) == 0 {
			// Cycle / unsatisfiable -- append every leftover phase as its own
			// sequential layer (by index) so we always terminate.
			for i := range phases {
				if !placed[i] {
					layers = append(layers, []int{i})
					placed[i] = true
					remaining--
				}
			}
			break
		}
		for _, i := range layer {
			placed[i] = true
		}
		layers = append(layers, layer)
		remaining -= len(layer)
	}
	return layers
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
