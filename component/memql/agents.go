package memql

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/auth"
)

// AgentDefinition is the engine-internal representation of an
// invokable agent. Mirrors the v1:agents:agent concept's baseline
// fields. Source: materialized v1:agents:agent rows (loaded into
// AgentRegistry by AgentRegistry.LoadFromRows). The `agent(name)`
// builtin looks definitions up by RoleSlug (the registry's primary
// key) and uses the fields below to construct a dispatch request.
type AgentDefinition struct {
	// Identity. Name is the registry key (today this is the row's
	// RoleSlug; see LoadFromRows). DisplayName is the user-facing
	// label (the agent's "name" field on the row, which the user
	// may have renamed). Origin records the source for diagnostics
	// ("row:<row-id>" after the seed migration).
	Name        string
	Description string
	Role        string // "specialist" | "assistant"
	RoleSlug    string
	Kind        string // "assistant" | "specialist" | "system" -- first-class agent identity (memql#398). Read directly from the row's `kind` field. Cognition routing skips Kind=="system" agents from utterance dispatch candidates; the agent factory skips them from match/extend dedupe targets. "assistant" is the per-user GA (the frontend's agent-creation default); "specialist" is tool-driven (planner-provisioned under memql#399 or user-created with role=specialist); "system" is platform infrastructure (MemQL Planner / MemQL Trainer). Schema invariant: Kind == "system" OR Kind == Role.
	DisplayName string
	Personality string
	Gender      string // "female" | "male"

	// Scope: "global" | "perUser". Carried as a plain string post-
	// migration -- the old AgentScope enum was tied to the parser.
	Scope string

	// SystemPrompt is the agent's persona instructions. Sourced from
	// the row's `systemPrompt` field, which the SeedMaterializer
	// stamps when the seed declares an `@templateFile`. The
	// agent("name") builtin's dispatch path injects this as the
	// "system" turn before the user's utterance.
	SystemPrompt string

	// Provider config (LLM only -- voice + avatar provider settings
	// live on the row via UI mutations and are read directly there).
	LLMProvider    string
	LLMModel       string
	LLMPolicyName  string
	LLMTemperature float64
	LLMTempSet     bool // distinguishes "0.0 explicitly" from "unset"
	LLMMaxTokens   int
	LLMMaxTokSet   bool

	// Capabilities
	CapAvatar        bool
	CapLipSync       bool
	CapVision        bool
	CapVoiceToVoice  bool
	CapClaw          bool
	CapClawWorkspace string
	CapDomains       []string
	CapKeywords      []string
	CapTools         []AgentToolRef

	// Trigger behavior
	TBAutoJoin          bool
	TBGreetOnJoin       bool
	TBInterruptionStyle string // "polite" | "assertive" | "passive"
	TBSpeakWhen         string // "asked" | "relevant" | "always"

	// Media control
	AudioControl string // "always_on" | "always_off" | "mirror_user"
	VideoControl string

	// Id is the agent's row id. Carried because it is the TOTAL tie-break
	// when two rows contest one registry key: the order rows arrive in is
	// not something resolution may depend on (memql#3209).
	Id string

	// Provenance
	Origin string
}

// agentCandidate pairs a definition with HOW it claims its registry key --
// through its roleSlug, or through the display-name fallback. The two rank
// differently, so they cannot be compared on the definition alone.
type agentCandidate struct {
	def      *AgentDefinition
	fromSlug bool
}

// registryKeyFor returns the key a definition claims, and whether the claim
// came from its roleSlug. The display-name fallback exists only for legacy
// rows written before roleSlug -- and `name` is a caller-supplied arg on
// createAgent, so a name-derived claim must never displace a slug-derived one
// (see betterAgentMatch).
func registryKeyFor(def *AgentDefinition) (string, bool) {
	if def == nil {
		return "", false
	}
	if s := strings.TrimSpace(def.RoleSlug); s != "" {
		return s, true
	}
	return strings.TrimSpace(def.DisplayName), false
}

// betterAgentMatch reports whether candidate should displace current for a
// contested registry key.
//
// TOTAL by construction: for any two DISTINCT rows exactly one is better, so no
// pair can fall through to positional order. That totality is the property --
// a comparator with ties would leave the winner decided by iteration order
// again, one layer down. Mirrors betterRoleMatch (integrations/agents/
// factory.go), the same shape memql#3066 chose for the agentRole catalog.
//
//  1. kind=="system" wins. A user actor cannot write kind="system" --
//     validateAgentKindActorScope refuses it -- so this is the v1:agents:agent
//     analogue of agentRole's `predefined` flag: an authority marker a caller
//     cannot forge.
//  2. a slug-keyed row beats a name-keyed one. Closes "name your agent
//     system-planner and take the key without ever writing a roleSlug".
//  3. lowest row id. Intrinsic, stable, and not caller-orderable.
func betterAgentMatch(candidate, current agentCandidate) bool {
	if candidate.def == nil {
		return false
	}
	if current.def == nil {
		return true
	}
	if candSys, curSys := candidate.def.Kind == agentKindSystem, current.def.Kind == agentKindSystem; candSys != curSys {
		return candSys
	}
	if candidate.fromSlug != current.fromSlug {
		return candidate.fromSlug
	}
	return candidate.def.Id < current.def.Id
}

// buildAgentIndex resolves a row set into the registry map, order-independently.
//
// The map it replaces was last-wins: `byName[key] = def` on every row, so the
// winner was whichever row the query returned last. That made specialist
// resolution a property of `sort row.createdAt desc` -- and because the engine
// positions each logical row at its NEWEST version, that ordering is really
// last-MODIFIED, so editing an unrelated agent could change which one answered.
func buildAgentIndex(defs []*AgentDefinition) map[string]*AgentDefinition {
	winners := make(map[string]agentCandidate, len(defs))
	for _, def := range defs {
		if def == nil {
			continue
		}
		key, fromSlug := registryKeyFor(def)
		if key == "" {
			continue
		}
		cand := agentCandidate{def: def, fromSlug: fromSlug}
		if cur, ok := winners[key]; !ok || betterAgentMatch(cand, cur) {
			winners[key] = cand
		}
	}
	out := make(map[string]*AgentDefinition, len(winners))
	for key, c := range winners {
		c.def.Name = key
		out[key] = c.def
	}
	return out
}

// AgentToolRef is the in-memory representation of a tool the agent
// has been granted. Stored on the row's capabilities.tools[] as a
// bare string; resolved against the ToolRegistry at invocation time.
type AgentToolRef struct {
	Name string
}

// AgentRegistry caches AgentDefinitions in memory, keyed by the
// row's RoleSlug. Populated by LoadFromRows at engine startup --
// the materialized v1:agents:agent rows are the source of truth.
type AgentRegistry struct {
	mu     sync.RWMutex
	byName map[string]*AgentDefinition
}

// NewAgentRegistry returns an empty registry. Capitalized so the
// engine bootstrap (Phase 3) can construct one alongside the others.
func NewAgentRegistry() *AgentRegistry {
	return &AgentRegistry{byName: make(map[string]*AgentDefinition)}
}

// Get retrieves an agent definition by name. Returns ok=false when
// the name is unregistered. Safe to call on a nil receiver
// (returns nil, false).
func (r *AgentRegistry) Get(name string) (*AgentDefinition, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.byName[name]
	return a, ok
}

// Names returns every registered agent name. Order is map-iteration
// order (non-deterministic). Used by diagnostic dumps + the eventual
// `cockpit agents list` view.
func (r *AgentRegistry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byName))
	for name := range r.byName {
		out = append(out, name)
	}
	return out
}

// Upsert inserts or replaces an agent definition by name. Used by
// LoadUnifiedAgents during startup. Mirrors the Upsert signature
// other registries expose.
func (r *AgentRegistry) Upsert(def *AgentDefinition) error {
	if r == nil {
		return nil
	}
	if def == nil || def.Name == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName[def.Name] = def
	return nil
}

// Clear empties the registry. Used by the row-backed loader so a
// re-sweep starts from a clean state without leaving stale entries.
func (r *AgentRegistry) Clear() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName = make(map[string]*AgentDefinition)
}

// LoadFromRows populates the registry by scanning v1:agents:agent
// rows out of the database. Each row becomes one AgentDefinition,
// keyed by the row's roleSlug (universally unique per partition,
// human-readable, distinct from per-user row ids).
//
// This is the canonical post-seed-migration source of truth: the
// SeedMaterializer writes rows, the row IS the agent, and the
// registry is a thin in-memory cache built from those rows.
// Replaces the prior path where the registry was populated by
// parsing `agent X { }` DSL declarations -- those are now `seed X { }`
// declarations consumed by the materializer.
//
// Idempotent: Clear()'s the registry before loading so a re-run
// reflects the latest row set without stale entries.
//
// Returns the number of definitions registered + any error from the
// underlying row query (in which case the registry is left empty so
// callers see a deterministic "not registered" rather than a partial
// load).
//
// # Who can enter this input set (the memql#3209 trace, answered)
//
// A row whose registry key is chosen by a NON-SYSTEM actor CAN enter it.
// Established from the write path and the query, not from what the callers
// happen to be today:
//
//   - `createAgent` (dsl/agents/mutations.memql) takes `roleSlug` as a plain
//     caller-supplied arg -- no enum, no validation against the agentRole
//     catalog, no uniqueness check. The concept's own doc says the constraint
//     is "enforced caller-side".
//   - `agentsForRegistry` filters `isNotDeleted` and nothing else: not active,
//     not kind, not owner.
//   - When roleSlug is empty the key falls back to the DISPLAY NAME, which is
//     also caller-supplied, so a key can be claimed without writing a slug.
//   - Structurally, and with no adversary at all: `plannerAgent` and
//     `trainerAgent` are @scope("perUser") seeds carrying a FIXED roleSlug, so
//     N users produce N rows under one slug in any multi-user cluster.
//
// So resolution must not depend on row order -- see buildAgentIndex, which
// replaces the previous last-wins assignment with a total comparator.
//
// What this does NOT fix: the winner is still global rather than per-owner, so
// `askSpecialist` can hand user A's assistant the persona of user B's
// specialist under a contested key. That is a distinct defect (a missing owner
// dimension on the lookup, not an ordering one) and is filed separately -- do
// not read the ordering fix as covering it.
//
// # On pagination -- `paginate 50` was NOT a bound to rely on
//
// This used to read `allAgents`, which paginates 50 and mints a cursor that
// was never followed. That is right for a UI list and wrong for a resolution
// table: an agent missing from this map is not "on page 2", it is
// unresolvable. Worse, the engine fills a page from `target*2` PHYSICAL
// version rows and dedupes to logical ones afterwards, so ordinary edit churn
// shrinks the registry far below 50 -- 100 versions of one agent yields a
// registry of one. Hence the dedicated complete read below. A residual bound
// remains: the effective window is clamped by MaxWindow (default 5000), so a
// cluster past that many agents needs this revisited rather than silently
// truncated.
func (r *AgentRegistry) LoadFromRows(ctx context.Context, engine *MemQLEngine, logger *slog.Logger) (int, error) {
	if r == nil {
		return 0, fmt.Errorf("agent registry is nil")
	}
	if engine == nil {
		return 0, fmt.Errorf("engine is nil")
	}

	// agentsForRegistry (dsl/agents/queries.memql) -- @serverOnly + @unbounded,
	// the complete-set sibling of allAgents. Going through a named query gets
	// shape resolution + trait filtering for free; the raw `node(concept==...)`
	// shorthand is not valid query syntax.
	result, err := engine.Execute(auth.ContextWithInternalOrigin(ctx), `query agentsForRegistry()`)
	if err != nil {
		return 0, fmt.Errorf("agentsForRegistry: %w", err)
	}

	rows := extractRowList(result)
	r.Clear()

	defs := make([]*AgentDefinition, 0, len(rows))
	skipped := 0
	for _, row := range rows {
		def, ok := agentDefinitionFromRow(row)
		if !ok {
			skipped++
			continue
		}
		defs = append(defs, def)
	}

	index := buildAgentIndex(defs)
	// Rows that claimed no key at all, plus rows that lost a contested key.
	skipped += len(defs) - len(index)

	r.mu.Lock()
	for key, def := range index {
		r.byName[key] = def
	}
	registered := len(r.byName)
	r.mu.Unlock()

	if logger != nil {
		logger.Info("memql.agentRegistry: loaded from rows",
			"component", "memql.agentRegistry",
			"registered", registered,
			"skipped", skipped,
			"rows", len(rows))
	}
	return registered, nil
}

// extractRowList walks an engine.Execute result and returns the row
// maps. Handles the shapes the engine returns: the *ExecuteResult wrapper
// itself, a top-level []any of row maps, or a {"nodes":[...]} wrapper.
//
// The *ExecuteResult arm is not optional (memql#3209). MemQLEngine.Execute
// returns *ExecuteResult, so without it every caller fell through to
// `default: return nil` and read ZERO rows -- silently, because "the query
// matched nothing" and "we could not read what the query returned" arrive here
// as the same value. That emptied the entire specialist registry.
func extractRowList(result any) []map[string]any {
	if result == nil {
		return nil
	}
	var rows []any
	switch v := result.(type) {
	case *ExecuteResult:
		if v == nil {
			return nil
		}
		return extractRowList(v.OutputPayload())
	case []any:
		rows = v
	case map[string]any:
		if nodes, ok := v["nodes"].([]any); ok {
			rows = nodes
		} else {
			rows = []any{v}
		}
	default:
		return nil
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, m)
	}
	return out
}

// agentDefinitionFromRow builds an AgentDefinition from a single
// v1:agents:agent row map. The materialized row's payload mirrors
// what createAgent stamped (which in turn came from the
// seed body), so this is a near-direct field mapping.
//
// Returns ok=false when the row lacks the minimum fields (id +
// either roleSlug or name) -- such rows aren't usefully invokable
// as agents.
func agentDefinitionFromRow(row map[string]any) (*AgentDefinition, bool) {
	if row == nil {
		return nil, false
	}
	payload := row
	if p, ok := row["payload"].(map[string]any); ok && p != nil {
		payload = p
	}

	id, _ := row["id"].(string)
	if id == "" {
		if pid, ok := payload["id"].(string); ok {
			id = pid
		}
	}
	name := getStringField(payload, "name")
	roleSlug := getStringField(payload, "roleSlug")
	if name == "" && roleSlug == "" && id == "" {
		return nil, false
	}

	def := &AgentDefinition{
		Id:          id,
		Name:        name,
		Description: getStringField(payload, "description"),
		Role:        getStringField(payload, "role"),
		RoleSlug:    roleSlug,
		Kind:        getStringField(payload, "kind"),
		DisplayName: name,
		Personality: getStringField(payload, "personality"),
		Gender:      getStringField(payload, "gender"),

		AudioControl: getStringField(payload, "audioControl"),
		VideoControl: getStringField(payload, "videoControl"),

		SystemPrompt: getStringField(payload, "systemPrompt"),

		Origin: "row:" + id,
	}

	// providerConfig.llm.*
	if pc, ok := payload["providerConfig"].(map[string]any); ok && pc != nil {
		if llm, ok := pc["llm"].(map[string]any); ok && llm != nil {
			def.LLMProvider = getStringField(llm, "provider")
			def.LLMModel = getStringField(llm, "model")
			def.LLMPolicyName = getStringField(llm, "policyName")
			if v, ok := llm["temperature"].(float64); ok {
				def.LLMTemperature = v
				def.LLMTempSet = true
			}
			if v, ok := llm["maxTokens"].(float64); ok {
				def.LLMMaxTokens = int(v)
				def.LLMMaxTokSet = true
			}
		}
	}

	// capabilities.*
	if cap, ok := payload["capabilities"].(map[string]any); ok && cap != nil {
		def.CapAvatar, _ = cap["avatar"].(bool)
		def.CapLipSync, _ = cap["lipSync"].(bool)
		def.CapVision, _ = cap["vision"].(bool)
		def.CapVoiceToVoice, _ = cap["voiceToVoice"].(bool)
		def.CapClaw, _ = cap["claw"].(bool)
		def.CapClawWorkspace = getStringField(cap, "clawWorkspace")
		def.CapDomains = getStringArrayField(cap, "domains")
		def.CapKeywords = getStringArrayField(cap, "keywords")
		for _, toolName := range getStringArrayField(cap, "tools") {
			def.CapTools = append(def.CapTools, AgentToolRef{Name: toolName})
		}
	}

	// triggerBehavior.*
	if tb, ok := payload["triggerBehavior"].(map[string]any); ok && tb != nil {
		def.TBAutoJoin, _ = tb["autoJoin"].(bool)
		def.TBGreetOnJoin, _ = tb["greetOnJoin"].(bool)
		def.TBInterruptionStyle = getStringField(tb, "interruptionStyle")
		def.TBSpeakWhen = getStringField(tb, "speakWhen")
	}

	return def, true
}

// getStringField pulls a string field from a row payload map, with
// the empty string as the zero value.
func getStringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// getStringArrayField pulls a []string from a row payload's []any
// slot. Non-string entries are silently skipped.
func getStringArrayField(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	arr, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
