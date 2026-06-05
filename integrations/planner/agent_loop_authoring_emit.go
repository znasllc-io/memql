package planner

// agent_loop_authoring_emit.go -- increment 2 of the planner authoring job
// (epic memql#954, issue #960): grounded bundle-source EMISSION + the Gate-1
// REPAIR LOOP.
//
// The design pass (agent_loop_authoring.go) resolved a Responsibility into a
// designPlan: the headline automation + every dependency tagged reuse-or-
// author. This file turns that plan into a COMPILING bundle:
//
//   1. EMIT: render the AUTHOR dependencies + the automation through the
//      authoringEmit structured-output prompt into candidate .memql sources,
//      grounded so they reference the REAL reused construct names the design
//      pass resolved (no hallucinated symbols).
//   2. GATE 1: run the candidate bundle through SandboxCompileBundle*
//      (#956) -- the isolated compile-and-bind harness. Clean -> done.
//   3. REPAIR: on any diagnostic, feed the failures back to the model via
//      authoringRepair and re-emit ONLY the failing constructs, then re-run
//      Gate 1. Bounded by a hard repair-attempt cap AND the planner's
//      existing cumulative LLM ceiling (#819) so the loop can never spend
//      without bound.
//
// The validated (Gate-1-clean) bundle is the input to the Gate-2 dry-run
// handoff (#958, increment 3). When the budget/cap is exhausted before the
// bundle compiles, emitAndRepairBundle returns the last report so the caller
// can park the Plan with the outstanding diagnostics rather than ship a
// broken bundle.
//
// SPEC ANNOTATIONS: @shape (an optional pin documenting the shape a predicate
// reads) and @enabled / @disabled are accepted no-ops on specs -- the sandbox
// and the live spec loader both accept them (specDeclToSpec is shared). The
// earlier latent boot-time skip of @shape-bearing specs was fixed in #1031
// (the converter now derives its annotation surface from the single
// component/language/annotations registry). The authoringEmit / authoringRepair
// prompts keep generated specs minimal (body + @description), but @shape is no
// longer a compile hazard, so a reused spec that carries one is fine.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// maxRepairAttempts caps how many times the loop re-emits a failing bundle
// before giving up and parking. Each attempt is one authoringRepair LLM call
// over the current diagnostics. Bounded + small: a grounded emit usually
// compiles on the first pass or after one targeted repair; a bundle still
// broken after a few rounds is unlikely to converge and should surface to the
// user. The planner's cumulative LLM ceiling (#819) is the hard backstop on
// top of this per-bundle cap. Override with MEMQL_AUTHORING_MAX_REPAIRS.
const maxRepairAttempts = 4

// authoringBundle is the candidate bundle the emit pass produces: the headline
// automation plus the authored member constructs, as (kind, name, source)
// triples ready for Gate 1 + the reuse edges the design pass resolved (carried
// through for the bundle's reusedConstructRefs + the Gate-2 handoff).
type authoringBundle struct {
	AutomationName string                   `json:"automationName"`
	Constructs     []memql.SandboxConstruct `json:"constructs"`
	// ReuseEdges records each composed (not authored) dependency the design
	// pass resolved, so the bundle's reusedConstructRefs + impact graph (#957)
	// capture the compose-first edges.
	ReuseEdges []reuseEdge `json:"reuseEdges,omitempty"`
}

// reuseEdge is one compose-first edge: a cataloged construct the bundle reuses
// rather than authors.
type reuseEdge struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
}

// authoringSandbox is the Gate-1 seam: compile + bind a candidate bundle in
// isolation and return per-construct diagnostics. Satisfied in production by
// the engine's SandboxCompileBundleWithEngine (#956); a fake in tests. Split
// from the Engine interface so the emit + repair loop is unit-testable with a
// canned compile report and no live engine.
type authoringSandbox interface {
	CompileBundle(constructs []memql.SandboxConstruct) memql.SandboxReport
}

// emitAndRepairBundle renders the designPlan's AUTHOR dependencies + the
// automation into a candidate bundle and runs the bounded Gate-1 repair loop
// until the bundle compiles cleanly or the repair budget is exhausted.
//
// Returns the (possibly-not-yet-clean) bundle + its final Gate-1 report + a
// clean flag. The caller (increment 3) hands a clean bundle to Gate 2; on an
// unclean return it parks the Plan with the outstanding diagnostics. planId is
// used only to thread the planner's per-plan LLM ceiling through the repair
// loop (the bundle emission spends against the same cumulative budget as the
// rest of the Plan).
func (l *PlannerAgentLoop) emitAndRepairBundle(ctx context.Context, planId, statement string, plan designPlan, sandbox authoringSandbox) (authoringBundle, memql.SandboxReport, bool, error) {
	if sandbox == nil {
		return authoringBundle{}, memql.SandboxReport{}, false, fmt.Errorf("authoring emit: nil sandbox (Gate 1 unavailable on this binary)")
	}

	bundle, err := l.emitBundle(ctx, statement, plan)
	if err != nil {
		return authoringBundle{}, memql.SandboxReport{}, false, err
	}

	report := sandbox.CompileBundle(bundle.Constructs)
	if report.OK {
		l.logger.Info("authoring emit: bundle compiled clean on first pass",
			"automation", bundle.AutomationName, "constructs", len(bundle.Constructs))
		return bundle, report, true, nil
	}

	// Repair loop: bounded by the per-bundle attempt cap AND the planner's
	// cumulative per-plan LLM ceiling (#819). Re-emit only the failing
	// constructs against the diagnostics, re-run Gate 1, repeat.
	for attempt := 1; attempt <= repairAttemptCap(); attempt++ {
		if blocked, reason := l.repairBudgetExhausted(ctx, planId); blocked {
			l.logger.Warn("authoring repair: per-plan LLM budget exhausted; stopping repair loop",
				"planId", planId, "attempt", attempt, "reason", reason)
			return bundle, report, false, nil
		}

		failing := failingConstructs(bundle.Constructs, report)
		if len(failing) == 0 {
			// Defensive: report not OK but no actionable failing construct
			// (e.g. only skipped kinds). Nothing to repair -- surface as-is.
			break
		}

		repaired, rerr := l.repairConstructs(ctx, statement, bundle, failing, report)
		if rerr != nil {
			l.logger.Warn("authoring repair: re-emit failed; stopping",
				"planId", planId, "attempt", attempt, "error", rerr)
			return bundle, report, false, rerr
		}
		bundle.Constructs = mergeRepaired(bundle.Constructs, repaired)

		report = sandbox.CompileBundle(bundle.Constructs)
		if report.OK {
			l.logger.Info("authoring repair: bundle compiled clean",
				"planId", planId, "automation", bundle.AutomationName, "attempt", attempt)
			return bundle, report, true, nil
		}
		l.logger.Info("authoring repair: bundle still failing; re-attempting",
			"planId", planId, "attempt", attempt, "failures", len(failingConstructs(bundle.Constructs, report)))
	}

	l.logger.Warn("authoring repair: exhausted repair attempts without a clean bundle",
		"planId", planId, "automation", bundle.AutomationName)
	return bundle, report, false, nil
}

// emitBundle renders the headline automation + every AUTHOR dependency into
// candidate .memql sources via the authoringEmit prompt. REUSE dependencies
// are NOT authored -- their real cataloged names are passed as grounding so
// the automation + authored constructs reference real symbols.
func (l *PlannerAgentLoop) emitBundle(ctx context.Context, statement string, plan designPlan) (authoringBundle, error) {
	authored := make([]map[string]any, 0, len(plan.Dependencies))
	reuse := make([]map[string]any, 0)
	edges := make([]reuseEdge, 0)
	for _, dep := range plan.Dependencies {
		if dep.Disposition == dispAuthor {
			authored = append(authored, map[string]any{
				"kind":            dep.Kind,
				"name":            dep.Name,
				"purpose":         dep.Purpose,
				"candidateSource": dep.CandidateSource,
			})
			continue
		}
		reuse = append(reuse, map[string]any{
			"name":      dep.ReuseName,
			"kind":      dep.ReuseKind,
			"namespace": dep.ReuseNamespace,
		})
		edges = append(edges, reuseEdge{Name: dep.ReuseName, Kind: dep.ReuseKind, Namespace: dep.ReuseNamespace})
	}

	resp, err := l.engine.InvokeSI(systemActorContext(ctx), "authoringEmit", map[string]any{
		"responsibility":    statement,
		"automationName":    plan.AutomationName,
		"automationPurpose": plan.AutomationPurpose,
		"authored":          authored,
		"reused":            reuse,
		"now":               time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return authoringBundle{}, fmt.Errorf("authoringEmit prompt: %w", err)
	}
	constructs, err := parseEmittedConstructs(resp)
	if err != nil {
		return authoringBundle{}, err
	}
	return authoringBundle{
		AutomationName: plan.AutomationName,
		Constructs:     constructs,
		ReuseEdges:     edges,
	}, nil
}

// repairConstructs feeds the failing constructs + their Gate-1 diagnostics
// back to the model via authoringRepair and parses the re-emitted sources. The
// prompt is asked to return corrected (kind, name, source) for ONLY the failing
// constructs, keyed by name so mergeRepaired can splice them in.
func (l *PlannerAgentLoop) repairConstructs(ctx context.Context, statement string, bundle authoringBundle, failing []memql.SandboxConstruct, report memql.SandboxReport) ([]memql.SandboxConstruct, error) {
	resp, err := l.engine.InvokeSI(systemActorContext(ctx), "authoringRepair", map[string]any{
		"responsibility": statement,
		"automationName": bundle.AutomationName,
		"failing":        sandboxConstructsToMaps(failing),
		"diagnostics":    failingDiagnostics(report),
		"now":            time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, fmt.Errorf("authoringRepair prompt: %w", err)
	}
	return parseEmittedConstructs(resp)
}

// repairBudgetExhausted reports whether the planner's cumulative per-plan LLM
// ceiling (#819) is already at/over its limit, so the repair loop must not make
// another emit call. Reuses the exact gate the decompose loop uses -- the
// authoring job spends against the same Plan budget. A plan-load failure fails
// OPEN (not exhausted) so a transient read error doesn't abort a healthy repair
// loop; the hard per-bundle attempt cap still bounds it.
func (l *PlannerAgentLoop) repairBudgetExhausted(ctx context.Context, planId string) (bool, string) {
	if planId == "" {
		return false, ""
	}
	plan, err := l.loadPlan(ctx, planId)
	if err != nil {
		return false, ""
	}
	gate := evaluatePlannerCallGate(plan, plannerSystemPromptTokenEstimate, maxPlannerInvocationsPerPlan(), plannerDefaultTokenBudget())
	if gate.Blocked {
		return true, gate.Reason
	}
	return false, ""
}

// repairAttemptCap returns the per-bundle repair cap, honoring the env
// override (mirrors maxPlannerInvocationsPerPlan's override shape).
func repairAttemptCap() int {
	if v := os.Getenv("MEMQL_AUTHORING_MAX_REPAIRS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return maxRepairAttempts
}

// --- bundle / diagnostic helpers ------------------------------------------

// failingConstructs returns the bundle constructs whose Gate-1 diagnostic is a
// hard failure (OK=false, not skipped) -- the set the repair pass re-emits. A
// skipped construct (a kind the sandbox doesn't compile) is NOT a repair
// target; it neither passes nor fails the bundle.
func failingConstructs(constructs []memql.SandboxConstruct, report memql.SandboxReport) []memql.SandboxConstruct {
	failed := map[string]bool{}
	for _, d := range report.Diagnostics {
		if !d.OK && !d.Skipped {
			failed[d.Kind+"/"+d.Name] = true
		}
	}
	out := make([]memql.SandboxConstruct, 0, len(failed))
	for _, c := range constructs {
		if failed[c.Kind+"/"+c.Name] {
			out = append(out, c)
		}
	}
	return out
}

// failingDiagnostics projects the hard-failure diagnostics to the compact form
// the repair prompt consumes: {kind, name, error}.
func failingDiagnostics(report memql.SandboxReport) []map[string]any {
	out := make([]map[string]any, 0)
	for _, d := range report.Diagnostics {
		if d.OK || d.Skipped {
			continue
		}
		out = append(out, map[string]any{"kind": d.Kind, "name": d.Name, "error": d.Error})
	}
	return out
}

// mergeRepaired splices the re-emitted constructs back into the bundle by
// (kind, name): a repaired construct replaces its same-keyed original; a
// repaired construct with no match is appended (the model may legitimately add
// a missing dependency the diagnostics revealed). Originals not re-emitted are
// kept unchanged.
func mergeRepaired(originals, repaired []memql.SandboxConstruct) []memql.SandboxConstruct {
	byKey := map[string]int{}
	out := make([]memql.SandboxConstruct, len(originals))
	copy(out, originals)
	for i, c := range out {
		byKey[c.Kind+"/"+c.Name] = i
	}
	for _, r := range repaired {
		if idx, ok := byKey[r.Kind+"/"+r.Name]; ok {
			out[idx] = r
			continue
		}
		out = append(out, r)
		byKey[r.Kind+"/"+r.Name] = len(out) - 1
	}
	return out
}

// sandboxConstructsToMaps projects constructs to the {kind, name, source} maps
// the repair prompt consumes.
func sandboxConstructsToMaps(constructs []memql.SandboxConstruct) []map[string]any {
	out := make([]map[string]any, 0, len(constructs))
	for _, c := range constructs {
		out = append(out, map[string]any{"kind": c.Kind, "name": c.Name, "source": c.Source})
	}
	return out
}

// --- emit-prompt output shape + parser ------------------------------------

// emittedConstruct is one (kind, name, source) triple the emit / repair prompts
// return.
type emittedConstruct struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Source string `json:"source"`
}

// emitResult is the structured output of authoringEmit / authoringRepair: a
// list of constructs.
type emitResult struct {
	Constructs []emittedConstruct `json:"constructs"`
}

// parseEmittedConstructs unmarshals the emit / repair structured output into
// SandboxConstructs, tolerating both a string (raw model text) and a map
// (schema-enforced) return shape + a stray markdown fence. Mirrors
// parsePlannerDecision / parseDesignDependencies. Drops entries missing a kind
// / name / source (an unusable construct can't be compiled).
func parseEmittedConstructs(resp any) ([]memql.SandboxConstruct, error) {
	if resp == nil {
		return nil, fmt.Errorf("authoring emit returned nil")
	}
	var raw []byte
	switch r := resp.(type) {
	case string:
		raw = []byte(r)
	default:
		b, err := json.Marshal(resp)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	raw = stripJSONFence(raw)
	var res emitResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parse emitted constructs JSON: %w (raw=%s)", err, truncate(string(raw), 200))
	}
	out := make([]memql.SandboxConstruct, 0, len(res.Constructs))
	for _, c := range res.Constructs {
		if strings.TrimSpace(c.Kind) == "" || strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Source) == "" {
			continue
		}
		out = append(out, memql.SandboxConstruct{Kind: c.Kind, Name: c.Name, Source: c.Source})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("authoring emit produced no usable constructs (raw=%s)", truncate(string(raw), 200))
	}
	return out, nil
}
