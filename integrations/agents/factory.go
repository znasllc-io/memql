package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	memorynodes "github.com/visionarys-io/memql/component/database/memory-nodes"
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
//	spaceId      string  optional -- forwarded to the analysis prompt
//
// Behavior:
//
//  1. Load the user's existing agents via queryActiveAgentsForUser.
//  2. Load the role catalog via queryActiveAgentRoles.
//  3. Call si("agentFactoryAnalyze", {...}) for the structured
//     decision -- {action, targetAgentId, roleSlug, domainIds,
//     liveSourceIds, toolSlugs, reasoning}.
//  4. Dispatch on action:
//     - "match":   return the targetAgentId unchanged.
//     - "extend":  union the proposed domains + tools onto the
//                  target's current capabilities and write via
//                  mutationUpdateAgent (the lock validator blocks
//                  any removal of locked items).
//     - "create":  compose capabilities from the role's locked +
//                  default + proposed additions, then write a new
//                  agent via mutationCreateAgent.
//  5. Return ONE MemoryNode whose payload JSON is
//     {agentId, agentName, roleSlug, action, reasoning}.
//
// Failure modes:
//   - Missing required arg              -> typed error.
//   - Analysis returns invalid JSON     -> wrapping error with the raw text.
//   - Analysis names a non-existent     -> error pointing the caller at
//     role/agent                          queryActiveAgentRoles / queryActiveAgentsForUser.
//   - Lock violation on extend          -> propagated from the
//                                          validator (the GA can retry
//                                          with action="create").
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
		updated, err := i.extendAgent(ctx, existing, decision)
		if err != nil {
			return nil, err
		}
		agentId = updated.Id
		agentName = updated.Name
		roleSlug = updated.RoleSlug
		action = "extend"
	case "create":
		created, err := i.createAgent(ctx, ownerUserId, decision, roleCatalog)
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
// ignored, missing optional fields are zero values.
type factoryDecision struct {
	Action         string   `json:"action"`
	TargetAgentId  string   `json:"targetAgentId"`
	RoleSlug       string   `json:"roleSlug"`
	DomainIds      []string `json:"domainIds"`
	LiveSourceIds  []string `json:"liveSourceIds"`
	ToolSlugs      []string `json:"toolSlugs"`
	Reasoning      string   `json:"reasoning"`
}

// agentSnapshot is the compact view of an existing v1:agents:agent
// row the factory needs for analysis + extension.
type agentSnapshot struct {
	Id           string
	Name         string
	RoleSlug     string
	Domains      []string
	Tools        []string
	OwnerUserId  string
	Capabilities map[string]any // raw capabilities sub-object for partial-update merge
}

// roleSnapshot is the compact view of a v1:agents:agentRole row.
type roleSnapshot struct {
	Slug                  string
	Name                  string
	Category              string
	Tier                  string
	LockedDomainIds       []string
	DefaultDomainIds      []string
	LockedToolSlugs       []string
	DefaultToolSlugs      []string
	RecommendedPolicySlug string
	SystemPromptHints     string
}

// loadExistingAgents walks the user's active agents. Best-effort:
// failures (no engine, query error, no rows) yield an empty slice
// so the analysis prompt still runs and `action: "create"` is
// always achievable.
func (i *Integration) loadExistingAgents(ctx context.Context, ownerUserId string) []agentSnapshot {
	query := fmt.Sprintf(`queryActiveAgentsForUser({ownerUserId: %q})`, ownerUserId)
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
		s.OwnerUserId = ownerUserId
		out = append(out, s)
	}
	return out
}

// loadRoleCatalog walks the v1:agents:agentRole catalog. Same
// best-effort tolerance as loadExistingAgents.
func (i *Integration) loadRoleCatalog(ctx context.Context) []roleSnapshot {
	raw, err := i.engine.Execute(ctx, `queryActiveAgentRoles()`)
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
// InvokeSIStructured. Returns the parsed decision struct.
func (i *Integration) analyzeGoal(ctx context.Context, goal string, existing []agentSnapshot, roles []roleSnapshot) (factoryDecision, error) {
	data := map[string]any{
		"goal":              goal,
		"existingAgents":    existingForPrompt(existing),
		"roleCatalog":       roleCatalogForPrompt(roles),
		"domainCatalog":    []any{}, // populated by a future commit; the prompt tolerates empty
		"liveSourceCatalog": []any{},
		"toolCatalog":       []any{},
		"now":               time.Now().UTC().Format(time.RFC3339),
	}
	rawJSON, err := i.engine.InvokeSIStructured(
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

// extendAgent unions the decision's domains + tools onto the target
// agent's current capabilities and writes via mutationUpdateAgent.
// Lock removal is rejected server-side by validateAgentLockedItems;
// extensions only ADD ids, never remove, so the path is safe.
func (i *Integration) extendAgent(ctx context.Context, existing []agentSnapshot, decision factoryDecision) (agentSnapshot, error) {
	if decision.TargetAgentId == "" {
		return agentSnapshot{}, fmt.Errorf("ensureForGoal: action=extend but targetAgentId is empty")
	}
	target, ok := findById(existing, decision.TargetAgentId)
	if !ok {
		return agentSnapshot{}, fmt.Errorf("ensureForGoal: action=extend targetAgentId %q not found", decision.TargetAgentId)
	}
	mergedDomains := unionStrings(target.Domains, decision.DomainIds)
	mergedTools := unionStrings(target.Tools, decision.ToolSlugs)
	caps := map[string]any{}
	for k, v := range target.Capabilities {
		caps[k] = v
	}
	caps["domains"] = mergedDomains
	caps["tools"] = mergedTools
	payload := map[string]any{"capabilities": caps}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return agentSnapshot{}, fmt.Errorf("marshal update payload: %w", err)
	}
	query := fmt.Sprintf(`mutationUpdateAgent({agentId: %q, payload: %s})`, target.Id, string(payloadJSON))
	if _, err := i.engine.Execute(ctx, query); err != nil {
		return agentSnapshot{}, fmt.Errorf("execute mutationUpdateAgent: %w", err)
	}
	target.Domains = mergedDomains
	target.Tools = mergedTools
	return target, nil
}

// createAgent composes the new agent's capabilities from the role
// catalog row + the analysis-proposed additions and writes via
// mutationCreateAgent. roleCatalog is the snapshot loaded above so
// we don't re-query.
func (i *Integration) createAgent(ctx context.Context, ownerUserId string, decision factoryDecision, roleCatalog []roleSnapshot) (agentSnapshot, error) {
	role, ok := findRoleBySlug(roleCatalog, decision.RoleSlug)
	if !ok {
		return agentSnapshot{}, fmt.Errorf("ensureForGoal: action=create but roleSlug %q not in catalog", decision.RoleSlug)
	}
	domains := unionStrings(role.LockedDomainIds, role.DefaultDomainIds)
	domains = unionStrings(domains, decision.DomainIds)
	tools := unionStrings(role.LockedToolSlugs, role.DefaultToolSlugs)
	tools = unionStrings(tools, decision.ToolSlugs)

	agentId := uuid.New().String()
	caps := map[string]any{
		"domains":  domains,
		"tools":    tools,
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
	insertArgs := map[string]any{
		"agentId":        agentId,
		"ownerUserId":    ownerUserId,
		"name":           role.Name, // user can rename later
		"description":    role.SystemPromptHints,
		"role":           "specialist",
		"roleSlug":       role.Slug,
		"capabilities":   caps,
		"providerConfig": provider,
		"active":         true,
	}
	argsJSON, err := json.Marshal(insertArgs)
	if err != nil {
		return agentSnapshot{}, fmt.Errorf("marshal create args: %w", err)
	}
	query := fmt.Sprintf(`mutationCreateAgent(%s)`, string(argsJSON))
	if _, err := i.engine.Execute(ctx, query); err != nil {
		return agentSnapshot{}, fmt.Errorf("execute mutationCreateAgent: %w", err)
	}
	return agentSnapshot{
		Id:       agentId,
		Name:     role.Name,
		RoleSlug: role.Slug,
		Domains:  domains,
		Tools:    tools,
	}, nil
}

// factoryDecisionSchema is the JSON Schema enforced on the
// agentFactoryAnalyze output. Strict so the prompt's contract is
// load-bearing rather than aspirational.
var factoryDecisionSchema = json.RawMessage(`{
  "type": "object",
  "required": ["action", "roleSlug", "domainIds", "toolSlugs", "reasoning"],
  "additionalProperties": false,
  "properties": {
    "action":         {"type": "string", "enum": ["match", "extend", "create"]},
    "targetAgentId":  {"type": "string"},
    "roleSlug":       {"type": "string"},
    "domainIds":      {"type": "array", "items": {"type": "string"}},
    "liveSourceIds":  {"type": "array", "items": {"type": "string"}},
    "toolSlugs":      {"type": "array", "items": {"type": "string"}},
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
	if id == "" {
		return agentSnapshot{}, false
	}
	caps, _ := payload["capabilities"].(map[string]any)
	if caps == nil {
		caps = map[string]any{}
	}
	domains := stringSliceFromAny(caps["domains"])
	tools := stringSliceFromAny(caps["tools"])
	return agentSnapshot{
		Id:           id,
		Name:         name,
		RoleSlug:     roleSlug,
		Domains:      domains,
		Tools:        tools,
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
		LockedDomainIds:       stringSliceFromAny(payload["lockedDomainIds"]),
		DefaultDomainIds:      stringSliceFromAny(payload["defaultDomainIds"]),
		LockedToolSlugs:       stringSliceFromAny(payload["lockedToolSlugs"]),
		DefaultToolSlugs:      stringSliceFromAny(payload["defaultToolSlugs"]),
		RecommendedPolicySlug: stringField(payload, "recommendedPolicySlug"),
		SystemPromptHints:     stringField(payload, "systemPromptHints"),
	}, true
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

func findRoleBySlug(roles []roleSnapshot, slug string) (roleSnapshot, bool) {
	for _, r := range roles {
		if r.Slug == slug {
			return r, true
		}
	}
	return roleSnapshot{}, false
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
				"domains": a.Domains,
				"tools":   a.Tools,
			},
		})
	}
	return out
}

func roleCatalogForPrompt(roles []roleSnapshot) []map[string]any {
	out := make([]map[string]any, 0, len(roles))
	for _, r := range roles {
		out = append(out, map[string]any{
			"slug":             r.Slug,
			"name":             r.Name,
			"category":         r.Category,
			"tier":             r.Tier,
			"lockedDomainIds":  r.LockedDomainIds,
			"defaultDomainIds": r.DefaultDomainIds,
			"lockedToolSlugs":  r.LockedToolSlugs,
			"defaultToolSlugs": r.DefaultToolSlugs,
		})
	}
	return out
}
