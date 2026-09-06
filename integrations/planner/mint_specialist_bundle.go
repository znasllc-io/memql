package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/skills"
	"github.com/znasllc-io/memql/core/id"
)

// mint_specialist_bundle.go -- a specialist is a SKILL BUNDLE (epic
// memql#4970, spec section C).
//
// ===========================================================================
// WHAT CHANGED, AND WHY IT IS NOT A RENAME
// ===========================================================================
// `createSpecialist` mints an AGENT row and hangs capabilities off it. The
// spine has no place for that: work never routes to an agent (decision D3), a
// step binds SKILLS, and the thing a person wants to reuse is "the way we do
// this kind of job" rather than a persona that happens to know it.
//
// So a specialist becomes a skill whose `constructRefs` and `scripts` compose
// OTHER skills through `dependsOn` edges, with a name and instructions a
// person can read. Selecting it pulls its components in for free, because
// that is exactly what structural retrieval does with a dependsOn edge
// (component/skills/selection.go) -- the bundle needs no special case
// anywhere in selection, which is the test of whether the modelling is right.
//
// ===========================================================================
// THE THREE GATES ARE THE SAME THREE GATES
// ===========================================================================
// Catalog search before mint, the standing authorization row with its tier
// allowlist, and an approval when the mint is outside the envelope. They are
// REUSED here, unexported, in the same package -- deliberately, and for the
// reason cockpitapp.go reuses preDispatchCheck: a bundle mint needs exactly
// the gates a skill mint needs, and a second copy of those gates is a copy
// that drifts. What differs is what gets written afterwards, not who is
// allowed to write it.

// SpecialistBundle is what the planner asked for.
type SpecialistBundle struct {
	Slug        string
	Name        string
	Description string
	Tier        string
	Category    string
	// Justification is why this bundle should exist, in the planner's own
	// words. REQUIRED, by the same validator the skill mint uses: a mint is
	// the last resort on both paths, and a mint that cannot say why it was
	// not an extendSpecialist is the one an operator most needs to read.
	Justification string
	// ComponentSkillIds are the skills this bundle composes. Each becomes a
	// `dependsOn` edge FROM the bundle, which is what makes selecting the
	// bundle pull them in.
	ComponentSkillIds []string
	// ConstructRefs are the catalog constructs the bundle contributes.
	ConstructRefs []string
	// Instructions is the three-tier body a person and a model both read.
	Instructions map[string]any
	// OriginatingGoalId is the goal whose work surfaced the need.
	OriginatingGoalId string
	// MintedByRunId is the run whose compile emitted the mint.
	MintedByRunId string
}

// MintSpecialistBundle runs the three gates and, when they pass, writes the
// bundle skill and its dependsOn edges.
//
// THE EDGES ARE COMMITTED, not proposed, and that is the one place this
// departs from the propose-then-commit protocol. A proposal is a hypothesis a
// run made from what it observed; these edges are a DECLARATION -- the author
// of the bundle is saying what it is made of, and an edge that steers nothing
// until some later run happens to succeed would make a freshly minted bundle
// select as an empty shell.
func (l *PlannerAgentLoop) MintSpecialistBundle(ctx context.Context, planId string, bundle SpecialistBundle, ownerAgentId, requestedBy string) (string, error) {
	decision := plannerDecision{
		Slug:          bundle.Slug,
		Name:          bundle.Name,
		Description:   bundle.Description,
		Tier:          bundle.Tier,
		Category:      bundle.Category,
		Justification: bundle.Justification,
	}
	if err := validateMintPayload(decision); err != nil {
		return "", fmt.Errorf("mintSpecialistBundle: %w", err)
	}
	if len(bundle.ComponentSkillIds) == 0 {
		// A bundle that composes nothing is a skill, and the ordinary mint
		// path is what mints one. Refusing here rather than writing an empty
		// bundle keeps "specialist" meaning something.
		return "", fmt.Errorf("mintSpecialistBundle: a bundle composes at least one skill")
	}

	// GATE 1 -- catalog search before mint. Reuses the same overlap heuristic
	// the skill mint uses; a bundle that duplicates an existing row is the
	// same waste whichever shape it takes.
	if existing, overlap, err := l.findCatalogOverlap(ctx, decision); err == nil && existing != "" {
		return "", fmt.Errorf("mintSpecialistBundle: %q already covers %.0f%% of this bundle", existing, overlap*100)
	}

	// GATE 2 -- the standing authorization row and its tier allowlist. Read
	// under the REQUESTING USER's actor, not a system one, exactly as the
	// skill mint reads it (memql#3177): the grant is a row that user owns,
	// and reading it with the engine's own reach would answer a different
	// question from the one being asked.
	allowed, reason, err := l.checkMintAuthority(ctx, ownerAgentId, requestedBy, decision.Tier)
	if err != nil {
		return "", fmt.Errorf("mintSpecialistBundle: authority check: %w", err)
	}
	if !allowed {
		return "", fmt.Errorf("mintSpecialistBundle: outside the standing envelope (%s)", reason)
	}

	// GATE 3 is the caller's: a mint outside the envelope parks the plan with
	// an approval, which handleMintSkill already does and this does not
	// duplicate. Reaching here means the envelope allowed it.
	skillId := strings.TrimSpace(bundle.Slug)
	if skillId == "" {
		skillId = id.NewShortId()
	}
	args := map[string]any{
		"skillId":       skillId,
		"slug":          bundle.Slug,
		"name":          bundle.Name,
		"description":   bundle.Description,
		"category":      firstNonBlank(bundle.Category, "specialized"),
		"tier":          bundle.Tier,
		"constructRefs": bundle.ConstructRefs,
		"instructions":  bundle.Instructions,
	}
	if bundle.OriginatingGoalId != "" {
		args["originatingGoalId"] = bundle.OriginatingGoalId
	}
	if bundle.MintedByRunId != "" {
		args["mintedByRunId"] = bundle.MintedByRunId
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("mintSpecialistBundle: marshal args: %w", err)
	}
	if _, err := l.engine.Execute(systemActorContext(ctx), fmt.Sprintf("mintSkill(%s)", string(payload))); err != nil {
		return "", fmt.Errorf("mintSpecialistBundle: mint: %w", err)
	}

	if err := l.writeBundleEdges(ctx, skillId, bundle, planId); err != nil {
		// THE BUNDLE ROW SURVIVES A FAILED EDGE WRITE. It is a real skill
		// either way; what it loses is the composition, which reads as a
		// bundle that pulls nothing in -- visible, and repairable by writing
		// the edges again. Deleting the row instead would lose the
		// instructions somebody's model just wrote.
		l.logger.Warn("mintSpecialistBundle: the bundle was minted but its edges were not written",
			"planId", planId, "skillId", skillId, "error", err)
	}
	l.logger.Info("mintSpecialistBundle: minted",
		"planId", planId, "skillId", skillId, "components", len(bundle.ComponentSkillIds))
	return skillId, nil
}

// writeBundleEdges writes one committed dependsOn edge per component.
//
// The edge ids are DERIVED from the pair, so re-minting a bundle over the
// same components re-versions the same rows rather than accumulating
// duplicates -- the singleton-id reasoning memql#4766 settled for the cluster
// rows, applied to a relation.
func (l *PlannerAgentLoop) writeBundleEdges(ctx context.Context, skillId string, bundle SpecialistBundle, planId string) error {
	components := append([]string(nil), bundle.ComponentSkillIds...)
	sort.Strings(components)
	evidence := []map[string]any{{"runId": firstNonBlank(bundle.MintedByRunId, planId), "stepKey": "mintSpecialistBundle"}}
	for _, component := range components {
		component = strings.TrimSpace(component)
		if component == "" || component == skillId {
			continue
		}
		edgeId := bundleEdgeId(skillId, component)
		create := map[string]any{
			"edgeId":      edgeId,
			"fromSkillId": skillId,
			"toSkillId":   component,
			"edgeType":    string(skills.EdgeDependsOn),
			"evidence":    evidence,
			"proposedBy":  "system",
		}
		payload, err := json.Marshal(create)
		if err != nil {
			return err
		}
		if _, err := l.engine.Execute(systemActorContext(ctx),
			fmt.Sprintf("mutation createSkillEdge(%s)", string(payload))); err != nil {
			return err
		}
		commit, err := json.Marshal(map[string]any{"edgeId": edgeId, "evidence": evidence})
		if err != nil {
			return err
		}
		if _, err := l.engine.Execute(systemActorContext(ctx),
			fmt.Sprintf("mutation commitSkillEdge(%s)", string(commit))); err != nil {
			return err
		}
	}
	return nil
}

// bundleEdgeId is stable for a (bundle, component) pair.
func bundleEdgeId(from, to string) string {
	return "bundle-" + shortSegment(from) + "-" + shortSegment(to)
}

// shortSegment reduces a canonical id to something an id segment accepts:
// the part after the last colon, which is the short id the engine composes
// from. A bare id passes through unchanged.
func shortSegment(v string) string {
	if i := strings.LastIndex(v, ":"); i >= 0 && i+1 < len(v) {
		return v[i+1:]
	}
	return v
}

func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
