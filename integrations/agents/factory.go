package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// ensureForGoalCapName is the integration capability name the
// ensureAgent tool resolves to via its builtin wrapper. Lives in this
// package so the registration in Capabilities() and the handler share
// the constant.
const ensureForGoalCapName = "ensureForGoal"

// factoryResultConcept is the MemoryNode concept the handler returns.
// Same pattern as envelopeConcept -- an in-flight integration result,
// never persisted to a row.
const factoryResultConcept = "integration:agents:factory-result"

// handleEnsureForGoal is the executor for the ensureAgent factory
// tool. Wired through dsl/agents/builtins.memql's
// `ensureAgentForGoal` builtin which the tool's @handler resolves.
//
// Args contract (matches the tool decl + builtin):
//
//	goal         string  required -- the user-stated goal
//	ownerUserId  string  required -- target agent owner
//	partitionId      string  optional -- forwarded to the analysis prompt
//	planId       string  optional -- planner-driven callers pass the
//	                                  originating v1:planner:plan.id so
//	                                  createAgent can stamp
//	                                  lineage.originatingPlanId on the new
//	                                  specialist (memql#399). GA-driven
//	                                  ensureAgent tool calls omit it.
//
// Behavior:
//
//  1. Load the user's existing agents via activeAgentsForUser.
//  2. Load the role catalog via activeAgentRoles.
//  3. Call ai("agentFactoryAnalyze", {...}) for the structured
//     decision -- {action, targetAgentId, roleSlug, domainIds,
//     liveSourceIds, toolSlugs, reasoning}.
//  4. Dispatch on action:
//     - "match":   return the targetAgentId unchanged.
//     - "extend":  union the proposed domains + tools onto the
//     target's current capabilities and write via
//     updateAgent (the lock validator blocks
//     any removal of locked items).
//     - "create":  compose capabilities from the role's locked +
//     default + proposed additions, then write a new
//     agent via createAgent.
//  5. Return ONE MemoryNode whose payload JSON is
//     {agentId, agentName, roleSlug, action, reasoning}.
//
// Failure modes:
//   - Missing required arg              -> typed error.
//   - Analysis returns invalid JSON     -> wrapping error with the raw text.
//   - Analysis names a non-existent     -> error pointing the caller at
//     role/agent                          activeAgentRoles / activeAgentsForUser.
//   - Lock violation on extend          -> propagated from the
//     validator (the GA can retry
//     with action="create").
func (i *Integration) handleEnsureForGoal(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.agents == nil {
		return nil, fmt.Errorf("agents integration not initialized -- AgentRegistry is nil")
	}
	if i.engine == nil {
		return nil, fmt.Errorf("agents integration not initialized -- engine handle missing")
	}

	goal, _ := args["goal"].(string)
	if strings.TrimSpace(goal) == "" {
		return nil, fmt.Errorf("ensureForGoal: 'goal' argument is required")
	}
	ownerUserId, _ := args["ownerUserId"].(string)
	if strings.TrimSpace(ownerUserId) == "" {
		return nil, fmt.Errorf("ensureForGoal: 'ownerUserId' argument is required")
	}
	planId, _ := args["planId"].(string)

	// Step 1-2: load the user's agents + the role catalog. Both are
	// best-effort -- the analysis prompt tolerates an empty slice.
	existing := i.loadExistingAgents(ctx, ownerUserId)
	roleCatalog := i.loadRoleCatalog(ctx)

	// Step 3: structured analysis.
	decision, err := i.analyzeGoal(ctx, goal, existing, roleCatalog)
	if err != nil {
		return nil, fmt.Errorf("ensureForGoal: analyze: %w", err)
	}

	// Step 4: dispatch.
	var agentId, agentName, roleSlug, action string
	switch decision.Action {
	case "match":
		if decision.TargetAgentId == "" {
			return nil, fmt.Errorf("ensureForGoal: action=match but targetAgentId is empty")
		}
		match, ok := findById(existing, decision.TargetAgentId)
		if !ok {
			return nil, fmt.Errorf("ensureForGoal: action=match targetAgentId %q not found in user's agents", decision.TargetAgentId)
		}
		agentId = match.Id
		agentName = match.Name
		roleSlug = match.RoleSlug
		action = "match"
	case "extend":
		updated, err := i.extendAgent(ctx, ownerUserId, existing, decision, planId)
		if err != nil {
			return nil, err
		}
		agentId = updated.Id
		agentName = updated.Name
		roleSlug = updated.RoleSlug
		action = "extend"
	case "create":
		created, err := i.createAgent(ctx, ownerUserId, decision, roleCatalog, planId)
		if err != nil {
			return nil, err
		}
		agentId = created.Id
		agentName = created.Name
		roleSlug = created.RoleSlug
		action = "create"
	default:
		return nil, fmt.Errorf("ensureForGoal: analyze returned unknown action %q (expected match|extend|create)", decision.Action)
	}

	resultPayload := map[string]any{
		"agentId":   agentId,
		"agentName": agentName,
		"roleSlug":  roleSlug,
		"action":    action,
		"reasoning": decision.Reasoning,
	}
	resultBytes, err := json.Marshal(resultPayload)
	if err != nil {
		return nil, fmt.Errorf("ensureForGoal: marshal result: %w", err)
	}
	node := memorynodes.MemoryNode{
		ID:        fmt.Sprintf("agents-factory:%s:%d", agentId, time.Now().UnixNano()),
		Concept:   factoryResultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: systemActorId,
		Payload:   resultBytes,
	}
	return []memorynodes.MemoryNode{node}, nil
}

// factoryDecision mirrors the JSON schema the agentFactoryAnalyze
// prompt is contracted to return. Loosely typed: extra fields are
// ignored, missing optional fields are zero values. Phase 2 cut
// (#158): SkillIds replaces the per-domain / per-tool lists.
type factoryDecision struct {
	Action        string   `json:"action"`
	TargetAgentId string   `json:"targetAgentId"`
	RoleSlug      string   `json:"roleSlug"`
	SkillIds      []string `json:"skillIds"`
	Reasoning     string   `json:"reasoning"`
}

// agentSnapshot is the compact view of an existing v1:agents:agent
// row the factory needs for analysis + extension. SkillIds is the
// canonical capability surface (Phase 2 cut #158); the legacy Domains
// / Tools fields are resolved at snapshot time via SkillResolver so
// the dedupe + extension code paths that read them still work without
// rewriting every callsite.
type agentSnapshot struct {
	Id           string
	Name         string
	RoleSlug     string
	Kind         string // "assistant" | "specialist" | "system" (memql#398) -- read from agent.kind so loadExistingAgents can filter platform infrastructure (Kind=="system") out of the dedupe candidate pool. Planner-created specialists carry Kind=="specialist" (memql#399); user-created agents via the frontend's agent-creation modal carry Kind=="assistant".
	SkillIds     []string
	Domains      []string // resolved union across SkillIds
	Tools        []string // resolved union across SkillIds
	OwnerUserId  string
	Capabilities map[string]any // raw capabilities sub-object for partial-update merge
}

// roleSnapshot is the compact view of a v1:agents:agentRole row.
// Phase 2 cut (#158): the seven flat lockedDomain / defaultDomain /
// availableDomain / lockedTool / defaultTool / forbiddenTool /
// lockedLiveKnowledge fields collapse onto the five skill-id fields
// the catalog now carries.
type roleSnapshot struct {
	Slug              string
	Name              string
	Category          string
	Tier              string
	LockedSkillIds    []string
	DefaultSkillIds   []string
	AvailableSkillIds []string
	// ForbiddenSkillIds is the role's explicit opt-out list -- skill
	// ids the agent factory MUST NOT grant even when its default
	// behavior would. Mirrors the old forbiddenToolSlugs semantic;
	// use sparingly for Tier-C medical roles that should never carry
	// operator-computer-use, regulated finance roles whose audit
	// story can't tolerate workbench shell access, etc.
	ForbiddenSkillIds     []string
	MaxSkills             int
	RecommendedPolicySlug string
	SystemPromptHints     string
	// Predefined marks a row seeded from dsl/agents/roles/*.memql. Read here so
	// slug resolution can prefer the seeded catalog row over a user row that
	// claims the same slug (memql#3066) -- see findRoleBySlug.
	Predefined bool
	// ID is the row id, carried ONLY as the tie-break for findRoleBySlug. Two
	// rows that are equal on Predefined have to resolve the same way every
	// time, and row order out of an @cache(300) query is not something to
	// depend on -- so the tie-break is a property of the rows themselves.
	ID string
}

// loadExistingAgents walks the user's active agents. Best-effort:
// failures (no engine, query error, no rows) yield an empty slice
// so the analysis prompt still runs and `action: "create"` is
// always achievable.
//
// Rows whose `kind` is "system" (MemQL Planner, MemQL Trainer, etc.)
// are filtered out: they are platform infrastructure, never valid
// match/extend dedupe targets for a user goal. The schema default
// is "user" so legacy rows pre-dating the kind field are kept.
func (i *Integration) loadExistingAgents(ctx context.Context, ownerUserId string) []agentSnapshot {
	query := fmt.Sprintf(`query activeAgentsForUser(ownerUserId: %q)`, ownerUserId)
	raw, err := i.engine.Execute(ctx, query)
	if err != nil || raw == nil {
		return nil
	}
	rows := extractRowsFromExecuteResult(raw)
	out := make([]agentSnapshot, 0, len(rows))
	for _, row := range rows {
		s, ok := agentSnapshotFromRow(row)
		if !ok {
			continue
		}
		if s.Kind == "system" {
			continue
		}
		s.OwnerUserId = ownerUserId
		out = append(out, s)
	}
	return out
}

// loadRoleCatalog walks the v1:agents:agentRole catalog. Same
// best-effort tolerance as loadExistingAgents.
func (i *Integration) loadRoleCatalog(ctx context.Context) []roleSnapshot {
	raw, err := i.engine.Execute(ctx, `activeAgentRoles()`)
	if err != nil || raw == nil {
		return nil
	}
	rows := extractRowsFromExecuteResult(raw)
	out := make([]roleSnapshot, 0, len(rows))
	for _, row := range rows {
		s, ok := roleSnapshotFromRow(row)
		if !ok {
			continue
		}
		out = append(out, s)
	}
	return out
}

// analyzeGoal invokes the agentFactoryAnalyze prompt via
// InvokeAIStructured. Returns the parsed decision struct.
func (i *Integration) analyzeGoal(ctx context.Context, goal string, existing []agentSnapshot, roles []roleSnapshot) (factoryDecision, error) {
	data := map[string]any{
		"goal":              goal,
		"existingAgents":    existingForPrompt(existing),
		"roleCatalog":       roleCatalogForPrompt(roles),
		"domainCatalog":     []any{}, // populated by a future commit; the prompt tolerates empty
		"liveSourceCatalog": []any{},
		"toolCatalog":       []any{},
		"now":               time.Now().UTC().Format(time.RFC3339),
	}
	rawJSON, err := i.engine.InvokeAIStructured(
		ctx,
		"agentFactoryAnalyze",
		data,
		"agentFactoryDecision",
		factoryDecisionSchema,
		true, // strict
	)
	if err != nil {
		return factoryDecision{}, fmt.Errorf("invoke agentFactoryAnalyze: %w", err)
	}
	var decision factoryDecision
	if err := json.Unmarshal([]byte(rawJSON), &decision); err != nil {
		return factoryDecision{}, fmt.Errorf("parse agentFactoryAnalyze output: %w (raw: %s)", err, rawJSON)
	}
	return decision, nil
}

// extendAgent unions the decision's skillIds onto the target agent's
// current capabilities.skillIds and writes via updateAgent.
// Lock removal is rejected server-side by validateAgentLockedItems;
// extensions only ADD ids, never remove, so the path is safe. The
// effective max-skills cap is also enforced server-side -- a decision
// that would push the agent over the cap rejects loudly.
//
// After the agent-row update lands, one v1:agents:skillChangeEvent row
// is appended per skill that is net-new on the agent post-extend (in
// the merged set but not the pre-extend set). This is the audit trail
// the Tasks UI reads to render "extended for Plan X" (memql#405).
// Attribution branches on planId presence -- the same discriminator
// createAgent uses for lineage (memql#399):
//   - planId != "": planner-driven extend. actorAgentId is the
//     per-user Planner Agent (`plannerAgent-<userId>`); planId carries
//     the originating Plan id. (The planner invokes the factory under a
//     system-actor context, so the context actor is NOT the right
//     attribution -- the per-user planner agent id is.)
//   - planId == "": GA-driven extend (the ensureAgent tool, invoked
//     from a user turn). actorUserId is the agent owner; planId empty.
//
// Kind audit (memql#398/#405): a target row carrying the retired
// kind enum value ("user" or "") can only survive on a stale dev DB.
// We warn-log and skip the kind-related work -- the row is already
// orphaned by the schema enforcement and the dev should reset their
// DB; the factory does NOT try to repair it via update.
func (i *Integration) extendAgent(ctx context.Context, ownerUserId string, existing []agentSnapshot, decision factoryDecision, planId string) (agentSnapshot, error) {
	if decision.TargetAgentId == "" {
		return agentSnapshot{}, fmt.Errorf("ensureForGoal: action=extend but targetAgentId is empty")
	}
	target, ok := findById(existing, decision.TargetAgentId)
	if !ok {
		return agentSnapshot{}, fmt.Errorf("ensureForGoal: action=extend targetAgentId %q not found", decision.TargetAgentId)
	}

	// Kind audit-pass: a pre-#398 row with the retired enum value gets
	// a warning, then the kind-related work is skipped. (Extend touches
	// no kind field today, so "skip" is a documented no-op -- the
	// warning is the load-bearing part: it tells the dev the row is
	// orphaned by schema enforcement.)
	if target.Kind == "user" || target.Kind == "" {
		slog.Default().Warn("agents factory: extendAgent target carries retired kind enum value; skipping kind work (reset your dev DB)",
			"component", "agents-factory",
			"agentId", target.Id,
			"kind", target.Kind)
	}

	preExtendSkills := append([]string(nil), target.SkillIds...)
	mergedSkills := unionStrings(target.SkillIds, decision.SkillIds)
	caps := map[string]any{}
	for k, v := range target.Capabilities {
		caps[k] = v
	}
	caps["skillIds"] = mergedSkills
	payload := map[string]any{"capabilities": caps}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return agentSnapshot{}, fmt.Errorf("marshal update payload: %w", err)
	}
	query := fmt.Sprintf(`mutation updateAgent(agentId: %q, payload: %s)`, target.Id, string(payloadJSON))
	if _, err := i.engine.Execute(ctx, query); err != nil {
		return agentSnapshot{}, fmt.Errorf("execute updateAgent: %w", err)
	}

	// Snapshot the resolved before/after capability shape for the audit
	// rows. Best-effort -- a resolve failure leaves the snapshot empty;
	// the event's skillId + actor attribution is the load-bearing part.
	before := resolvedCapShape(ctx, i.engine, preExtendSkills)

	target.SkillIds = mergedSkills
	// Refresh the resolved Domains / Tools so callers reading them
	// see the post-extension surface.
	after := map[string]any{}
	if bundle, rerr := i.engine.ResolveSkills(ctx, mergedSkills); rerr == nil {
		target.Domains = bundle.DomainIds
		target.Tools = bundle.ToolSlugs
		after = capShapeFromBundle(bundle.DomainIds, bundle.ToolSlugs, bundle.LiveSourceIds)
	}

	// Append one skillChangeEvent per net-new skill. Failures here are
	// non-fatal: the agent-row update already landed and is the source
	// of truth; the audit log is denormalized history. We surface a
	// warning rather than aborting the extend.
	netNew := diffStrings(mergedSkills, preExtendSkills)
	for _, skillId := range netNew {
		evArgs := buildSkillChangeEventArgs(id.NewShortId(), target.Id, skillId, ownerUserId, planId, before, after)
		evJSON, merr := json.Marshal(evArgs)
		if merr != nil {
			slog.Default().Warn("agents factory: marshal skillChangeEvent args failed; skipping audit row",
				"component", "agents-factory", "agentId", target.Id, "skillId", skillId, "error", merr)
			continue
		}
		evQuery := fmt.Sprintf(`createSkillChangeEvent(%s)`, string(evJSON))
		if _, eerr := i.engine.Execute(ctx, evQuery); eerr != nil {
			slog.Default().Warn("agents factory: write skillChangeEvent failed; agent update already committed",
				"component", "agents-factory", "agentId", target.Id, "skillId", skillId, "error", eerr)
		}
	}
	return target, nil
}

// buildSkillChangeEventArgs composes the args map for
// createSkillChangeEvent recording one net-new skill landing on
// an agent during an extend. Extracted (mirroring buildCreateAgentArgs,
// memql#399) so a unit test can pin the attribution contract without an
// engine handle (memql#405).
//
// Attribution branches on planId presence:
//   - planId != "": planner-driven. actorAgentId = `plannerAgent-<userId>`
//     (the per-user Planner Agent), actorUserId empty, planId stamped.
//   - planId == "": GA-driven. actorUserId = ownerUserId, actorAgentId
//     empty, planId empty.
//
// The skillChangeEvent concept declares the two actor fields as mutually
// exclusive (exactly one set per event); this builder honors that.
func buildSkillChangeEventArgs(eventId, targetAgentId, skillId, ownerUserId, planId string, before, after map[string]any) map[string]any {
	args := map[string]any{
		"skillChangeEventId": eventId,
		"targetAgentId":      targetAgentId,
		"skillId":            skillId,
		"changeKind":         "attached",
		"before":             before,
		"after":              after,
	}
	if planId != "" {
		args["actorAgentId"] = plannerAgentId(ownerUserId)
		args["planId"] = planId
	} else {
		args["actorUserId"] = ownerUserId
		args["planId"] = ""
	}
	return args
}

// plannerAgentId derives the per-user Planner Agent's deterministic id
// from the owner user id. Mirrors the seed materializer's
// `<seedName>-<userShortId>` form (component/memql/seed_materializer.go:
// deterministicPerUserSeedId) -- the canonical user prefix is stripped
// so the tail matches the materialized row's id.
func plannerAgentId(ownerUserId string) string {
	return "plannerAgent-" + strings.TrimPrefix(ownerUserId, "v1:identity:user:")
}

// resolvedCapShape resolves the given skill ids to a capability-shape
// snapshot {domainIds, toolSlugs, liveSourceIds}. Best-effort: a
// resolve failure (or empty input) yields an empty shape.
func resolvedCapShape(ctx context.Context, engine memql.IntegrationEngineAccess, skillIds []string) map[string]any {
	if engine == nil || len(skillIds) == 0 {
		return map[string]any{}
	}
	bundle, err := engine.ResolveSkills(ctx, skillIds)
	if err != nil {
		return map[string]any{}
	}
	return capShapeFromBundle(bundle.DomainIds, bundle.ToolSlugs, bundle.LiveSourceIds)
}

func capShapeFromBundle(domainIds, toolSlugs, liveSourceIds []string) map[string]any {
	return map[string]any{
		"domainIds":     append([]string(nil), domainIds...),
		"toolSlugs":     append([]string(nil), toolSlugs...),
		"liveSourceIds": append([]string(nil), liveSourceIds...),
	}
}

// diffStrings returns the members of a that are not present in b,
// preserving a's order and skipping empties / duplicates.
func diffStrings(a, b []string) []string {
	exclude := make(map[string]struct{}, len(b))
	for _, s := range b {
		exclude[s] = struct{}{}
	}
	seen := make(map[string]struct{}, len(a))
	out := make([]string, 0, len(a))
	for _, s := range a {
		if s == "" {
			continue
		}
		if _, skip := exclude[s]; skip {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// createAgent composes the new agent's capabilities from the role
// catalog row + the analysis-proposed additions and writes via
// createAgent. roleCatalog is the snapshot loaded above so
// we don't re-query. planId is the originating v1:planner:plan.id for
// planner-driven calls; the empty string means GA-driven (no plan
// back-pointer to stamp).
//
// Stamping: kind="specialist" is hard-coded -- the factory only ever
// creates specialists (GA-driven ensureAgent has the same semantics,
// and the assistant + system agents come from their own seed
// materializers). lineage.originatingPlanId is stamped when planId is
// non-empty; the lineage.createdBy bucket is "planner" for plan-driven
// calls and "user" otherwise (the GA's ensureAgent tool is invoked
// from a user turn).
// buildCreateAgentArgs composes the args map for createAgent
// from a factory decision + role snapshot + optional planId. Extracted
// from createAgent so tests can pin the contract (kind="specialist",
// lineage stamping per planId presence) without needing an engine
// handle (memql#399).
func buildCreateAgentArgs(agentId, ownerUserId string, decision factoryDecision, role roleSnapshot, planId string) map[string]any {
	skillIds := unionStrings(role.LockedSkillIds, role.DefaultSkillIds)
	skillIds = unionStrings(skillIds, decision.SkillIds)

	caps := map[string]any{
		"skillIds": skillIds,
		"keywords": []string{},
		// Default capability bools follow the role-baseline pattern.
		// Future iteration may push these onto the role catalog
		// (recommended capabilities); today, conservative defaults.
		"avatar":       false,
		"lipSync":      false,
		"vision":       false,
		"voiceToVoice": false,
		"claw":         false,
	}
	provider := map[string]any{
		"llm": map[string]any{"policyName": coalesceString(role.RecommendedPolicySlug, "balancedChat")},
	}
	// Lineage attribution. The createdBy bucket is the agent.lineage
	// field (NOT the row intrinsic createdBy that the engine stamps from
	// the actor) -- it records the bootstrap source ("user" vs "planner"
	// vs "system"). The factory is invoked from both the planner loop
	// (planId set) and the GA ensureAgent tool (planId empty), so we
	// derive the bucket from planId presence.
	lineageBucket := "user"
	if planId != "" {
		lineageBucket = "planner"
	}
	lineage := map[string]any{
		"createdBy": lineageBucket,
	}
	if planId != "" {
		lineage["originatingPlanId"] = planId
	}
	return map[string]any{
		"agentId":        agentId,
		"ownerUserId":    ownerUserId,
		"name":           role.Name, // user can rename later
		"description":    role.SystemPromptHints,
		"kind":           "specialist",
		"role":           "specialist",
		"roleSlug":       role.Slug,
		"capabilities":   caps,
		"providerConfig": provider,
		"lineage":        lineage,
		"active":         true,
	}
}

func (i *Integration) createAgent(ctx context.Context, ownerUserId string, decision factoryDecision, roleCatalog []roleSnapshot, planId string) (agentSnapshot, error) {
	role, ok := findRoleBySlug(roleCatalog, decision.RoleSlug)
	if !ok {
		return agentSnapshot{}, fmt.Errorf("ensureForGoal: action=create but roleSlug %q not in catalog", decision.RoleSlug)
	}
	agentId := id.NewShortId()
	insertArgs := buildCreateAgentArgs(agentId, ownerUserId, decision, role, planId)
	skillIds, _ := insertArgs["capabilities"].(map[string]any)["skillIds"].([]string)
	argsJSON, err := json.Marshal(insertArgs)
	if err != nil {
		return agentSnapshot{}, fmt.Errorf("marshal create args: %w", err)
	}
	query := fmt.Sprintf(`createAgent(%s)`, string(argsJSON))
	if _, err := i.engine.Execute(ctx, query); err != nil {
		return agentSnapshot{}, fmt.Errorf("execute createAgent: %w", err)
	}
	snap := agentSnapshot{
		Id:       agentId,
		Name:     role.Name,
		RoleSlug: role.Slug,
		SkillIds: skillIds,
	}
	if bundle, rerr := i.engine.ResolveSkills(ctx, skillIds); rerr == nil {
		snap.Domains = bundle.DomainIds
		snap.Tools = bundle.ToolSlugs
	}
	return snap, nil
}

// factoryDecisionSchema is the JSON Schema enforced on the
// agentFactoryAnalyze output. Strict so the prompt's contract is
// load-bearing rather than aspirational. Phase 2 cut (#158): the
// model now returns skillIds instead of separate domainIds/toolSlugs/
// liveSourceIds lists -- the skill catalog is the bundling surface.
var factoryDecisionSchema = json.RawMessage(`{
  "type": "object",
  "required": ["action", "roleSlug", "skillIds", "reasoning"],
  "additionalProperties": false,
  "properties": {
    "action":         {"type": "string", "enum": ["match", "extend", "create"]},
    "targetAgentId":  {"type": "string"},
    "roleSlug":       {"type": "string"},
    "skillIds":       {"type": "array", "items": {"type": "string"}},
    "reasoning":      {"type": "string"}
  }
}`)

// extractRowsFromExecuteResult pulls a slice of row-maps out of the
// engine.Execute return. Tolerant of the two shapes the engine
// emits: a wrapped {result: {rows: [...]}} envelope or a bare slice.
func extractRowsFromExecuteResult(raw any) []map[string]any {
	if raw == nil {
		return nil
	}
	// Try the *ExecuteResult shape via reflection-free marshalling.
	// Easiest: JSON-roundtrip and walk the loose shape.
	rawJSON, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var loose any
	if err := json.Unmarshal(rawJSON, &loose); err != nil {
		return nil
	}
	return rowsFromLoose(loose)
}

func rowsFromLoose(v any) []map[string]any {
	switch x := v.(type) {
	case map[string]any:
		if rows, ok := x["rows"].([]any); ok {
			return castRows(rows)
		}
		if result, ok := x["result"].(map[string]any); ok {
			if rows, ok := result["rows"].([]any); ok {
				return castRows(rows)
			}
		}
		if nodes, ok := x["nodes"].([]any); ok {
			return castRows(nodes)
		}
	case []any:
		return castRows(x)
	}
	return nil
}

func castRows(items []any) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func agentSnapshotFromRow(row map[string]any) (agentSnapshot, bool) {
	payload := row
	if p, ok := row["payload"].(map[string]any); ok && p != nil {
		payload = p
	}
	id, _ := row["id"].(string)
	if id == "" {
		id, _ = payload["id"].(string)
	}
	name, _ := payload["name"].(string)
	roleSlug, _ := payload["roleSlug"].(string)
	kind, _ := payload["kind"].(string)
	if id == "" {
		return agentSnapshot{}, false
	}
	caps, _ := payload["capabilities"].(map[string]any)
	if caps == nil {
		caps = map[string]any{}
	}
	// Phase 2 cut (#158): SkillIds is the source of truth for the
	// capability surface. Resolved Domains / Tools are filled by the
	// caller via engine.ResolveSkills when the consumer needs them.
	return agentSnapshot{
		Id:           id,
		Name:         name,
		RoleSlug:     roleSlug,
		Kind:         kind,
		SkillIds:     stringSliceFromAny(caps["skillIds"]),
		Capabilities: caps,
	}, true
}

func roleSnapshotFromRow(row map[string]any) (roleSnapshot, bool) {
	payload := row
	if p, ok := row["payload"].(map[string]any); ok && p != nil {
		payload = p
	}
	slug, _ := payload["slug"].(string)
	if slug == "" {
		return roleSnapshot{}, false
	}
	return roleSnapshot{
		Slug:                  slug,
		Name:                  stringField(payload, "name"),
		Category:              stringField(payload, "category"),
		Tier:                  stringField(payload, "tier"),
		LockedSkillIds:        stringSliceFromAny(payload["lockedSkillIds"]),
		DefaultSkillIds:       stringSliceFromAny(payload["defaultSkillIds"]),
		AvailableSkillIds:     stringSliceFromAny(payload["availableSkillIds"]),
		ForbiddenSkillIds:     stringSliceFromAny(payload["forbiddenSkillIds"]),
		MaxSkills:             intFromAnyLoose(payload["maxSkills"]),
		RecommendedPolicySlug: stringField(payload, "recommendedPolicySlug"),
		SystemPromptHints:     stringField(payload, "systemPromptHints"),
		Predefined:            boolField(payload, "predefined"),
		// The id lives on the ROW, not the payload -- fall back to the payload
		// spelling for the flat shape some callers pass.
		ID: coalesceString(stringField(row, "id"), stringField(payload, "id")),
	}, true
}

// boolField reads a boolean payload field, tolerating the shapes a JSON
// round-trip produces. Absent or unparseable reads as false, which is the safe
// default here: an unrecognised row is treated as NOT predefined, so it can
// never win the preference in findRoleBySlug by accident.
func boolField(payload map[string]any, key string) bool {
	switch v := payload[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	default:
		return false
	}
}

// intFromAnyLoose handles the float64 default JSON unmarshal produces
// for numeric payload fields, plus typed int variants for callers
// that pass them directly.
func intFromAnyLoose(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case int32:
		return int(x)
	}
	return 0
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func stringSliceFromAny(v any) []string {
	switch x := v.(type) {
	case []string:
		return append([]string(nil), x...)
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func unionStrings(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, s := range b {
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func findById(agents []agentSnapshot, id string) (agentSnapshot, bool) {
	for _, a := range agents {
		if a.Id == id {
			return a, true
		}
	}
	return agentSnapshot{}, false
}

// findRoleBySlug resolves a role slug against the catalog, preferring a
// PREDEFINED row over a user-created one that claims the same slug.
//
// The preference is a security boundary, not a tidiness rule (memql#3066). The
// predefined lock (#3061) protects a row ID, while this resolves a SLUG from an
// unscoped catalog, and `createAgentRole` opens `id: args.agentRoleId ??
// args.slug` -- so a caller passing an explicit id with a seeded row's slug
// mints a SECOND row carrying that slug with predefined:false. No lock is
// bypassed and none fires, because by its own contract that is an ordinary
// user-role write. `activeAgentRoles` is @public and unscoped, so the catalog
// carries both.
//
// This function was first-match-wins over an UNORDERED result set, so the
// forged row could supply Name, SystemPromptHints, DefaultSkillIds and
// RecommendedPolicySlug for a newly created agent -- what it is called, how it
// is instructed, and which AI-router policy it runs under.
//
// The grant ceiling was never affected: fetchAgentRole keys on `id = slug` with
// createdAt DESC LIMIT 1, so forbiddenSkillIds and maxSkills are still enforced
// against the real seeded row.
//
// Preferring predefined does NOT mean requiring it -- a user-defined slug with
// no seeded counterpart resolves exactly as before.
//
// Two rows sharing a slug always resolve the same way, and preferring
// predefined is only half of why. It disambiguates the MIXED case; it says
// nothing when the tie is between two rows that agree on Predefined -- two user
// rows on one slug, or (in principle) two seeded ones. An earlier version of
// this comment claimed order-independence on the strength of the preference
// alone, which was false for exactly those cases: the winner was whichever the
// query returned first, out of an @cache(300) result whose order can change
// between cache fills.
//
// So the tie-break is INTRINSIC -- lowest row id wins among equals. That is a
// property of the rows rather than of the result order, which is what makes the
// sentence above true instead of nearly true.
//
// This is the narrow half of the fix. Making the slug UNIQUE per catalog is the
// half that makes the concept's own doc true ("Canonical slug... stable, never
// renamed"); it needs a write-time rule and is filed as memql#3114. Scoping the
// catalog read is a third option, declined in memql#2985 for a wire reason
// recorded on activeAgentRoles in dsl/agents/queries.memql.
//
// Note what this preference now leans on: activeAgentRoles filters
// isActiveRecord in SQL, so an inactive row never reaches here. Relax that and
// findRoleBySlug would happily prefer an INACTIVE seeded row over a live user
// one.
func findRoleBySlug(roles []roleSnapshot, slug string) (roleSnapshot, bool) {
	var best roleSnapshot
	found := false
	for _, r := range roles {
		if r.Slug != slug {
			continue
		}
		if !found {
			best, found = r, true
			continue
		}
		if betterRoleMatch(r, best) {
			best = r
		}
	}
	return best, found
}

// betterRoleMatch reports whether candidate should displace current as the
// resolution for a shared slug: predefined first, then lowest row id.
//
// The id comparison is the tie-break that makes findRoleBySlug's result
// independent of the order the catalog query returned. It is deliberately total
// -- for any two distinct rows exactly one is "better" -- so no pair can fall
// through to positional order.
func betterRoleMatch(candidate, current roleSnapshot) bool {
	if candidate.Predefined != current.Predefined {
		return candidate.Predefined
	}
	return candidate.ID < current.ID
}

func coalesceString(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// existingForPrompt formats the existing-agents slice for the
// agentFactoryAnalyze prompt -- compact, only the fields the prompt
// references. JSON-able so the template's {{ .existingAgents }} can
// render it.
func existingForPrompt(agents []agentSnapshot) []map[string]any {
	out := make([]map[string]any, 0, len(agents))
	for _, a := range agents {
		out = append(out, map[string]any{
			"id":       a.Id,
			"name":     a.Name,
			"roleSlug": a.RoleSlug,
			"capabilities": map[string]any{
				"skillIds": a.SkillIds,
			},
		})
	}
	return out
}

func roleCatalogForPrompt(roles []roleSnapshot) []map[string]any {
	out := make([]map[string]any, 0, len(roles))
	for _, r := range roles {
		out = append(out, map[string]any{
			"slug":              r.Slug,
			"name":              r.Name,
			"category":          r.Category,
			"tier":              r.Tier,
			"lockedSkillIds":    r.LockedSkillIds,
			"defaultSkillIds":   r.DefaultSkillIds,
			"availableSkillIds": r.AvailableSkillIds,
			"forbiddenSkillIds": r.ForbiddenSkillIds,
			"maxSkills":         r.MaxSkills,
		})
	}
	return out
}
