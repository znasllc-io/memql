package memql

import (
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
