package memql

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// AgentDefinition is the engine-internal compiled form of a parsed
// `agent` DSL declaration. Mirrors the v1:agents:agent concept's
// baseline-subset fields (the set declarable in DSL per the
// brainstorm decision; user-personalization fields like avatarPersonaId
// stay out -- those come from UI mutations).
//
// Phase 2 of the agents-dsl-primitive feature: this struct is what
// the parser-output (agentDecl) gets lowered into. The loader (Phase 3,
// in unified_kinds_loader.go's LoadUnifiedAgents) populates the
// SystemPrompt field by reading @templateFile, validates the Tools and
// Knowledge cross-references against the ToolRegistry / runtime
// knowledge-domain rows, and registers the result in AgentRegistry.
//
// The agent(...) builtin (Phase 4) looks the AgentDefinition up by Name
// from the registry and uses its fields to construct the dispatch
// request to the agent node.
type AgentDefinition struct {
	// Identity (from annotations + body)
	Name        string
	Namespace   string
	Version     string
	Description string // from @description (annotation-level)
	BodyDesc    string // from `description: "..."` in body

	Scope      AgentScope // global | perUser
	Visibility []string   // node-type filter, e.g. ["bff", "cognition", "agent"]

	// Template
	TemplateFile string // relative path declared in @templateFile
	SystemPrompt string // resolved content of TemplateFile (filled at load time)

	// Body identity
	Role        string // "specialist" | "general_assistant"
	RoleSlug    string
	DisplayName string
	Personality string
	Gender      string // "female" | "male"

	// Provider config (LLM only -- voice + avatar provider settings come
	// from the user's row via UI mutations).
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
	CapTools         []AgentToolRef // resolved tool refs

	// Knowledge (top-level, distinct from CapDomains: knowledge = RAG
	// corpus to retrieve from; domains = routing-scoring categories).
	Knowledge []AgentKnowledgeRef

	// Trigger behavior
	TBAutoJoin          bool
	TBGreetOnJoin       bool
	TBInterruptionStyle string // "polite" | "assertive" | "passive"
	TBSpeakWhen         string // "asked" | "relevant" | "always"

	// Media control
	AudioControl string // "always_on" | "always_off" | "mirror_user"
	VideoControl string

	// Provenance
	Origin string // "unified:<file>:<name>" for diagnostics
}

// AgentScope is the materialization scope an agent declaration carries.
// Determines how the loader stamps v1:agents:agent rows for this
// agent (Phase 3) and how the agent(...) builtin (Phase 4) resolves
// the row at invocation time.
type AgentScope string

const (
	// AgentScopePerUser materializes one v1:agents:agent row per
	// v1:identity:user. Personalization fields (avatar persona, voice
	// id) are left empty by the loader; user fills them via UI
	// mutations. Default when @scope is omitted.
	AgentScopePerUser AgentScope = "perUser"

	// AgentScopeGlobal materializes a single v1:agents:agent row in
	// the _system partition with no ownerUserId. Use for
	// system-internal agents invoked by automations (e.g. a "PR
	// reviewer" agent triggered by CI events).
	AgentScopeGlobal AgentScope = "global"
)

// AgentToolRef is a resolved reference to a `tool` DSL construct.
// Phase 2 lowers parsed agentRef{Kind:"tool", Name:"..."} into this
// shape; Phase 3 (loader) cross-validates Name against ToolRegistry
// and rejects unresolved refs at load time.
type AgentToolRef struct {
	Name string
}

// AgentKnowledgeRef is a resolved reference to a v1:common:knowledgeDomain
// row. Resolution happens at runtime (knowledge domains are concept
// rows, not DSL constructs); the loader stores the bare name. The
// agent(...) builtin (Phase 4) hands these names to the existing
// knowledge-retrieval path in integrations/agent/.
type AgentKnowledgeRef struct {
	Name string
}

// AgentRegistry stores compiled AgentDefinitions by name. Mirrors
// PromptRegistry / ToolRegistry / etc. -- thread-safe, populated at
// engine startup by LoadUnifiedAgents, queried at runtime by the
// agent(...) builtin's executor.
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
func (r *AgentRegistry) LoadFromRows(ctx context.Context, engine *MemQLEngine, logger *slog.Logger) (int, error) {
	if r == nil {
		return 0, fmt.Errorf("agent registry is nil")
	}
	if engine == nil {
		return 0, fmt.Errorf("engine is nil")
	}

	result, err := engine.Execute(ctx, `node(concept=="v1:agents:agent")`)
	if err != nil {
		return 0, fmt.Errorf("query v1:agents:agent rows: %w", err)
	}

	rows := extractRowList(result)
	r.Clear()

	registered := 0
	skipped := 0
	for _, row := range rows {
		def, ok := agentDefinitionFromRow(row)
		if !ok {
			skipped++
			continue
		}
		// Key by roleSlug (per-partition unique). Falls back to row
		// id if roleSlug is empty -- rare, but defensible.
		key := def.RoleSlug
		if key == "" {
			key = def.Name
		}
		if key == "" {
			skipped++
			continue
		}
		def.Name = key
		r.mu.Lock()
		r.byName[key] = def
		r.mu.Unlock()
		registered++
	}

	if logger != nil {
		logger.Info("memql.agentRegistry: loaded from rows",
			"component", "memql.agentRegistry",
			"registered", registered,
			"skipped", skipped)
	}
	return registered, nil
}

// extractRowList walks an engine.Execute result and returns the row
// maps. Handles the two shapes the engine returns: a top-level []any
// of row maps, or a {"nodes":[...]} wrapper.
func extractRowList(result any) []map[string]any {
	if result == nil {
		return nil
	}
	var rows []any
	switch v := result.(type) {
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
// what mutationCreateAgent stamped (which in turn came from the
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
		Name:        name,
		Description: getStringField(payload, "description"),
		Role:        getStringField(payload, "role"),
		RoleSlug:    roleSlug,
		DisplayName: name,
		Personality: getStringField(payload, "personality"),
		Gender:      getStringField(payload, "gender"),

		AudioControl: getStringField(payload, "audioControl"),
		VideoControl: getStringField(payload, "videoControl"),

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

// compileAgentDecl lowers a parsed agentDecl into an AgentDefinition.
// Pure transformation -- no cross-reference validation, no template
// reading. Both of those happen at load time (LoadUnifiedAgents,
// Phase 3) where the relevant registries + filesystem are available.
//
// Defaults applied here:
//   - @scope omitted -> AgentScopePerUser
//   - @visibility omitted -> nil (loader treats as "every node loads it")
//   - role omitted -> "specialist"
//
// Returns an error only for impossible-to-recover input (e.g. unknown
// scope value -- the parser already validates this, so this is
// belt-and-suspenders).
func compileAgentDecl(decl *agentDecl) (*AgentDefinition, error) {
	if decl == nil {
		return nil, nil
	}

	scope := AgentScopePerUser
	switch decl.scope {
	case "", "perUser":
		scope = AgentScopePerUser
	case "global":
		scope = AgentScopeGlobal
	}

	role := decl.role
	if role == "" {
		role = "specialist"
	}

	tools := make([]AgentToolRef, 0, len(decl.capTools))
	for _, ref := range decl.capTools {
		// Parser already enforces ref.Kind == "tool" inside
		// capabilities.tools; defensively skip anything else.
		if ref.Kind != "tool" {
			continue
		}
		tools = append(tools, AgentToolRef{Name: ref.Name})
	}

	knowledge := make([]AgentKnowledgeRef, 0, len(decl.knowledge))
	for _, ref := range decl.knowledge {
		if ref.Kind != "knowledgeDomain" {
			continue
		}
		knowledge = append(knowledge, AgentKnowledgeRef{Name: ref.Name})
	}

	def := &AgentDefinition{
		Name:        decl.name,
		Namespace:   decl.namespace,
		Version:     decl.version,
		Description: decl.description,
		BodyDesc:    decl.bodyDesc,

		Scope:      scope,
		Visibility: append([]string(nil), decl.visibility...),

		TemplateFile: decl.templateFile,
		// SystemPrompt left empty -- loader fills it from TemplateFile.

		Role:        role,
		RoleSlug:    decl.roleSlug,
		DisplayName: decl.displayName,
		Personality: decl.personality,
		Gender:      decl.gender,

		LLMProvider:    decl.llmProvider,
		LLMModel:       decl.llmModel,
		LLMPolicyName:  decl.llmPolicyName,
		LLMTemperature: decl.llmTemperature,
		LLMTempSet:     decl.llmTempSet,
		LLMMaxTokens:   decl.llmMaxTokens,
		LLMMaxTokSet:   decl.llmMaxTokSet,

		CapAvatar:        decl.capAvatar,
		CapLipSync:       decl.capLipSync,
		CapVision:        decl.capVision,
		CapVoiceToVoice:  decl.capVoiceToVoice,
		CapClaw:          decl.capClaw,
		CapClawWorkspace: decl.capClawWorkspace,
		CapDomains:       append([]string(nil), decl.capDomains...),
		CapKeywords:      append([]string(nil), decl.capKeywords...),
		CapTools:         tools,

		Knowledge: knowledge,

		TBAutoJoin:          decl.tbAutoJoin,
		TBGreetOnJoin:       decl.tbGreetOnJoin,
		TBInterruptionStyle: decl.tbInterruptionStyle,
		TBSpeakWhen:         decl.tbSpeakWhen,

		AudioControl: decl.audioControl,
		VideoControl: decl.videoControl,
	}
	return def, nil
}

// validateAgentRefs cross-checks an AgentDefinition's Tool refs
// against the supplied ToolRegistry. Returns a slice of unresolved
// tool names (empty when every ref resolves). Knowledge refs are NOT
// validated here -- knowledge domains are runtime concept rows, not
// DSL constructs, so resolution happens at invocation time inside
// the agent(...) builtin (Phase 4).
//
// Used by LoadUnifiedAgents (Phase 3) to fail-fast on a typo'd tool
// name with a friendly error pointing at the agent file.
func validateAgentToolRefs(def *AgentDefinition, tools *ToolRegistry) []string {
	if def == nil || tools == nil {
		return nil
	}
	var unresolved []string
	for _, ref := range def.CapTools {
		if _, err := tools.Get(ref.Name); err != nil {
			unresolved = append(unresolved, ref.Name)
		}
	}
	return unresolved
}
