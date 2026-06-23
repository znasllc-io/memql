package harness

// planner.go is the harness OUTER loop (issue #587): goal -> plan + step
// DAG (#582), then per-step fitScore-driven route / upgrade / provision
// with dedup/merge to prevent agent sprawl. It is the IMPURE half --
// it leans on planner_logic.go for every decision and on the engine +
// graph for the live decompose (LLM), the fitScore (embeddings), and the
// mutation writes.
//
// Design seams (all narrow interfaces) keep the core unit-testable without
// a live DB / LLM / embedding provider:
//   - Decomposer   -- goal -> step DAG (LLM in prod; stub/fallback in tests).
//   - AgentRoster  -- lists existing agents + computes fitScore vs a step
//                     and vs a candidate agent (embeddings in prod).
//   - PlanWriter   -- the #582 plan/step/observation mutations.
//   - AgentFactory -- create / upgrade an agent (the agents-namespace
//                     mutations); reused, never redefined.
//
// The planner produces the plan + steps with step.assignedAgent set; the
// reconciler (#583) then executes them. Every route/upgrade/provision is
// recorded as a `decision` observation (#582) for auditability.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/id"
)

// PlannerComponentName is the planner's logger / dependency identifier.
const PlannerComponentName = "harnessPlanner"

// plannerSystemActorId attributes the planner's graph writes to a stable
// system principal in the audit trail (createdBy). Mirrors the
// reconciler's systemActorId.
const plannerSystemActorId = "system:harness-planner"

// ---------------------------------------------------------------------------
// Narrow dependency interfaces (the test seams)
// ---------------------------------------------------------------------------

// Decomposer turns a goal into a step DAG. The prod implementation calls
// the decomposeGoal LLM prompt; a failure (or a malformed result) lets the
// planner fall back to SingleStepFallback so a plan still runs.
type Decomposer interface {
	Decompose(ctx context.Context, goal string) ([]PlanStep, error)
}

// ExistingAgent is the minimal projection of a v1:agents:agent the planner
// reasons about: its id + the capabilities a route/upgrade decision needs.
type ExistingAgent struct {
	ID               string
	Name             string
	RoleSlug         string
	KnowledgeDomains []string
	Tools            []string
}

// AgentRoster reads the existing agent roster and computes fitScores. The
// prod implementation embeds the step / candidate text and the agents'
// capability text and returns cosine similarities (the cognition fitScore
// generalized). Kept behind an interface so tests inject scores directly.
type AgentRoster interface {
	// ListAgents returns the owner's existing (non-deleted) agents.
	ListAgents(ctx context.Context, ownerUserID string) ([]ExistingAgent, error)
	// FitScores scores stepText against each agent's capability text,
	// returning one AgentFit per agent (0..1). The agents are the slice
	// ListAgents returned, so the caller controls the roster snapshot.
	FitScores(ctx context.Context, stepText string, agents []ExistingAgent) ([]AgentFit, error)
}

// PlanWriter is the #582 mutation surface the planner writes plans, steps,
// and observations through. Reuses the LANDED #582 mutations only.
type PlanWriter interface {
	// CreatePlan creates a v1:harness:plan (status open) and returns its id.
	CreatePlan(ctx context.Context, planID, goal string, input map[string]any) error
	// AddStep adds a v1:harness:step (status pending) with its dependsOn
	// DAG in-edges (already mapped to persisted step ids by the caller).
	AddStep(ctx context.Context, step NewStep) error
	// AssignAgent sets step.assignedAgent. The #582 surface ships no
	// dedicated assign mutation (assignedAgent is stamped by startStep at
	// claim time), so the prod implementation re-versions the pending step
	// row carrying assignedAgent -- the planner runs BEFORE the reconciler
	// claims, so the step is still pending and the re-version is legal.
	AssignAgent(ctx context.Context, step NewStep, agentID string) error
	// RecordDecision appends a `decision` observation for auditability.
	RecordDecision(ctx context.Context, obs Observation) error
}

// NewStep carries everything AddStep / AssignAgent need to (re-)assert the
// owned-tier step row (raw re-version writes do not read-merge).
type NewStep struct {
	ID             string
	PlanID         string
	OwnerUserID    string
	Title          string
	Goal           string
	IdempotencyKey string
	DependsOn      []string
	Input          map[string]any
}

// AgentFactory creates and upgrades agents via the agents-namespace
// mutations (createAgent / updateAgent). Reused, never
// redefined (#587 stays in lane).
type AgentFactory interface {
	// CreateAgent persists a freshly-composed specialist and returns its
	// id. The factory mints the id, stamps kind=specialist + the role +
	// scoped tools + knowledge domains + system prompt + capability
	// embedding source text.
	CreateAgent(ctx context.Context, spec ComposedAgent, ownerUserID, originatingPlanID string) (agentID string, err error)
	// UpgradeAgent attaches the gap's missing domains/tools to an existing
	// agent (the partial-fit upgrade path). newDomains/newTools are the
	// already-merged full lists from MergeCapabilities.
	UpgradeAgent(ctx context.Context, agentID string, newDomains, newTools []string, originatingPlanID string) error
}

// ---------------------------------------------------------------------------
// Planner
// ---------------------------------------------------------------------------

// PlannerConfig tunes the planner. Zero value yields safe defaults.
type PlannerConfig struct {
	Thresholds PlannerThresholds
	// RoleCatalog is the seed-role vocabulary provisioning composes from.
	// Empty uses SeedRoleCatalog().
	RoleCatalog []SeedRole
}

func (c PlannerConfig) withDefaults() PlannerConfig {
	c.Thresholds = c.Thresholds.withDefaults()
	if len(c.RoleCatalog) == 0 {
		c.RoleCatalog = SeedRoleCatalog()
	}
	return c
}

// Planner is the harness outer-loop planner.
type Planner struct {
	decomposer Decomposer
	roster     AgentRoster
	writer     PlanWriter
	factory    AgentFactory
	logger     *slog.Logger
	cfg        PlannerConfig
}

// NewPlanner constructs a Planner. decomposer/roster/writer/factory are
// required; logger defaults to slog.Default.
func NewPlanner(
	decomposer Decomposer,
	roster AgentRoster,
	writer PlanWriter,
	factory AgentFactory,
	logger *slog.Logger,
	cfg PlannerConfig,
) (*Planner, error) {
	if decomposer == nil {
		return nil, fmt.Errorf("harness planner: decomposer is required")
	}
	if roster == nil {
		return nil, fmt.Errorf("harness planner: roster is required")
	}
	if writer == nil {
		return nil, fmt.Errorf("harness planner: writer is required")
	}
	if factory == nil {
		return nil, fmt.Errorf("harness planner: factory is required")
	}
	if logger == nil {
		logger = slog.Default().With("component", PlannerComponentName)
	}
	return &Planner{
		decomposer: decomposer,
		roster:     roster,
		writer:     writer,
		factory:    factory,
		logger:     logger,
		cfg:        cfg.withDefaults(),
	}, nil
}

// PlanResult is the outcome of Plan: the persisted plan id + per-step
// assignment summaries (for logging / tests / the caller's audit view).
type PlanResult struct {
	PlanID string
	Steps  []StepPlanResult
}

// StepPlanResult records what the planner decided for one step.
type StepPlanResult struct {
	StepID        string
	Title         string
	Action        RouteAction
	AssignedAgent string
	Score         float64
	Rationale     string
}

// Plan is the planner's entry point: it decomposes the goal into a step
// DAG, persists the plan + steps (#582), and for each step decides
// route / upgrade / provision against the existing roster, dedups any
// freshly-composed agent, sets step.assignedAgent, and records a
// `decision` observation. Returns the plan id + per-step summaries.
//
// ownerUserID scopes the roster + stamps the agents the planner mints; it
// is the same owned-tier key the #582 mutations stamp from the actor. The
// caller is responsible for stamping the system actor on ctx (see
// contextWithPlannerSystemActor) so the graph writes attribute correctly.
func (p *Planner) Plan(ctx context.Context, ownerUserID, goal string, input map[string]any) (PlanResult, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return PlanResult{}, fmt.Errorf("plan: empty goal")
	}

	// (1) Decompose the goal into a validated step DAG. On any failure --
	// LLM error or malformed output -- fall back to a single-step plan so
	// the goal still runs.
	decomp := p.decomposeOrFallback(ctx, goal)

	// (2) Persist the plan (status open).
	planID := newPlanID(ownerUserID, goal)
	if err := p.writer.CreatePlan(ctx, planID, goal, input); err != nil {
		return PlanResult{}, fmt.Errorf("plan: create plan: %w", err)
	}

	// (3) Persist the steps with their DAG edges. The local Key -> real
	// step id map wires dependsOn across the persisted ids.
	stepIDByKey := make(map[string]string, len(decomp.Steps))
	persisted := make([]NewStep, 0, len(decomp.Steps))
	for _, s := range decomp.Steps {
		stepIDByKey[s.Key] = newStepID(planID, s.Key)
	}
	for _, s := range decomp.Steps {
		deps := make([]string, 0, len(s.DependsOn))
		for _, depKey := range s.DependsOn {
			if did, ok := stepIDByKey[depKey]; ok {
				deps = append(deps, did)
			}
		}
		ns := NewStep{
			ID:             stepIDByKey[s.Key],
			PlanID:         planID,
			OwnerUserID:    ownerUserID,
			Title:          s.Title,
			Goal:           s.Goal,
			IdempotencyKey: newIdempotencyKey(planID, s.Key),
			DependsOn:      deps,
			Input:          map[string]any{"goal": s.Goal, "roleHint": s.RoleHint},
		}
		if err := p.writer.AddStep(ctx, ns); err != nil {
			return PlanResult{}, fmt.Errorf("plan: add step %q: %w", s.Key, err)
		}
		persisted = append(persisted, ns)
	}

	// (4) For each step: fitScore vs roster -> route / upgrade / provision
	// -> assign + record a decision observation.
	result := PlanResult{PlanID: planID}
	for i, ns := range persisted {
		roleHint := decomp.Steps[i].RoleHint
		spr := p.routeStep(ctx, ownerUserID, planID, ns, roleHint)
		result.Steps = append(result.Steps, spr)
	}
	return result, nil
}

// decomposeOrFallback runs the decomposer, validates its result, and falls
// back to a single-step plan on any failure -- the goal always runs.
func (p *Planner) decomposeOrFallback(ctx context.Context, goal string) DecomposeResult {
	raw, err := p.decomposer.Decompose(ctx, goal)
	if err != nil {
		p.logger.Warn("planner: decompose failed -- single-step fallback", "error", err)
		return SingleStepFallback(goal)
	}
	decomp, verr := ValidateDecompose(raw)
	if verr != nil {
		p.logger.Warn("planner: decompose invalid -- single-step fallback", "error", verr)
		return SingleStepFallback(goal)
	}
	return decomp
}

// routeStep makes the per-step route/upgrade/provision decision, performs
// the agent-side mutation it implies, sets step.assignedAgent, and records
// a `decision` observation. Returns the per-step summary.
func (p *Planner) routeStep(ctx context.Context, ownerUserID, planID string, ns NewStep, roleHint string) StepPlanResult {
	spr := StepPlanResult{StepID: ns.ID, Title: ns.Title}

	stepText := StepCapabilityText(ns.Title, ns.Goal)
	agents, err := p.roster.ListAgents(ctx, ownerUserID)
	if err != nil {
		p.logger.Warn("planner: list agents failed -- treating as empty roster",
			"plan", planID, "step", ns.ID, "error", err)
		agents = nil
	}
	fits, err := p.roster.FitScores(ctx, stepText, agents)
	if err != nil {
		p.logger.Warn("planner: fit scoring failed -- treating as no fit",
			"plan", planID, "step", ns.ID, "error", err)
		fits = nil
	}

	decision := DecideRoute(fits, p.cfg.Thresholds)
	spr.Action = decision.Action
	spr.Score = decision.Score
	spr.Rationale = decision.Rationale

	var assignedAgent string
	switch decision.Action {
	case ActionRoute:
		assignedAgent = decision.AgentID

	case ActionUpgrade:
		assignedAgent = p.applyUpgrade(ctx, planID, ns, roleHint, decision, agents)

	case ActionProvision:
		assignedAgent = p.applyProvision(ctx, ownerUserID, planID, ns, roleHint, agents)
	}

	spr.AssignedAgent = assignedAgent

	// Set step.assignedAgent so the reconciler dispatches it to the chosen
	// specialist. A provision/dedup that produced no agent (write failure)
	// leaves the step unassigned -- the reconciler still claims it under
	// the system actor, so the plan does not wedge.
	if assignedAgent != "" {
		if err := p.writer.AssignAgent(ctx, ns, assignedAgent); err != nil {
			p.logger.Warn("planner: assign agent failed",
				"plan", planID, "step", ns.ID, "agent", assignedAgent, "error", err)
		}
	}

	// Record the decision for auditability (acceptance criterion).
	p.recordDecision(ctx, planID, ns.ID, decision, assignedAgent)
	return spr
}

// applyUpgrade attaches the role's missing knowledge/tools to the
// partial-fit agent and returns its id. A no-op gap (agent already covers
// the role) just routes as-is.
func (p *Planner) applyUpgrade(ctx context.Context, planID string, ns NewStep, roleHint string, decision RouteDecision, agents []ExistingAgent) string {
	agentID := decision.AgentID
	role := PickSeedRole(roleHint, StepCapabilityText(ns.Title, ns.Goal), p.cfg.RoleCatalog)
	cur, ok := findAgent(agents, agentID)
	if !ok {
		// The agent vanished between fit-scoring and now; route as-is.
		return agentID
	}
	gap := ComputeUpgradeGap(role, cur.KnowledgeDomains, cur.Tools)
	if gap.Empty() {
		return agentID
	}
	domains, tools := MergeCapabilities(cur.KnowledgeDomains, cur.Tools, gap)
	if err := p.factory.UpgradeAgent(ctx, agentID, domains, tools, planID); err != nil {
		p.logger.Warn("planner: upgrade agent failed -- routing as-is",
			"plan", planID, "step", ns.ID, "agent", agentID, "error", err)
	}
	return agentID
}

// applyProvision composes a new agent from the seed role, dedups it
// against the roster (merge into a near-duplicate instead of creating
// sprawl), and returns the assigned agent id (the new one or the merge
// target). Returns "" only when the create write fails.
func (p *Planner) applyProvision(ctx context.Context, ownerUserID, planID string, ns NewStep, roleHint string, agents []ExistingAgent) string {
	role := PickSeedRole(roleHint, StepCapabilityText(ns.Title, ns.Goal), p.cfg.RoleCatalog)
	composed := ComposeAgent(role, ns.Title, ns.Goal)

	// Dedup: similarity-check the composed agent's capability text against
	// the roster; merge into a near-duplicate instead of persisting.
	dedupFits, err := p.roster.FitScores(ctx, composed.CapabilityText, agents)
	if err != nil {
		p.logger.Warn("planner: dedup scoring failed -- keeping new agent",
			"plan", planID, "step", ns.ID, "error", err)
		dedupFits = nil
	}
	dd := DecideDedup(dedupFits, p.cfg.Thresholds)
	if dd.Merge {
		p.logger.Info("planner: provision merged into existing agent (dedup)",
			"plan", planID, "step", ns.ID, "mergeInto", dd.MergeInto, "score", dd.Score)
		// Merge = treat the near-duplicate as the assignment, and upgrade
		// it with anything the composed role adds (so the merge target
		// gains any capability the new role needed).
		if cur, ok := findAgent(agents, dd.MergeInto); ok {
			gap := ComputeUpgradeGap(role, cur.KnowledgeDomains, cur.Tools)
			if !gap.Empty() {
				domains, tools := MergeCapabilities(cur.KnowledgeDomains, cur.Tools, gap)
				if uerr := p.factory.UpgradeAgent(ctx, dd.MergeInto, domains, tools, planID); uerr != nil {
					p.logger.Warn("planner: merge-upgrade failed",
						"plan", planID, "agent", dd.MergeInto, "error", uerr)
				}
			}
		}
		return dd.MergeInto
	}

	agentID, err := p.factory.CreateAgent(ctx, composed, ownerUserID, planID)
	if err != nil {
		p.logger.Warn("planner: create agent failed -- step left unassigned",
			"plan", planID, "step", ns.ID, "error", err)
		return ""
	}
	p.logger.Info("planner: provisioned new specialist",
		"plan", planID, "step", ns.ID, "agent", agentID, "role", role.Slug)
	return agentID
}

// recordDecision appends the route/upgrade/provision decision as a
// `decision` observation for auditability.
func (p *Planner) recordDecision(ctx context.Context, planID, stepID string, decision RouteDecision, assignedAgent string) {
	data := map[string]any{
		"choice":        string(decision.Action),
		"rationale":     decision.Rationale,
		"fitScore":      decision.Score,
		"assignedAgent": assignedAgent,
	}
	if decision.AgentID != "" {
		data["candidateAgent"] = decision.AgentID
	}
	content := fmt.Sprintf("step %s: %s (%s)", stepID, decision.Action, decision.Rationale)
	if err := p.writer.RecordDecision(ctx, Observation{
		StepID:  stepID,
		PlanID:  planID,
		Kind:    "decision",
		Content: content,
		Data:    data,
	}); err != nil {
		p.logger.Warn("planner: record decision failed",
			"plan", planID, "step", stepID, "error", err)
	}
}

// ---------------------------------------------------------------------------
// id minting + helpers
// ---------------------------------------------------------------------------

// newPlanID content-addresses a plan id from owner + goal so the same
// caller minting the same plan twice collapses (idempotent at the id
// level). Includes a discriminator so it never collides with a step id.
func newPlanID(ownerUserID, goal string) string {
	short := id.New().MustFromMap(map[string]any{
		"kind":  "harness-plan",
		"owner": ownerUserID,
		"goal":  goal,
	})
	return memorynodes.ConceptHarnessPlan + ":" + string(short)
}

// newStepID content-addresses a step id from plan + the local decompose
// key, stable across re-plans of the same goal.
func newStepID(planID, key string) string {
	short := id.New().MustFromMap(map[string]any{
		"kind": "harness-step",
		"plan": planID,
		"key":  key,
	})
	return memorynodes.ConceptHarnessStep + ":" + string(short)
}

// newIdempotencyKey derives the step's stable idempotency key (#582) from
// the plan + local key so a re-emit of the same logical step collapses.
func newIdempotencyKey(planID, key string) string {
	return string(id.New().FromString(planID + "|" + key))
}

// findAgent returns the agent with id agentID from the roster, or ok=false.
func findAgent(agents []ExistingAgent, agentID string) (ExistingAgent, bool) {
	for _, a := range agents {
		if a.ID == agentID {
			return a, true
		}
	}
	return ExistingAgent{}, false
}
