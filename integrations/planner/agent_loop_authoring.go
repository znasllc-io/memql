package planner

// agent_loop_authoring.go -- the planner's AUTHORING job (epic memql#954,
// issue #960): the design pass that turns a natural-language Responsibility
// into a GROUNDED, reuse-or-author dependency plan.
//
// This is the SIBLING of produceArtifact (agent_loop.go): produceArtifact
// turns a one-off goal into a deliverable file; the authoring job turns a
// standing Responsibility into a v1:authoring:bundle (an automation plus its
// dependency closure) that Gate 1 (#956), Gate 2 (#958), and Gate 3 then
// validate + activate.
//
// THIS FILE owns increment 1: the DESIGN PASS.
//
//   1. Load the caller's reusable catalog (cataloguedConstructsForOwner,
//      #957) once, up front -- the compose-first candidate set.
//   2. Run the authoringDesign structured-output prompt: Responsibility (NL)
//      + the catalog summary -> a list of the dependencies the bundle needs,
//      each with a candidate `.memql` source SKETCH so the deterministic
//      matcher can key it.
//   3. Per dependency, run the reuse-or-author decision:
//        - DecideReuse (#957) for the EXACT structural match (a safe direct
//          reuse, no embedding round-trip);
//        - on an exact miss, CatalogNearMatches (#957, similarTo) for the
//          fuzzy tier -- a close-enough cataloged construct is reused, else
//        - the dependency is marked AUTHOR (a genuine gap the emit pass fills).
//
// The grounded bundle-source emission + the Gate-1 repair loop (#960
// increment 2) and the Gate-2 handoff (increment 3) build on this design
// plan; they land in sibling files so each increment ships its own tested PR.
//
// The reuse-or-author math is the EXISTING component/memql layer
// (DecideReuse / CatalogNearMatches); this file is the planner-side
// orchestration around it, kept testable behind narrow seams so the package's
// table-driven tests run with a fake structured-output engine + a fake catalog
// (no live DB), matching agent_loop_*_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// nearMatchThreshold is the minimum cosine similarity a fuzzy (similarTo)
// catalog candidate must clear to be treated as a REUSE rather than an
// author-the-gap. Below this the candidate is "in the neighborhood" but not
// close enough to safely compose, so the dependency is authored net-new.
// Conservative on purpose: a wrong reuse silently changes behavior, whereas a
// spurious author is caught + deduped at promotion time (#957). Tunable; the
// 0.82 floor mirrors the similarity integration's "strong match" band.
const nearMatchThreshold = 0.82

// maxNearMatchCandidates caps how many fuzzy candidates the retrieval returns
// per dependency. We only need the top few to decide reuse-vs-author; a small
// cap keeps the similarTo round-trip cheap.
const maxNearMatchCandidates = 5

// authoringNearMatcher is the fuzzy-tier seam: embed a need's MatchText and
// rank the owner's cataloged constructs by cosine similarity. Satisfied in
// production by the engine's CatalogNearMatches (#957); a fake in tests.
// Split from the Engine interface so the design pass is unit-testable without
// a live embedding provider / DB.
type authoringNearMatcher interface {
	CatalogNearMatches(ctx context.Context, matchText string, limit int) ([]memql.CatalogNearMatch, error)
}

// dependencyDisposition is the per-dependency outcome of the reuse-or-author
// decision: REUSE (exact or fuzzy catalog hit) vs AUTHOR (a genuine gap).
type dependencyDisposition string

const (
	// dispReuseExact -- an identical CatalogKey hit; compose the cataloged
	// construct directly, no embedding round-trip.
	dispReuseExact dependencyDisposition = "reuseExact"
	// dispReuseNear -- a fuzzy (similarTo) hit at or above nearMatchThreshold;
	// compose the close cataloged construct.
	dispReuseNear dependencyDisposition = "reuseNear"
	// dispAuthor -- no exact + no close-enough catalog match; the emit pass
	// authors this dependency net-new (compose-first, author-the-gap).
	dispAuthor dependencyDisposition = "author"
)

// designDependency is one entry the authoringDesign prompt emitted: a
// dependency the bundle needs, with a candidate source sketch the
// deterministic matcher keys.
type designDependency struct {
	// Kind is the DSL construct kind (query / mutation / logic / spec / trait
	// / shape -- the kinds the catalog + sandbox cover).
	Kind string `json:"kind"`
	// Name is the construct name the model proposed. On a REUSE it is replaced
	// by the cataloged construct's real name so the emitted bundle references a
	// real symbol (no hallucinated names).
	Name string `json:"name"`
	// Purpose is the one-line intent -- carried into the emit pass as grounding
	// and into the bundle's reuse-edge record.
	Purpose string `json:"purpose"`
	// CandidateSource is the model's `.memql` sketch of the construct. It is
	// the input to DecideReuse (which keys it) and, on an AUTHOR disposition,
	// the seed the emit pass grounds + repairs.
	CandidateSource string `json:"candidateSource"`
}

// resolvedDependency is a designDependency after the reuse-or-author decision:
// the disposition plus, on a reuse, the cataloged construct it resolved to.
type resolvedDependency struct {
	designDependency
	Disposition dependencyDisposition `json:"disposition"`
	// ReuseName / ReuseKind / ReuseNamespace identify the cataloged construct a
	// REUSE disposition composes. Empty on AUTHOR.
	ReuseName      string `json:"reuseName,omitempty"`
	ReuseKind      string `json:"reuseKind,omitempty"`
	ReuseNamespace string `json:"reuseNamespace,omitempty"`
	// Similarity is the fuzzy-tier cosine score on a dispReuseNear (0 otherwise).
	Similarity float64 `json:"similarity,omitempty"`
}

// resolvedPhase is one phase of a multi-phase bundle after the
// reuse-or-author decision: its own sub-automation name + purpose + the
// dependency closure that phase needs. The headline automation chains the
// phases in slice order (phase 0 -> phase 1 -> ...). See
// agent_loop_authoring_phases.go.
type resolvedPhase struct {
	Name         string               `json:"name"`
	Purpose      string               `json:"purpose"`
	Dependencies []resolvedDependency `json:"dependencies"`
	// DependsOn names the phases that must COMPLETE before this phase may
	// start (epic memql#1160, issue #1164). Empty for a phase that depends on
	// nothing -- a DAG root. When NO phase declares dependsOn the emit falls
	// back to a strict sequential chain (the #1163 shape); when dependsOn is
	// present the synthesized headline runs independent phases (same DAG level)
	// concurrently via a `parallel` step and dependent phases in order.
	DependsOn []string `json:"dependsOn,omitempty"`
}

// designPlan is the full output of the design pass: the automation's own
// outline plus every dependency resolved to reuse-or-author.
type designPlan struct {
	// AutomationName / AutomationPurpose describe the bundle's headline
	// automation (the construct that the dependency closure supports). On a
	// multi-phase plan the headline is synthesized to chain the phases.
	AutomationName    string               `json:"automationName"`
	AutomationPurpose string               `json:"automationPurpose"`
	Dependencies      []resolvedDependency `json:"dependencies"`
	// Phases is the ordered phase decomposition for a long, multi-step
	// Responsibility (epic memql#1160, issue #1163). Empty for a single-phase
	// bundle (the common case) -- then AutomationName/Dependencies above are
	// the whole bundle. When len(Phases) >= 2 the emit pass emits one
	// sub-automation per phase + synthesizes a headline that triggers them in
	// sequence; AutomationName names that headline.
	Phases []resolvedPhase `json:"phases,omitempty"`
}

// isMultiPhase reports whether the plan decomposed into 2+ sequential
// phases (the headline-chains-sub-automations shape). A 0/1-phase plan is
// the flat single-automation bundle.
func (p designPlan) isMultiPhase() bool { return len(p.Phases) >= 2 }

// authorCount returns how many dependencies the emit pass must author net-new
// (the genuine gaps). Reuse edges are composed, not authored.
func (p designPlan) authorCount() int {
	n := 0
	for _, d := range p.Dependencies {
		if d.Disposition == dispAuthor {
			n++
		}
	}
	for _, ph := range p.Phases {
		for _, d := range ph.Dependencies {
			if d.Disposition == dispAuthor {
				n++
			}
		}
	}
	return n
}

// runDesignPass turns a Responsibility statement into a resolved design plan:
// it loads the owner's catalog, runs the authoringDesign structured-output
// prompt, and resolves each emitted dependency to reuse-or-author. ownerUserId
// scopes the catalog load + near-match retrieval to the caller.
//
// near may be nil on a binary without the similarity integration registered
// (the planner runs on the planner node, which carries it; tests pass a fake).
// When nil, the fuzzy tier is skipped and an exact miss falls straight to
// AUTHOR -- bounded + deterministic, never a hard failure.
func (l *PlannerAgentLoop) runDesignPass(ctx context.Context, statement, ownerUserId string, near authoringNearMatcher) (designPlan, error) {
	if strings.TrimSpace(statement) == "" {
		return designPlan{}, fmt.Errorf("authoring design pass: empty responsibility statement")
	}

	catalog, err := l.loadCatalog(ctx, ownerUserId)
	if err != nil {
		// A catalog read failure must not strand the job: proceed with an
		// EMPTY catalog so every dependency falls to AUTHOR. The bundle is
		// still valid (just composes nothing); a later run with a healthy
		// catalog dedups. Logged so the degraded mode is visible.
		l.logger.Warn("authoring design pass: catalog load failed; proceeding compose-nothing",
			"ownerUserId", ownerUserId, "error", err)
		catalog = nil
	}

	resp, err := l.engine.InvokeAI(systemActorContext(ctx), "authoringDesign", map[string]any{
		"responsibility": statement,
		"catalog":        catalogSummary(catalog),
		"now":            time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return designPlan{}, fmt.Errorf("authoringDesign prompt: %w", err)
	}
	raw, err := parseDesignDependencies(resp)
	if err != nil {
		return designPlan{}, err
	}

	plan := designPlan{
		AutomationName:    raw.AutomationName,
		AutomationPurpose: raw.AutomationPurpose,
	}
	for _, dep := range raw.Dependencies {
		resolved := l.resolveDependency(ctx, dep, catalog, near)
		plan.Dependencies = append(plan.Dependencies, resolved)
	}
	// Multi-phase decomposition (#1163): resolve each phase's closure the same
	// reuse-or-author way. A phase with a blank name is given a deterministic
	// fallback so the synthesized headline can reference it.
	if raw.hasPhaseWork() {
		// Map each phase's model-given name to the normalized sub-automation
		// name first, so dependsOn references (which use the model's names) can
		// be remapped to the normalized names the synthesized headline chains.
		normalized := make([]string, len(raw.Phases))
		byRawName := map[string]string{}
		for i, ph := range raw.Phases {
			n := phaseAutomationName(plan.AutomationName, ph.Name, i)
			normalized[i] = n
			if rn := strings.TrimSpace(ph.Name); rn != "" {
				byRawName[rn] = n
			}
		}
		for i, ph := range raw.Phases {
			rp := resolvedPhase{Name: normalized[i], Purpose: ph.Purpose, DependsOn: remapDependsOn(ph.DependsOn, byRawName)}
			for _, dep := range ph.Dependencies {
				rp.Dependencies = append(rp.Dependencies, l.resolveDependency(ctx, dep, catalog, near))
			}
			plan.Phases = append(plan.Phases, rp)
		}
	}
	l.logger.Info("authoring design pass complete",
		"automation", plan.AutomationName, "deps", len(plan.Dependencies),
		"phases", len(plan.Phases), "authorCount", plan.authorCount())
	return plan, nil
}

// phaseAutomationName derives the sub-automation name for phase i. It
// prefers the model's phase name (camelCased, deduped against a collision
// with the headline), falling back to "<headline>PhaseN" so the synthesized
// headline always has a real symbol to chain.
func phaseAutomationName(headline, phaseName string, i int) string {
	n := strings.TrimSpace(phaseName)
	if n == "" || n == headline {
		return fmt.Sprintf("%sPhase%d", headline, i)
	}
	return n
}

// remapDependsOn translates a phase's dependsOn references (the model's
// phase names) to the normalized sub-automation names, dropping any that
// don't resolve to a real phase (a hallucinated / self reference can't be a
// dependency edge).
func remapDependsOn(deps []string, byRawName map[string]string) []string {
	if len(deps) == 0 {
		return nil
	}
	out := make([]string, 0, len(deps))
	seen := map[string]bool{}
	for _, d := range deps {
		n, ok := byRawName[strings.TrimSpace(d)]
		if !ok || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// resolveDependency runs the reuse-or-author decision for one emitted
// dependency. Exact CatalogKey hit -> dispReuseExact; otherwise consult the
// fuzzy tier and, if the top candidate clears nearMatchThreshold ->
// dispReuseNear; else -> dispAuthor. A candidate source that doesn't parse for
// its kind can't be keyed, so it is AUTHORED (the emit pass + repair loop will
// produce a real, parseable construct) rather than failing the whole pass.
func (l *PlannerAgentLoop) resolveDependency(ctx context.Context, dep designDependency, catalog []memql.CatalogEntry, near authoringNearMatcher) resolvedDependency {
	out := resolvedDependency{designDependency: dep, Disposition: dispAuthor}

	decision, err := memql.DecideReuse(dep.Kind, dep.CandidateSource, catalog)
	if err != nil {
		l.logger.Debug("authoring design pass: candidate source not keyable; authoring",
			"kind", dep.Kind, "name", dep.Name, "error", err)
		return out
	}
	if decision.ExactMatch != nil {
		out.Disposition = dispReuseExact
		out.ReuseName = decision.ExactMatch.Name
		out.ReuseKind = decision.ExactMatch.Kind
		// Reference the REAL cataloged name so the emitted bundle composes a
		// real symbol, not the model's proposed name.
		out.Name = decision.ExactMatch.Name
		return out
	}

	// Exact miss -> fuzzy tier. Skipped when no matcher is wired or the
	// MatchText came back empty (DecideReuse only sets it on an exact miss).
	if near == nil || decision.MatchText == "" {
		return out
	}
	matches, err := near.CatalogNearMatches(ctx, decision.MatchText, maxNearMatchCandidates)
	if err != nil {
		l.logger.Warn("authoring design pass: near-match retrieval failed; authoring",
			"kind", dep.Kind, "name", dep.Name, "error", err)
		return out
	}
	// Candidates are similarity-ordered; take the best of the SAME kind that
	// clears the threshold. A cross-kind near match is never a safe reuse.
	for _, m := range matches {
		if m.Kind != dep.Kind {
			continue
		}
		if m.Similarity < nearMatchThreshold {
			break // ordered desc -- nothing below will clear either
		}
		out.Disposition = dispReuseNear
		out.ReuseName = m.Name
		out.ReuseKind = m.Kind
		out.ReuseNamespace = m.Namespace
		out.Similarity = m.Similarity
		out.Name = m.Name
		return out
	}
	return out
}

// loadCatalog reads the caller's cataloged (reusable) constructs via
// cataloguedConstructsForOwner (#957) and maps the rows to CatalogEntry.
// Rows missing a catalogKey are skipped -- an un-keyed row can't participate
// in the deterministic match. Returns an empty slice (not an error) when the
// owner has no catalog yet.
func (l *PlannerAgentLoop) loadCatalog(ctx context.Context, ownerUserId string) ([]memql.CatalogEntry, error) {
	if strings.TrimSpace(ownerUserId) == "" {
		return nil, fmt.Errorf("loadCatalog: empty ownerUserId")
	}
	// cataloguedConstructsForOwner filters on actor.userId; the planner's
	// system actor is not the owner, so we read it through ownerActorContext
	// (the same owner-impersonation context the reactive loop uses) which
	// carries an AccessContext whose UserId is the owner -- so actor.userId in
	// the query filter resolves to the owner. The query takes no args.
	res, err := l.engine.Execute(ownerActorContext(ctx, ownerUserId), `cataloguedConstructsForOwner({})`)
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(res)
	entries := make([]memql.CatalogEntry, 0, len(rows))
	for _, row := range rows {
		key := getString(row, "catalogKey")
		if key == "" {
			continue
		}
		entries = append(entries, memql.CatalogEntry{
			Name:       getString(row, "name"),
			Kind:       getString(row, "kind"),
			CatalogKey: key,
		})
	}
	return entries, nil
}

// catalogSummary projects the catalog entries to the compact form the
// authoringDesign prompt consumes: enough for the model to KNOW a construct
// exists (so it can describe a candidate the matcher will key to it) without
// shipping full sources. Name + kind only -- the deterministic + fuzzy tiers
// do the actual matching, not the model.
func catalogSummary(catalog []memql.CatalogEntry) []map[string]any {
	out := make([]map[string]any, 0, len(catalog))
	for _, e := range catalog {
		out = append(out, map[string]any{"name": e.Name, "kind": e.Kind})
	}
	return out
}

// --- design-prompt output shape + parser ----------------------------------

// rawDesignPhase is one phase the authoringDesign prompt emitted on the
// multi-phase path: a named sub-automation + the dependency closure that
// phase needs.
type rawDesignPhase struct {
	Name         string             `json:"name"`
	Purpose      string             `json:"purpose"`
	Dependencies []designDependency `json:"dependencies"`
	// DependsOn names earlier phases that must finish first (#1164). Omit for
	// a phase that can start immediately; phases with disjoint dependsOn run
	// concurrently.
	DependsOn []string `json:"dependsOn,omitempty"`
}

// designResult is the raw structured output of the authoringDesign prompt
// (before the reuse-or-author resolution overlays dispositions).
type designResult struct {
	AutomationName    string             `json:"automationName"`
	AutomationPurpose string             `json:"automationPurpose"`
	Dependencies      []designDependency `json:"dependencies"`
	// Phases is the optional ordered phase decomposition (#1163). When the
	// model emits 2+ phases, the per-phase dependency closures live here and
	// the top-level Dependencies may be empty.
	Phases []rawDesignPhase `json:"phases,omitempty"`
}

// hasPhaseWork reports whether the raw result carries a usable multi-phase
// decomposition (2+ phases, at least one with a dependency).
func (r designResult) hasPhaseWork() bool {
	if len(r.Phases) < 2 {
		return false
	}
	for _, ph := range r.Phases {
		if len(ph.Dependencies) > 0 {
			return true
		}
	}
	return false
}

// parseDesignDependencies unmarshals the authoringDesign structured output,
// tolerating both a string (raw model text) and a map (schema-enforced) return
// shape and stripping a stray markdown fence. Mirrors parsePlannerDecision /
// parseIntakeResult.
func parseDesignDependencies(resp any) (designResult, error) {
	if resp == nil {
		return designResult{}, fmt.Errorf("authoringDesign returned nil")
	}
	var raw []byte
	switch r := resp.(type) {
	case string:
		raw = []byte(r)
	default:
		b, err := json.Marshal(resp)
		if err != nil {
			return designResult{}, err
		}
		raw = b
	}
	raw = extractJSONObject(raw)
	var res designResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return designResult{}, fmt.Errorf("parse design JSON: %w (raw=%s)", err, truncate(string(raw), 200))
	}
	// A usable design has either a flat dependency closure (single-phase) or a
	// multi-phase decomposition with at least one phase carrying a dependency.
	if len(res.Dependencies) == 0 && !res.hasPhaseWork() {
		return designResult{}, fmt.Errorf("authoringDesign emitted no dependencies (raw=%s)", truncate(string(raw), 200))
	}
	return res, nil
}
