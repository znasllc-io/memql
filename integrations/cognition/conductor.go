package cognition

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Conductor: structured orchestration to replace ad-hoc prompt reasoning
// =============================================================================
//
// Background: through phases 1 and 2 of the multi-agent rework, the agent
// reply prompt grew into the de facto orchestration layer -- conditional
// templating ({{if .agentIsKnownToUser}}, {{if .handoffFrom}}, the SCOPE
// FENCE chain, the chime-in trigger branches) was carrying decision logic
// the cognition system had the data to make in Go.
//
// The result: the prompt is ~31K tokens, every "what should I do this turn?"
// decision is made twice (once by cognition's heuristics + LLM router,
// once by the model re-reasoning over the prompt context), and the model
// re-reasons sometimes wrong (over-introducing, repeating peers, adding
// fake handoff acknowledgments).
//
// The conductor refactor splits orchestration cleanly:
//
//   - Cognition has the state. It observes utterances + presence + scoring
//     output, runs heuristics + the SI router, and decides things like
//     "this agent should chime in briefly with the IT angle" or "skip
//     the takeover opener -- the user addressed the room."
//   - The agent prompt has identity + style + constraints. Decisions made
//     by cognition arrive as a structured Directive, and the prompt
//     simply executes them. No more conditional decision-trees.
//
// This file defines:
//
//   ConductorState                  -- per-space orchestration state model
//   ConductorRegistry               -- in-memory map of states keyed by space
//   AgentParticipationDirective     -- the structured instruction
//   BuildDirective                  -- factory translating state into directive
//
// Phase 1 (this commit): the structs are defined, a registry is wired into
// CognitionIntegration, directives are built and threaded into the
// AgentGenerateTurnMsg hints map. Behavior is unchanged -- the directive
// is emitted but the prompt template doesn't consume it yet.
//
// Phase 2: the prompt reads the directive and drops the conditional
// decision-trees. The 31K-token prompt drops to plausibly 8-12K.
//
// Phase 3: typing-indicator events feed ConductorState.HumanIsTyping;
// dispatch path holds while a human is mid-message; chime-in directives
// carry differentiated content angles.

// -----------------------------------------------------------------------------
// Directive types
// -----------------------------------------------------------------------------

// DirectiveMode describes what kind of participation the conductor expects
// from an agent on a turn. Distinct from `trigger` (which says WHY the
// turn fired); this says HOW the agent should approach it.
type DirectiveMode string

const (
	// DirectivePrimary: this agent is the primary responder. Substantive
	// reply addressing the user's ask directly.
	DirectivePrimary DirectiveMode = "primary"

	// DirectiveChimeIn: this agent is one of several voices on a
	// multi-agent turn. Brief contribution from this agent's angle;
	// do NOT re-summarize what the primary already said.
	DirectiveChimeIn DirectiveMode = "chimein"

	// DirectiveBriefAck: a short acknowledgment is appropriate (e.g.
	// greeting back). Keep to one short sentence.
	DirectiveBriefAck DirectiveMode = "brief_ack"

	// DirectiveDefer: don't speak this turn -- yield the floor.
	// Currently advisory; not all paths honor it. Phase 3 will wire
	// it into the dispatch interlock.
	DirectiveDefer DirectiveMode = "defer"
)

// Brevity hints how long the response should be. The agent prompt branches
// on this directly instead of the model inferring from context.
type Brevity string

const (
	BrevityShort    Brevity = "short"    // one short sentence
	BrevityNormal   Brevity = "normal"   // a concise paragraph
	BrevityDetailed Brevity = "detailed" // multi-paragraph if warranted
)

// AgentParticipationDirective is the conductor's structured instruction to
// a specific agent for a specific turn. It rides alongside the
// AgentGenerateTurnMsg as a set of hints, replacing ad-hoc prompt-side
// reasoning about "what role do I play this turn?".
//
// The directive is the conductor's *answer* to questions the agent would
// otherwise have to derive from prompt context: "should I speak?", "should
// I introduce myself?", "should I acknowledge the previous responder?",
// "how brief?". By pre-deciding these in Go (with full state visibility),
// the agent prompt can drop most of its conditional decision-trees and
// just execute.
type AgentParticipationDirective struct {
	// Mode -- what kind of participation. See DirectiveMode.
	Mode DirectiveMode

	// Reason -- a human-readable rationale, for telemetry + optionally
	// for the prompt to surface to the model when useful.
	Reason string

	// ContentHint -- guides the agent's content angle. "build on
	// Sofia's cost-reduction angle with HR implications", "give the
	// IT-support take", "keep it to a one-line hello". Free-form
	// string, set by the conductor when it has a clear opinion.
	// Empty leaves the model to choose.
	//
	// Legacy field. The LLM conductor populates `Instruction` (below)
	// instead, which is more concrete and per-turn-tailored. ContentHint
	// stays for the fallback path that still uses chimeInContentAngle().
	ContentHint string

	// Brevity -- caps response length. Empty defaults to BrevityNormal.
	Brevity Brevity

	// SkipHandoffOpener -- when true, the prompt should NOT open with
	// "Thanks <previous responder>, I'll take this one." Set when the
	// user addressed the room (no real takeover) or when this is a
	// chime-in (parallel participation).
	SkipHandoffOpener bool

	// SkipSelfIntro -- when true, the prompt should NOT introduce the
	// agent ("I'm Sofia, your general assistant"). Set when
	// agentIsKnownToUser is true.
	SkipSelfIntro bool

	// SkipRoomAnnounce -- when true, the prompt should NOT announce
	// who else is in the room ("Atlas and Nova are here too..."). The
	// user has the participant panel; this is filler.
	SkipRoomAnnounce bool

	// Instruction -- the LLM conductor's per-agent, per-turn behavioral
	// directive. This is the load-bearing field after the conductor
	// rework: when populated, the prompt should treat it as THE
	// instruction for the turn, overriding most prompt-side reasoning.
	//
	// Examples:
	//   "Greet warmly, mention the space title if known, ask what they
	//    want to dig into. Skip the resume."
	//   "Just say hi back, one short sentence in your own voice. No
	//    agenda, no list of capabilities."
	//   "Answer the OAuth question with depth. Reference the docs you
	//    can pull on if relevant."
	Instruction string

	// GlobalGuidance -- the conductor's one-line room-wide posture for
	// this turn. Applies to all agents on this turn so primary +
	// chime-ins land in the same emotional register. Examples:
	//   "Conversation just opened. Keep it human and warm. No work-talk yet."
	//   "User is frustrated -- drop the consultant register and address
	//    the actual concern."
	//   "Focused technical question; answer directly."
	GlobalGuidance string

	// Phase -- the conductor's classification of where the conversation
	// is. One of: cold_open, warming, discovery, working, clarifying,
	// closing, frustration. Empty when not classified. The prompt may
	// surface this to the model for additional grounding.
	Phase string

	// Temperature -- the social register on this turn. One of: casual,
	// focused, tense. Empty when not classified.
	Temperature string

	// UserIntent -- the conductor's one-line read on what the user
	// actually wants from this turn. Helps the agent ground its
	// response in the user's actual ask rather than re-interpreting
	// the utterance text.
	UserIntent string

	// AcknowledgePrior -- the structured replacement for the legacy
	// hardcoded "## HANDOFF" block. Free-form short string the
	// conductor produces only when an opener acknowledgment of a
	// prior responder is conversationally right ("warm nod to Sofia",
	// "thank Atlas for setup"). Empty means no acknowledgment fires.
	//
	// Contract: when present, the agent's prompt surfaces this as
	// "Open with a brief acknowledgment: <text>" -- one phrase, then
	// the agent moves on to the actual content. The prior HANDOFF
	// prose block (which fired automatically whenever there was a
	// prior responder, regardless of whether acknowledgment was
	// appropriate) is retired.
	AcknowledgePrior string

	// ExpectedOutput -- the kind of contribution the agent is
	// supposed to produce ("joke", "answer", "commentary",
	// "acknowledgment", "skip"). The branch-point completion check
	// uses this to detect "asked for joke, got commentary" cases and
	// re-dispatch the agent. Free-form short string but the
	// completion checker recognizes a small enumeration of common
	// values. Empty when not classified.
	ExpectedOutput string
}

// String renders a compact form for log lines.
func (d *AgentParticipationDirective) String() string {
	if d == nil {
		return "<nil-directive>"
	}
	parts := []string{string(d.Mode)}
	if d.Phase != "" {
		parts = append(parts, "phase="+d.Phase)
	}
	if d.Temperature != "" {
		parts = append(parts, "temp="+d.Temperature)
	}
	if d.Brevity != "" {
		parts = append(parts, "brevity="+string(d.Brevity))
	}
	if d.SkipHandoffOpener {
		parts = append(parts, "skip-handoff")
	}
	if d.SkipSelfIntro {
		parts = append(parts, "skip-intro")
	}
	if d.SkipRoomAnnounce {
		parts = append(parts, "skip-room-announce")
	}
	if d.Instruction != "" {
		parts = append(parts, "instruction="+conductorTruncate(d.Instruction, 60))
	} else if d.ContentHint != "" {
		parts = append(parts, "hint="+conductorTruncate(d.ContentHint, 40))
	}
	if d.Reason != "" {
		parts = append(parts, "reason="+conductorTruncate(d.Reason, 40))
	}
	return strings.Join(parts, " ")
}

func conductorTruncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// -----------------------------------------------------------------------------
// Per-space state
// -----------------------------------------------------------------------------

// ConductorState is the per-space orchestration state model. Distinct from
// PolyphonSession (which is the scoring engine's state); the conductor
// reads polyphon state and layers its own awareness on top.
//
// Lives in memory; rebuilt on memQL restart from the utterance log.
type ConductorState struct {
	mu sync.Mutex

	SpaceId string

	// HumanIsTyping is set when a typing-indicator event arrives and
	// cleared when the next utterance lands. Phase 3 wires this into
	// a dispatch interlock so agents don't fire while a human is
	// mid-message.
	HumanIsTyping        bool
	HumanTypingStartedAt time.Time
	HumanTypingHumanId   string

	// AgentsSpokenThisCycle counts contributions per agent since the
	// last human utterance. Resets when a human speaks. Lets the
	// conductor avoid stacking the same agent and bias toward agents
	// who haven't yet spoken.
	AgentsSpokenThisCycle map[string]int

	// LastTurnPrimaryAgentId -- who won the most recent
	// human-triggered turn.
	LastTurnPrimaryAgentId string

	// LastInferredTopic -- short string from the most recent topic
	// inference. Set by the predictive analyzer (phase 4+) or by
	// tactical heuristic.
	LastInferredTopic string

	// DispatchHoldUntil -- when set, the conductor is holding all
	// dispatches until this time. Used to back off after a runaway
	// chain or while a human is still typing (phase 3).
	DispatchHoldUntil time.Time

	// SessionSummary -- the conductor's rolling 1-2-sentence summary
	// of what's been going on in this room. Updated each turn (the
	// conductor produces a new one in its plan) and fed back into
	// the conductor on the NEXT turn so it has continuity beyond the
	// 15-turn transcript window.
	//
	// Without this, the conductor's view of the conversation resets
	// every time the transcript scrolls past N turns. With it, the
	// conductor maintains a high-level "we've been working on X"
	// pointer that survives transcript truncation.
	SessionSummary string

	// IterationCount -- the number of conductor consults made on the
	// CURRENT user turn. Resets when a human speaks. Read by the
	// branch-point re-invoke loop to enforce maxConductorIterations.
	IterationCount int
}

// NewConductorState creates an empty per-space conductor state.
func NewConductorState(spaceId string) *ConductorState {
	return &ConductorState{
		SpaceId:               spaceId,
		AgentsSpokenThisCycle: make(map[string]int),
	}
}

// RecordAgentSpoke marks that an agent contributed to the current cycle.
// Thread-safe.
func (s *ConductorState) RecordAgentSpoke(agentId string) {
	if s == nil || agentId == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.AgentsSpokenThisCycle[agentId]++
}

// RecordHumanSpoke clears the per-cycle spoken-by-agent map, resets
// the typing state, and resets the conductor iteration counter.
// Called when a new human utterance lands.
func (s *ConductorState) RecordHumanSpoke() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.AgentsSpokenThisCycle = make(map[string]int)
	s.HumanIsTyping = false
	s.HumanTypingStartedAt = time.Time{}
	s.HumanTypingHumanId = ""
	s.IterationCount = 0
}

// AgentSpokeThisCycle reports whether the agent has already contributed
// since the last human utterance.
func (s *ConductorState) AgentSpokeThisCycle(agentId string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.AgentsSpokenThisCycle[agentId]
	return ok
}

// SetSessionSummary records the conductor's rolling summary. Called
// after each successful consult.
func (s *ConductorState) SetSessionSummary(summary string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SessionSummary = strings.TrimSpace(summary)
}

// GetSessionSummary returns the most recent rolling session summary,
// or empty when no plan has produced one yet.
func (s *ConductorState) GetSessionSummary() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.SessionSummary
}

// IncrementIteration bumps the conductor-call counter for the current
// user turn. Returns the new value. The counter resets when a human
// speaks (RecordHumanSpoke).
func (s *ConductorState) IncrementIteration() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.IterationCount++
	return s.IterationCount
}

// CurrentIteration returns the current iteration counter without
// incrementing it. Used by the branch-point loop to check the cap
// before deciding to re-invoke.
func (s *ConductorState) CurrentIteration() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.IterationCount
}

// IsHoldingDispatches reports whether the conductor is currently holding
// dispatches (human still typing, recent runaway, etc.). Phase 3 uses
// this; for phase 1 the field exists and the method works but no
// callers consult it yet.
func (s *ConductorState) IsHoldingDispatches() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.HumanIsTyping {
		return true
	}
	if !s.DispatchHoldUntil.IsZero() && time.Now().Before(s.DispatchHoldUntil) {
		return true
	}
	return false
}

// SetHumanTyping marks a human as currently typing. Phase 3 wires this
// to typing-indicator events from the WebSocket bridge.
func (s *ConductorState) SetHumanTyping(humanId string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.HumanIsTyping = true
	s.HumanTypingStartedAt = time.Now()
	s.HumanTypingHumanId = strings.TrimSpace(humanId)
}

// -----------------------------------------------------------------------------
// Registry
// -----------------------------------------------------------------------------

// ConductorRegistry holds per-space ConductorState in memory. One state
// per space; created lazily on first observation. Cleaned up when the
// space's session is removed (phase 3 will wire that hook).
type ConductorRegistry struct {
	mu     sync.RWMutex
	states map[string]*ConductorState
}

// NewConductorRegistry creates an empty registry.
func NewConductorRegistry() *ConductorRegistry {
	return &ConductorRegistry{
		states: make(map[string]*ConductorState),
	}
}

// GetOrCreate returns the conductor state for the given space, creating
// one on first call.
func (r *ConductorRegistry) GetOrCreate(spaceId string) *ConductorState {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	if s, ok := r.states[spaceId]; ok {
		r.mu.RUnlock()
		return s
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-check under write lock.
	if s, ok := r.states[spaceId]; ok {
		return s
	}
	s := NewConductorState(spaceId)
	r.states[spaceId] = s
	return s
}

// Get returns the conductor state for the given space, or nil if not
// yet created.
func (r *ConductorRegistry) Get(spaceId string) *ConductorState {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.states[spaceId]
}

// Remove drops the state for the given space. Called when the space is
// closed.
func (r *ConductorRegistry) Remove(spaceId string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.states, spaceId)
}

// -----------------------------------------------------------------------------
// Directive factory
// -----------------------------------------------------------------------------

// BuildDirective produces an AgentParticipationDirective for an agent on
// a turn given the conductor's state + per-turn signals. This is the
// central translation point: cognition's understanding of the situation
// -> structured instructions for the agent's prompt to execute.
//
// The factory deliberately does NOT mutate state -- it's a pure function
// over (state snapshot, per-turn signals). Mutations happen in
// RecordAgentSpoke / RecordHumanSpoke.
func BuildDirective(
	state *ConductorState,
	agentId string,
	isPrimaryWinner bool,
	isChimeIn bool,
	handoffFrom string,
	userAddressedRoom bool,
	agentIsKnownToUser bool,
) *AgentParticipationDirective {
	d := &AgentParticipationDirective{
		Mode:    DirectivePrimary,
		Brevity: BrevityNormal,
	}

	if isChimeIn {
		d.Mode = DirectiveChimeIn
		d.Brevity = BrevityShort
		d.Reason = "chiming in to multi-agent turn"
	}

	// Skip handoff opener when:
	//   - user addressed the room (no real takeover)
	//   - this is a chime-in (parallel, not takeover)
	//   - no prior responder (greenfield turn, nothing to hand off from)
	if userAddressedRoom || isChimeIn || strings.TrimSpace(handoffFrom) == "" {
		d.SkipHandoffOpener = true
	}

	// Skip self-intro when:
	//   - agent has interacted with this user before
	//   - this is a chime-in (the room already saw the primary's intro
	//     if any; chiming in with another self-intro is noise)
	if agentIsKnownToUser || isChimeIn {
		d.SkipSelfIntro = true
	}

	// Always skip room-announce. The user has the participant panel;
	// listing who's in the room as part of the agent's reply is filler
	// regardless of context.
	d.SkipRoomAnnounce = true

	return d
}

// -----------------------------------------------------------------------------
// Hint serialization
// -----------------------------------------------------------------------------
//
// In phase 1 directives travel via the existing AgentGenerateTurnMsg.Hints
// map (string -> string). Encoding everything into hints avoids a proto
// change for the foundation commit; phase 2 can promote to a typed proto
// field if it makes the agent-side rendering cleaner.

const (
	hintDirectiveMode              = "directive_mode"
	hintDirectiveReason            = "directive_reason"
	hintDirectiveContentHint       = "directive_content_hint"
	hintDirectiveBrevity           = "directive_brevity"
	hintDirectiveSkipHandoffOpener = "directive_skip_handoff_opener"
	hintDirectiveSkipSelfIntro     = "directive_skip_self_intro"
	hintDirectiveSkipRoomAnnounce  = "directive_skip_room_announce"
	hintDirectiveInstruction       = "directive_instruction"
	hintDirectiveGlobalGuidance    = "directive_global_guidance"
	hintDirectivePhase             = "directive_phase"
	hintDirectiveTemperature       = "directive_temperature"
	hintDirectiveUserIntent        = "directive_user_intent"
	hintDirectiveAcknowledgePrior  = "directive_acknowledge_prior"
	hintDirectiveExpectedOutput    = "directive_expected_output"
)

// EncodeDirectiveIntoHints writes the directive into a Hints map. Empty
// fields are omitted so the agent-side decoder treats them as defaults.
// Mutates the passed-in map.
func EncodeDirectiveIntoHints(d *AgentParticipationDirective, hints map[string]string) {
	if d == nil || hints == nil {
		return
	}
	if d.Mode != "" {
		hints[hintDirectiveMode] = string(d.Mode)
	}
	if d.Reason != "" {
		hints[hintDirectiveReason] = d.Reason
	}
	if d.ContentHint != "" {
		hints[hintDirectiveContentHint] = d.ContentHint
	}
	if d.Brevity != "" {
		hints[hintDirectiveBrevity] = string(d.Brevity)
	}
	if d.SkipHandoffOpener {
		hints[hintDirectiveSkipHandoffOpener] = "true"
	}
	if d.SkipSelfIntro {
		hints[hintDirectiveSkipSelfIntro] = "true"
	}
	if d.SkipRoomAnnounce {
		hints[hintDirectiveSkipRoomAnnounce] = "true"
	}
	if d.Instruction != "" {
		hints[hintDirectiveInstruction] = d.Instruction
	}
	if d.GlobalGuidance != "" {
		hints[hintDirectiveGlobalGuidance] = d.GlobalGuidance
	}
	if d.Phase != "" {
		hints[hintDirectivePhase] = d.Phase
	}
	if d.Temperature != "" {
		hints[hintDirectiveTemperature] = d.Temperature
	}
	if d.UserIntent != "" {
		hints[hintDirectiveUserIntent] = d.UserIntent
	}
	if d.AcknowledgePrior != "" {
		hints[hintDirectiveAcknowledgePrior] = d.AcknowledgePrior
	}
	if d.ExpectedOutput != "" {
		hints[hintDirectiveExpectedOutput] = d.ExpectedOutput
	}
}

// directiveAsMap renders a directive as a map[string]any for use in
// prompt template data. Mirrors the agent-side decoding in
// integrations/agent/prompt_data.go (buildDirectiveMap) so both
// dispatch paths produce the same shape for templates to branch on.
func directiveAsMap(d *AgentParticipationDirective) map[string]any {
	if d == nil {
		return nil
	}
	out := map[string]any{
		"mode": string(d.Mode),
	}
	if d.Reason != "" {
		out["reason"] = d.Reason
	}
	if d.ContentHint != "" {
		out["contentHint"] = d.ContentHint
	}
	if d.Brevity != "" {
		out["brevity"] = string(d.Brevity)
	}
	if d.SkipHandoffOpener {
		out["skipHandoffOpener"] = true
	}
	if d.SkipSelfIntro {
		out["skipSelfIntro"] = true
	}
	if d.SkipRoomAnnounce {
		out["skipRoomAnnounce"] = true
	}
	// Conductor-LLM fields. When the LLM conductor produces a per-turn
	// plan, these carry the load-bearing instructions; the prompt
	// template surfaces them at the top so they override stored
	// personality and habit.
	if d.Instruction != "" {
		out["instruction"] = d.Instruction
	}
	if d.GlobalGuidance != "" {
		out["globalGuidance"] = d.GlobalGuidance
	}
	if d.Phase != "" {
		out["phase"] = d.Phase
	}
	if d.Temperature != "" {
		out["temperature"] = d.Temperature
	}
	if d.UserIntent != "" {
		out["userIntent"] = d.UserIntent
	}
	if d.AcknowledgePrior != "" {
		out["acknowledgePrior"] = d.AcknowledgePrior
	}
	if d.ExpectedOutput != "" {
		out["expectedOutput"] = d.ExpectedOutput
	}
	return out
}

// directiveContextKey carries an AgentParticipationDirective through
// the local-generation path (generateSIResponse). The forwarder path
// uses Hints; the local path uses context. Same shape on the other
// side: si_responder reads it and stamps `directive` into prompt data.
type directiveContextKey struct{}

// contextWithDirective attaches a directive to the context. nil
// directives are no-ops.
func contextWithDirective(ctx context.Context, d *AgentParticipationDirective) context.Context {
	if d == nil {
		return ctx
	}
	return context.WithValue(ctx, directiveContextKey{}, d)
}

// directiveFromContext returns the directive attached to ctx, or nil.
func directiveFromContext(ctx context.Context) *AgentParticipationDirective {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(directiveContextKey{}).(*AgentParticipationDirective); ok {
		return v
	}
	return nil
}

// conductorStateOrNil returns the per-space conductor state if it
// exists, or nil. Used by the chime-in chain to read state without
// blocking on registry initialisation.
func conductorStateOrNil(c *CognitionIntegration, spaceId string) *ConductorState {
	if c == nil || c.conductors == nil {
		return nil
	}
	return c.conductors.Get(spaceId)
}

// queryAgentIsKnownToUser runs the cross-space "have we met before?"
// query for the given agent. Returns true if any prior utterance from
// this agent exists in the partition (any space the user shares with
// this agent). Used by the conductor to set Directive.SkipSelfIntro.
//
// Best-effort: query failures default to false (treat as first
// meeting -- safer than silently telling the agent the user knows
// them when nothing's verified). The lookup is partition-scoped so
// "first meeting" reads as truly first-time for new partitions /
// new tenants.
func (c *CognitionIntegration) queryAgentIsKnownToUser(ctx context.Context, agentId string) bool {
	if c == nil || c.engine == nil {
		return false
	}
	agentId = strings.TrimSpace(agentId)
	if agentId == "" {
		return false
	}
	query := fmt.Sprintf(`queryAgentInteractionCount({agentId: "%s"})`, agentId)
	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		if c.Logger != nil {
			c.Logger.Debug("conductor: queryAgentIsKnownToUser failed (defaulting to false)",
				"error", err, "agentId", agentId)
		}
		return false
	}
	// queryAgentInteractionCount uses shape() so rows land in the
	// adapter's "data" axis, not the top-level []any. The previous
	// implementation only checked the top-level shape and silently
	// returned false for everyone -- which made every agent feel
	// like a first-meeting on every turn from cognition's vantage,
	// even though replier was supposed to fall back. Centralised
	// row count handles both axes.
	if resMap, ok := result.(map[string]any); ok {
		switch d := resMap["data"].(type) {
		case []any:
			if len(d) > 0 {
				return true
			}
		case []map[string]any:
			if len(d) > 0 {
				return true
			}
		case map[string]any:
			if len(d) > 0 {
				return true
			}
		}
		if bundle, ok := resMap["bundle"].(map[string]any); ok {
			if nodes, ok := bundle["nodes"].([]any); ok && len(nodes) > 0 {
				return true
			}
		}
		return false
	}
	if rows, ok := result.([]any); ok && len(rows) > 0 {
		return true
	}
	if rows, ok := result.([]map[string]any); ok && len(rows) > 0 {
		return true
	}
	return false
}

// DecodeDirectiveFromHints reads a directive out of the agent's Hints map.
// Returns nil if no directive fields are present (the dispatch path
// didn't set one).
func DecodeDirectiveFromHints(hints map[string]string) *AgentParticipationDirective {
	if len(hints) == 0 {
		return nil
	}
	mode := hints[hintDirectiveMode]
	if mode == "" {
		return nil
	}
	d := &AgentParticipationDirective{
		Mode:        DirectiveMode(mode),
		Reason:      hints[hintDirectiveReason],
		ContentHint: hints[hintDirectiveContentHint],
		Brevity:     Brevity(hints[hintDirectiveBrevity]),
	}
	if hints[hintDirectiveSkipHandoffOpener] == "true" {
		d.SkipHandoffOpener = true
	}
	if hints[hintDirectiveSkipSelfIntro] == "true" {
		d.SkipSelfIntro = true
	}
	if hints[hintDirectiveSkipRoomAnnounce] == "true" {
		d.SkipRoomAnnounce = true
	}
	d.Instruction = hints[hintDirectiveInstruction]
	d.GlobalGuidance = hints[hintDirectiveGlobalGuidance]
	d.Phase = hints[hintDirectivePhase]
	d.Temperature = hints[hintDirectiveTemperature]
	d.UserIntent = hints[hintDirectiveUserIntent]
	d.AcknowledgePrior = hints[hintDirectiveAcknowledgePrior]
	d.ExpectedOutput = hints[hintDirectiveExpectedOutput]
	return d
}
