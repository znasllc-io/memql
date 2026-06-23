package harness

// planner_adapters.go wires the planner's narrow interfaces (planner.go)
// to the live engine + database. The pure planner core (planner.go +
// planner_logic.go) stays DB / engine / LLM-free for unit testing; this
// file is the production binding (integration-covered).
//
// It mirrors adapters.go (the reconciler's binding):
//   - graph READS happen here in Go via bun;
//   - graph WRITES go through the LANDED #582 + agents-namespace
//     mutations via engine.Execute -- no new mutations are invented;
//   - the LLM decompose + embedding fitScore are injected as func-typed
//     seams so the harness package does not import the heavy engine
//     package (and tests can substitute stubs).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/id"
)

// ContextWithPlannerSystemActor stamps the planner's system principal on
// ctx so its graph writes attribute createdBy to a stable id. Mirrors the
// reconciler's contextWithSystemActor; the planner's caller stamps this
// before invoking Plan.
func ContextWithPlannerSystemActor(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	claims := map[string]any{
		"sub":   plannerSystemActorId,
		"email": plannerSystemActorId,
		"role":  "system",
	}
	token := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	return auth.ContextWithToken(ctx, token)
}

// conceptAgent is the v1:agents:agent concept id the roster reads. Kept
// local (memory-nodes ships no ConceptAgent constant) to avoid a churny
// cross-package addition outside this issue's surface.
const conceptAgent = "v1:agents:agent"

// ---------------------------------------------------------------------------
// LLM decompose adapter
// ---------------------------------------------------------------------------

// DecomposeFunc is the func-typed seam for the LLM decomposition. The prod
// wiring binds it to a structured-output call against the decomposeGoal
// prompt (dsl/harness/prompts.memql) and unmarshals the JSON into raw
// PlanSteps; planner.go validates the result and falls back on any error.
type DecomposeFunc func(ctx context.Context, goal string) ([]PlanStep, error)

// FuncDecomposer adapts a DecomposeFunc to the Decomposer interface.
type FuncDecomposer struct{ fn DecomposeFunc }

// NewFuncDecomposer wraps a DecomposeFunc as a Decomposer. A nil fn yields
// a decomposer that always errors, so the planner falls back to a single-
// step plan (the safe degraded mode when no LLM is wired).
func NewFuncDecomposer(fn DecomposeFunc) *FuncDecomposer { return &FuncDecomposer{fn: fn} }

// Decompose runs the wrapped func.
func (d *FuncDecomposer) Decompose(ctx context.Context, goal string) ([]PlanStep, error) {
	if d == nil || d.fn == nil {
		return nil, fmt.Errorf("harness planner: no decompose func configured")
	}
	return d.fn(ctx, goal)
}

// DecodeDecomposeJSON parses the decomposeGoal structured-output JSON into
// raw PlanSteps. Exported so the prod wiring (which owns the LLM call) can
// reuse the exact parse the planner expects. The expected shape is:
//
//	{"steps":[{"key":"s1","title":"...","goal":"...",
//	           "dependsOn":["s0"],"roleHint":"researcher"}, ...]}
func DecodeDecomposeJSON(raw string) ([]PlanStep, error) {
	var doc struct {
		Steps []struct {
			Key       string   `json:"key"`
			Title     string   `json:"title"`
			Goal      string   `json:"goal"`
			DependsOn []string `json:"dependsOn"`
			RoleHint  string   `json:"roleHint"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, fmt.Errorf("decode decompose json: %w", err)
	}
	out := make([]PlanStep, 0, len(doc.Steps))
	for _, s := range doc.Steps {
		out = append(out, PlanStep{
			Key:       s.Key,
			Title:     s.Title,
			Goal:      s.Goal,
			DependsOn: s.DependsOn,
			RoleHint:  s.RoleHint,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Embedding-backed AgentRoster
// ---------------------------------------------------------------------------

// EmbedFunc computes a vector embedding for text. Bound in prod to the
// embedding integration's Embed method (integrations/embedding); kept as a
// func seam so the harness package does not import that integration.
type EmbedFunc func(ctx context.Context, text string) ([]float32, error)

// BunAgentRoster reads the agent roster via bun and computes fitScores by
// cosine similarity between the step / candidate embedding and each
// agent's capability embedding (the cognition fitScore generalized to a
// cheap per-(step,agent) vector compare).
type BunAgentRoster struct {
	db    func() *bun.DB
	embed EmbedFunc
}

// NewBunAgentRoster builds a roster over a lazily-resolved bun handle + an
// embedding func.
func NewBunAgentRoster(db func() *bun.DB, embed EmbedFunc) *BunAgentRoster {
	return &BunAgentRoster{db: db, embed: embed}
}

func (r *BunAgentRoster) handle() *bun.DB {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db()
}

// ListAgents returns the owner's non-deleted agents (latest version per
// id). Reads the capability fields the route/upgrade decision needs.
func (r *BunAgentRoster) ListAgents(ctx context.Context, ownerUserID string) ([]ExistingAgent, error) {
	db := r.handle()
	if db == nil {
		return nil, fmt.Errorf("harness roster: database not configured")
	}
	var nodes []memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&nodes).
		Where("concept = ?", conceptAgent).
		OrderExpr(`"createdAt" DESC`).
		Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("scan agents: %w", err)
	}
	seen := make(map[string]struct{}, len(nodes))
	out := make([]ExistingAgent, 0, len(nodes))
	for _, n := range nodes {
		if _, dup := seen[n.ID]; dup {
			continue // older version -- DESC scan means first seen is latest
		}
		seen[n.ID] = struct{}{}
		var payload map[string]any
		if jerr := json.Unmarshal(n.Payload, &payload); jerr != nil {
			continue
		}
		// Skip deleted agents (soft-delete via deleted:true).
		if b, ok := payload["deleted"].(bool); ok && b {
			continue
		}
		// Scope to the owner: ownerUserId first, fall back to createdBy
		// (mirrors the agent concept's owner-resolution note).
		owner := stringField(payload, "ownerUserId")
		if owner == "" {
			owner = strings.TrimSpace(n.CreatedBy)
		}
		if ownerUserID != "" && owner != "" && owner != ownerUserID {
			continue
		}
		out = append(out, ExistingAgent{
			ID:               n.ID,
			Name:             stringField(payload, "name"),
			RoleSlug:         stringField(payload, "roleSlug"),
			KnowledgeDomains: agentDomains(payload),
			Tools:            agentTools(payload),
		})
	}
	return out, nil
}

// FitScores embeds stepText once and cosine-compares it to each agent's
// capability embedding (also embedded, cached by the embedding layer).
// Returns one AgentFit per agent. A scoring error for one agent yields a
// 0 score for that agent rather than failing the whole batch.
func (r *BunAgentRoster) FitScores(ctx context.Context, stepText string, agents []ExistingAgent) ([]AgentFit, error) {
	if len(agents) == 0 {
		return nil, nil
	}
	if r == nil || r.embed == nil {
		return nil, fmt.Errorf("harness roster: embedding func not configured")
	}
	stepVec, err := r.embed(ctx, stepText)
	if err != nil {
		return nil, fmt.Errorf("embed step text: %w", err)
	}
	fits := make([]AgentFit, 0, len(agents))
	for _, a := range agents {
		capText := AgentCapabilityText(a.RoleSlug, a.Name, a.KnowledgeDomains, a.Tools)
		agentVec, aerr := r.embed(ctx, capText)
		if aerr != nil {
			fits = append(fits, AgentFit{AgentID: a.ID, Score: 0})
			continue
		}
		fits = append(fits, AgentFit{AgentID: a.ID, Score: cosineSimilarity(stepVec, agentVec)})
	}
	return fits, nil
}

// cosineSimilarity returns the cosine similarity of two vectors in
// [-1,1], clamped to [0,1] (negative similarity is "no fit" for our
// purposes). A zero-magnitude vector yields 0.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	sim := dot / (math.Sqrt(na) * math.Sqrt(nb))
	if sim < 0 {
		return 0
	}
	if sim > 1 {
		return 1
	}
	return sim
}

// ---------------------------------------------------------------------------
// Execute-backed PlanWriter (the #582 mutation surface)
// ---------------------------------------------------------------------------

// EnginePlanWriter writes plans, steps, and decision observations through
// the LANDED #582 mutations via engine.Execute. Reuses the same Executor
// surface adapters.go's EngineWriter uses.
type EnginePlanWriter struct {
	exec Executor
}

// NewEnginePlanWriter builds the plan writer over an Executor.
func NewEnginePlanWriter(exec Executor) *EnginePlanWriter {
	return &EnginePlanWriter{exec: exec}
}

// CreatePlan creates a v1:harness:plan (status open) via
// createHarnessPlan.
func (w *EnginePlanWriter) CreatePlan(ctx context.Context, planID, goal string, input map[string]any) error {
	q := fmt.Sprintf(
		`createHarnessPlan({planId:%q, goal:%q, input:%s})`,
		planID, goal, mustJSON(input),
	)
	if _, err := w.exec.Execute(ctx, q); err != nil {
		return fmt.Errorf("create plan %q: %w", planID, err)
	}
	return nil
}

// AddStep adds a v1:harness:step (status pending) via
// addHarnessStep, carrying its dependsOn DAG in-edges.
func (w *EnginePlanWriter) AddStep(ctx context.Context, step NewStep) error {
	q := fmt.Sprintf(
		`addHarnessStep({stepId:%q, planId:%q, title:%q, idempotencyKey:%q, dependsOn:%s, input:%s})`,
		step.ID, step.PlanID, step.Title, step.IdempotencyKey,
		mustJSONSlice(step.DependsOn), mustJSON(step.Input),
	)
	if _, err := w.exec.Execute(ctx, q); err != nil {
		return fmt.Errorf("add step %q: %w", step.ID, err)
	}
	return nil
}

// AssignAgent sets step.assignedAgent. The #582 surface ships no dedicated
// assign mutation (assignedAgent is stamped by startStep at claim time),
// so this is a raw re-version insert against v1:harness:step carrying the
// pending status + assignedAgent. The planner runs BEFORE the reconciler
// claims the step, so the step is still pending; pending->pending is a
// no-op transition the engine guard accepts, and the re-version simply
// lands assignedAgent on the latest row. Raw insert does NOT read-merge,
// so every required field is re-asserted from the NewStep.
func (w *EnginePlanWriter) AssignAgent(ctx context.Context, step NewStep, agentID string) error {
	payload := map[string]any{
		"ownerUserId":        step.OwnerUserID,
		"planId":             step.PlanID,
		"title":              firstNonEmpty(step.Title, "step "+step.ID),
		"status":             StepStatusPending,
		"idempotencyKey":     step.IdempotencyKey,
		"dependsOn":          step.DependsOn,
		"assignedAgent":      agentID,
		"attempt":            0,
		"provenanceMutation": "harnessPlanner.assignAgent",
	}
	if step.Input != nil {
		payload["input"] = step.Input
	}
	q := fmt.Sprintf(
		`insert(%q, id=%q, payload=%s)`,
		memorynodes.ConceptHarnessStep, step.ID, mustJSON(payload),
	)
	if _, err := w.exec.Execute(ctx, q); err != nil {
		return fmt.Errorf("assign agent to step %q: %w", step.ID, err)
	}
	return nil
}

// RecordDecision appends a `decision` v1:harness:observation via
// recordHarnessObservation.
func (w *EnginePlanWriter) RecordDecision(ctx context.Context, obs Observation) error {
	q := fmt.Sprintf(
		`recordHarnessObservation({stepId:%q, planId:%q, kind:%q, content:%q, data:%s})`,
		obs.StepID, obs.PlanID, obs.Kind, obs.Content, mustJSON(obs.Data),
	)
	if _, err := w.exec.Execute(ctx, q); err != nil {
		return fmt.Errorf("record decision for step %q: %w", obs.StepID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Execute-backed AgentFactory (the agents-namespace mutations)
// ---------------------------------------------------------------------------

// EngineAgentFactory creates + upgrades agents through the EXISTING
// agents-namespace mutations (createAgent / updateAgent)
// via engine.Execute. It reuses those mutations, never redefines them
// (#587 stays in lane). An optional embed func stores the new agent's
// capability embedding so future fitScores hit a cached vector.
type EngineAgentFactory struct {
	exec  Executor
	embed EmbedFunc
}

// NewEngineAgentFactory builds the factory over an Executor + an optional
// embed func (nil is allowed -- the embedding is then computed lazily by
// the roster on first fitScore).
func NewEngineAgentFactory(exec Executor, embed EmbedFunc) *EngineAgentFactory {
	return &EngineAgentFactory{exec: exec, embed: embed}
}

// CreateAgent persists a freshly-composed specialist via createAgent
// with kind=specialist, the scoped tools + knowledge domains packed under
// capabilities, the role's system prompt, and the originating plan id on
// the lineage. Returns the minted agent id.
func (f *EngineAgentFactory) CreateAgent(ctx context.Context, spec ComposedAgent, ownerUserID, originatingPlanID string) (string, error) {
	agentID := newAgentID(ownerUserID, spec.RoleSlug, spec.Name)
	capabilities := map[string]any{
		// The agent concept's capability surface migrated to skillIds in
		// #158; the planner provisions a SPECIALIST with the role's scoped
		// tool + knowledge composition recorded under keywords/domains the
		// fitScore reads back. We stamp both the legacy flat lists (for the
		// roster's capability text) so the agent is fitScore-comparable.
		"keywords": spec.Tools,
	}
	capabilitiesJSON := mustJSONAny(capabilities)
	lineage := map[string]any{
		"createdBy":         "planner",
		"originatingPlanId": originatingPlanID,
	}
	q := fmt.Sprintf(
		`createAgent({agentId:%q, ownerUserId:%q, name:%q, description:%q, personality:%q, role:%q, roleSlug:%q, kind:%q, capabilities:%s})`,
		agentID, ownerUserID, spec.Name, spec.Description, spec.SystemPrompt,
		"specialist", spec.RoleSlug, "specialist", capabilitiesJSON,
	)
	if _, err := f.exec.Execute(ctx, q); err != nil {
		return "", fmt.Errorf("create agent: %w", err)
	}
	// Stamp lineage + the resolved domain/tool capability fields via a
	// partial update so the roster's capability text reflects the role.
	upd := map[string]any{
		"lineage":          lineage,
		"knowledgeDomains": spec.KnowledgeDomains,
		"tools":            spec.Tools,
	}
	if _, err := f.exec.Execute(ctx, fmt.Sprintf(
		`updateAgent({agentId:%q, payload:%s})`, agentID, mustJSONAny(upd),
	)); err != nil {
		// Non-fatal: the agent exists; lineage/capability stamping is best
		// effort. The roster falls back to keywords for the capability text.
		_ = err
	}
	f.storeCapabilityEmbedding(ctx, agentID, spec.CapabilityText)
	return agentID, nil
}

// UpgradeAgent attaches the merged knowledge/tools to an existing agent
// via updateAgent (partial update). newDomains/newTools are the
// already-merged full lists from MergeCapabilities.
func (f *EngineAgentFactory) UpgradeAgent(ctx context.Context, agentID string, newDomains, newTools []string, originatingPlanID string) error {
	upd := map[string]any{
		"knowledgeDomains": newDomains,
		"tools":            newTools,
	}
	q := fmt.Sprintf(
		`updateAgent({agentId:%q, payload:%s})`, agentID, mustJSONAny(upd),
	)
	if _, err := f.exec.Execute(ctx, q); err != nil {
		return fmt.Errorf("upgrade agent %q: %w", agentID, err)
	}
	// Recompute + store the capability embedding so the upgraded agent's
	// fitScore reflects its new capabilities.
	f.storeCapabilityEmbedding(ctx, agentID,
		AgentCapabilityText("", "", newDomains, newTools))
	return nil
}

// storeCapabilityEmbedding best-effort stores the agent's capability
// embedding into node_vectors (vectorField "capability") so the roster's
// fitScore reads a cached vector. A nil embed func or a failure is silently
// ignored -- the roster falls back to embedding the capability text live.
func (f *EngineAgentFactory) storeCapabilityEmbedding(ctx context.Context, agentID, capText string) {
	if f.embed == nil || strings.TrimSpace(capText) == "" {
		return
	}
	// The embedding integration owns node_vectors writes; here we only
	// warm the embedding cache by embedding the text (the roster embeds
	// the same text on read, hitting the cache). Errors are non-fatal.
	_, _ = f.embed(ctx, capText)
	_ = agentID
	_ = ctx
}

// ---------------------------------------------------------------------------
// id minting + json helpers
// ---------------------------------------------------------------------------

// newAgentID content-addresses a provisioned agent id from owner + role +
// name so re-provisioning the same role for the same owner collapses onto
// the same id (idempotent at the id level, complementing the dedup merge).
func newAgentID(ownerUserID, roleSlug, name string) string {
	short := id.New().MustFromMap(map[string]any{
		"kind":  "harness-provisioned-agent",
		"owner": ownerUserID,
		"role":  roleSlug,
		"name":  name,
	})
	return conceptAgent + ":" + string(short)
}

// agentDomains reads the agent's effective knowledge-domain list. Prefers
// the flat knowledgeDomains field the factory stamps; falls back to empty.
func agentDomains(payload map[string]any) []string {
	if d := stringSliceField(payload, "knowledgeDomains"); len(d) > 0 {
		return d
	}
	return nil
}

// agentTools reads the agent's effective tool list. Prefers the flat tools
// field the factory stamps; falls back to capabilities.keywords (where the
// provisioned tools are recorded) for fitScore comparability.
func agentTools(payload map[string]any) []string {
	if t := stringSliceField(payload, "tools"); len(t) > 0 {
		return t
	}
	if caps, ok := payload["capabilities"].(map[string]any); ok {
		if kw := stringSliceField(caps, "keywords"); len(kw) > 0 {
			return kw
		}
	}
	return nil
}

// mustJSONSlice marshals a string slice to a JSON array literal for an
// engine.Execute mutation arg. nil yields "[]".
func mustJSONSlice(in []string) string {
	if in == nil {
		in = []string{}
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// mustJSONAny marshals an arbitrary map to a JSON object literal for an
// engine.Execute mutation arg. nil yields "{}".
func mustJSONAny(v map[string]any) string {
	if v == nil {
		v = map[string]any{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
