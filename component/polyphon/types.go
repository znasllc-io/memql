// Package polyphon implements the Polyphon multi-agent voice conversation
// orchestration system. It scores multiple AI agents, manages turn-taking,
// and decides which agent should respond to human utterances.
//
// Polyphon (from Greek "polyphōnía" — many voices) enables up to 3 AI agents
// and 5 humans to participate in natural real-time voice conversations.
package polyphon

import (
	"context"
	"sync"
	"time"
)

// VoicePlatform identifies which voice provider flavor is active.
type VoicePlatform string

const (
	// PlatformOpenAI uses OpenAI Realtime transcription (ASR) + OpenAI TTS API.
	// Current default.
	PlatformOpenAI VoicePlatform = "openai"
)

// MaxAgentsPerSpace is the maximum number of AI agents in a Polyphon session.
const MaxAgentsPerSpace = 3

// MaxHumansPerSpace is the maximum number of human participants in a Polyphon session.
const MaxHumansPerSpace = 5

// Utterance represents a transcribed speech segment from a human participant.
type Utterance struct {
	ID            string            `json:"id"`
	SpaceId       string            `json:"spaceId"`
	ParticipantId string            `json:"participantId"`
	SpeakerName   string            `json:"speakerName,omitempty"`
	Text          string            `json:"text"`
	IsFinal       bool              `json:"isFinal"`
	IsVoice       bool              `json:"isVoice,omitempty"`
	Timestamp     time.Time         `json:"timestamp"`
	Mentions      []Mention         `json:"mentions,omitempty"`
	Intent        *IntentResult     `json:"intent,omitempty"`
	Source        map[string]string `json:"source,omitempty"`
	Embedding     []float32         `json:"-"` // Pre-computed embedding for vector domain scoring
}

// MentionRole indicates how a participant was mentioned in an utterance.
type MentionRole string

const (
	MentionRoleAddressee MentionRole = "addressee"
	MentionRoleReference MentionRole = "reference"
)

// Mention represents a detected @ mention in an utterance.
type Mention struct {
	ParticipantId   string      `json:"participantId"`
	Name            string      `json:"name"`
	ParticipantType string      `json:"participantType"`
	Position        string      `json:"position"` // "start" or "mid"
	Role            MentionRole `json:"role"`
}

// AgentCandidate represents an AI agent being evaluated by the score engine.
type AgentCandidate struct {
	ID                   string                 `json:"id"`
	ParticipantId        string                 `json:"participantId"`
	Name                 string                 `json:"name"`
	Description          string                 `json:"description,omitempty"`
	Personality          string                 `json:"personality,omitempty"`
	Role                 string                 `json:"role,omitempty"` // e.g. "assistant", "accounting-finance"
	Domains              []string               `json:"domains,omitempty"`
	Keywords             []string               `json:"keywords,omitempty"`
	SpeakWhen            string                 `json:"speakWhen,omitempty"`            // "asked", "relevant", "always"
	InterruptStyle       string                 `json:"interruptStyle,omitempty"`       // "polite", "assertive", "passive"
	ContinuationBehavior string                 `json:"continuationBehavior,omitempty"` // "add_context", "disagree", "summarize"
	ProviderConfig       map[string]interface{} `json:"providerConfig,omitempty"`
	Capabilities         map[string]interface{} `json:"capabilities,omitempty"`
	ProfileEmbedding     []float32              `json:"-"` // Pre-loaded from node_vectors for vector domain scoring
}

// ScoringFactor represents a single factor contributing to an agent's score.
type ScoringFactor struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
	Value  float64 `json:"value"`  // Raw value (0.0–1.0)
	Score  float64 `json:"score"`  // Weight × Value
	Detail string  `json:"detail"` // Human-readable explanation
}

// AgentScore is the scoring result for a single agent candidate.
type AgentScore struct {
	AgentId       string          `json:"agentId"`
	AgentName     string          `json:"agentName"`
	TotalScore    float64         `json:"totalScore"` // Weighted sum (0–100)
	Factors       []ScoringFactor `json:"factors"`
	ShouldRespond bool            `json:"shouldRespond"`
	Reason        string          `json:"reason"`
	ToolsNeeded   bool            `json:"toolsNeeded,omitempty"` // AI router signals the agent needs tools for this utterance
}

// ScoreDecision is the output of the score engine for a given utterance.
type ScoreDecision struct {
	SpaceId           string            `json:"spaceId"`
	UtteranceId       string            `json:"utteranceId"`
	Winner            *AgentScore       `json:"winner,omitempty"`
	Scores            []AgentScore      `json:"scores"`
	Action            string            `json:"action"`     // "respond", "continue", "silence"
	Confidence        string            `json:"confidence"` // "high", "medium", "low" -- determines whether AI router is invoked
	Continuation      bool              `json:"continuation"`
	ConversationPhase ConversationPhase `json:"conversationPhase,omitempty"`
	ResponseDelay     time.Duration     `json:"-"`
	Timestamp         time.Time         `json:"timestamp"`
}

// HasWinner returns true if a winning agent was selected.
func (d *ScoreDecision) HasWinner() bool {
	return d != nil && d.Winner != nil && d.Winner.ShouldRespond
}

// TurnPolicyConfig holds configurable thresholds for turn-taking behavior.
type TurnPolicyConfig struct {
	// ResponseThreshold is the minimum score (0–100) for an agent to respond.
	ResponseThreshold float64 `json:"responseThreshold"`

	// ContinuationThreshold is the minimum score for a follow-up agent turn.
	ContinuationThreshold float64 `json:"continuationThreshold"`

	// MaxConsecutiveAgentTurns is the hard cap on agent turns before yielding to humans.
	MaxConsecutiveAgentTurns int `json:"maxConsecutiveAgentTurns"`

	// MinResponseDelayMs is the minimum "thinking" pause in milliseconds.
	MinResponseDelayMs int `json:"minResponseDelayMs"`

	// MaxResponseDelayMs is the maximum "thinking" pause in milliseconds.
	MaxResponseDelayMs int `json:"maxResponseDelayMs"`

	// RecencyDecayFactor controls how quickly recency score decreases (0.0–1.0).
	// Higher values = faster decay = less likely to repeat the same agent.
	RecencyDecayFactor float64 `json:"recencyDecayFactor"`

	// TextThreadTimeout is how long a conversational thread stays active for text input.
	TextThreadTimeout time.Duration `json:"-"`
	// VoiceThreadTimeout is how long a conversational thread stays active for voice input.
	VoiceThreadTimeout time.Duration `json:"-"`
}

// DefaultTurnPolicy returns a TurnPolicyConfig with sensible defaults.
// ResponseThreshold of 30 ensures agents with strong domain relevance
// can respond without requiring direct addressing.
func DefaultTurnPolicy() TurnPolicyConfig {
	return TurnPolicyConfig{
		ResponseThreshold:        30.0,
		ContinuationThreshold:    70.0,
		MaxConsecutiveAgentTurns: 2,
		MinResponseDelayMs:       300,
		MaxResponseDelayMs:       500,
		RecencyDecayFactor:       0.7,
		TextThreadTimeout:        120 * time.Second,
		VoiceThreadTimeout:       60 * time.Second,
	}
}

// ScoringWeights defines the weight (0–100) for each scoring factor.
// The sum of all weights should equal 100.
type ScoringWeights struct {
	DirectAddress         float64 `json:"directAddress"`
	DomainRelevance       float64 `json:"domainRelevance"`
	ConversationalThread  float64 `json:"conversationalThread"`
	ConversationRecency   float64 `json:"conversationRecency"`
	QuestionDetection     float64 `json:"questionDetection"`
	ContinuationRelevance float64 `json:"continuationRelevance"`
}

// DefaultScoringWeights returns the default scoring factor weights.
//
// The cognitionRouting AI prompt is the primary turn-taking decision; the
// heuristic scorer produces signals the router reads (per-agent fit,
// intent, mentions) rather than itself picking the winner. That's why
// ConversationalThread is zeroed: the router already sees the previous
// responder and recent transcript and reasons about continuity directly;
// adding a silent +15 per turn caused the same agent to dominate once
// they got the first word in. Direct address and domain relevance still
// carry weight because they're observable signals, not preferences.
func DefaultScoringWeights() ScoringWeights {
	return ScoringWeights{
		DirectAddress:         25.0,
		DomainRelevance:       35.0,
		ConversationalThread:  0.0, // see docstring -- the router handles continuity
		ConversationRecency:   5.0,
		QuestionDetection:     10.0,
		ContinuationRelevance: 10.0,
	}
}

// TranscriptEntry is a single entry in the shared conversation transcript.
type TranscriptEntry struct {
	ID          string    `json:"id"`
	SpaceId     string    `json:"spaceId"`
	SpeakerId   string    `json:"speakerId"`
	SpeakerName string    `json:"speakerName"`
	SpeakerType string    `json:"speakerType"` // "human" or "agent"
	Text        string    `json:"text"`
	Timestamp   time.Time `json:"timestamp"`
	UtteranceId string    `json:"utteranceId,omitempty"`
}

// PolyphonSession tracks the state of a multi-agent conversation in a space.
type PolyphonSession struct {
	mu sync.RWMutex

	SpaceId               string                    `json:"spaceId"`
	Platform              VoicePlatform             `json:"platform"`
	Humans                []SessionParticipant      `json:"humans"`
	Agents                []SessionAgent            `json:"agents"`
	Transcript            []TranscriptEntry         `json:"transcript"`
	ConsecutiveAgentTurns int                       `json:"consecutiveAgentTurns"`
	LastSpeakerId         string                    `json:"lastSpeakerId,omitempty"`
	LastSpeakerType       string                    `json:"lastSpeakerType,omitempty"` // "human" or "agent"
	LastAgentId           string                    `json:"lastAgentId,omitempty"`
	LastAddressedAgentId  string                    `json:"lastAddressedAgentId,omitempty"` // Conversational thread: who was last addressed by name
	LastAddressedAt       time.Time                 `json:"lastAddressedAt,omitempty"`      // When the thread was established
	HandoffChainDepth     int                       `json:"handoffChainDepth"`              // Hops in the current handoff chain; reset on non-handoff outcomes.
	AgentTurnCounts       map[string]int            `json:"agentTurnCounts"`
	StateMachine          *ConversationStateMachine `json:"-"`
	Prediction            *PredictiveState          `json:"prediction,omitempty"`
	PendingEvents         []map[string]any          `json:"pendingEvents,omitempty"` // Queue for notifications to surface
	CompactedSummary      string                    `json:"-"`                       // Zone 1: AI-generated summary of old messages
	LastCompactionAt      time.Time                 `json:"-"`                       // When compaction last ran
	CompactionIndex       int                       `json:"-"`                       // Transcript index up to which summary covers
	CreatedAt             time.Time                 `json:"createdAt"`
	LastActivityAt        time.Time                 `json:"lastActivityAt"`
}

// SessionParticipant represents a human participant in a Polyphon session.
type SessionParticipant struct {
	ParticipantId string    `json:"participantId"`
	DisplayName   string    `json:"displayName"`
	JoinedAt      time.Time `json:"joinedAt"`
}

// SessionAgent represents an AI agent participant in a Polyphon session.
type SessionAgent struct {
	AgentId       string    `json:"agentId"`
	ParticipantId string    `json:"participantId"`
	Name          string    `json:"name"`
	JoinedAt      time.Time `json:"joinedAt"`
}

// NewSession creates a new PolyphonSession for the given space.
func NewSession(spaceId string) *PolyphonSession {
	now := time.Now().UTC()
	return &PolyphonSession{
		SpaceId:         spaceId,
		Platform:        PlatformOpenAI,
		Humans:          make([]SessionParticipant, 0),
		Agents:          make([]SessionAgent, 0),
		Transcript:      make([]TranscriptEntry, 0),
		AgentTurnCounts: make(map[string]int),
		StateMachine:    NewConversationStateMachine(),
		CreatedAt:       now,
		LastActivityAt:  now,
	}
}

// AddTranscript appends a transcript entry and updates session state.
func (s *PolyphonSession) AddTranscript(entry TranscriptEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Transcript = append(s.Transcript, entry)
	s.LastActivityAt = entry.Timestamp
	s.LastSpeakerId = entry.SpeakerId
	s.LastSpeakerType = entry.SpeakerType

	if entry.SpeakerType == "agent" {
		s.ConsecutiveAgentTurns++
		s.LastAgentId = entry.SpeakerId
		s.AgentTurnCounts[entry.SpeakerId]++
	} else {
		// Human spoke — reset consecutive agent counter.
		s.ConsecutiveAgentTurns = 0
	}
}

// RecentTranscript returns the last n entries from the transcript.
func (s *PolyphonSession) RecentTranscript(n int) []TranscriptEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if n <= 0 || len(s.Transcript) == 0 {
		return nil
	}
	start := len(s.Transcript) - n
	if start < 0 {
		start = 0
	}
	out := make([]TranscriptEntry, len(s.Transcript)-start)
	copy(out, s.Transcript[start:])
	return out
}

// AgentTurnsSinceHuman returns how many consecutive agent turns have occurred
// since the last human spoke.
func (s *PolyphonSession) AgentTurnsSinceHuman() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ConsecutiveAgentTurns
}

// SetAddressedAgent updates the conversational thread to the given agent.
func (s *PolyphonSession) SetAddressedAgent(agentId string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastAddressedAgentId = agentId
	s.LastAddressedAt = time.Now().UTC()
}

// GetAddressedAgent returns the currently addressed agent ID and when it was set.
// Returns empty string and zero time if no active thread.
func (s *PolyphonSession) GetAddressedAgent() (string, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.LastAddressedAgentId, s.LastAddressedAt
}

// GetHandoffChainDepth returns the current handoff chain depth. 0 means the
// most recent turn was not a handoff (specialist answered or general-
// assistant fallback with no prior handoff); >0 means we are inside a
// chain of handoffs of that length.
func (s *PolyphonSession) GetHandoffChainDepth() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.HandoffChainDepth
}

// RecordHandoffOutcome updates chain depth based on the current turn's
// handoff flag: increments by 1 when handoff=true, resets to 0 when false.
// Called by the router after each LLM routing decision.
func (s *PolyphonSession) RecordHandoffOutcome(handoff bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if handoff {
		s.HandoffChainDepth++
		return
	}
	s.HandoffChainDepth = 0
}

// SetPrediction updates the session's predictive state.
func (s *PolyphonSession) SetPrediction(state *PredictiveState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Prediction = state
}

// GetPrediction returns the current predictive state, or nil if unavailable.
func (s *PolyphonSession) GetPrediction() *PredictiveState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Prediction
}

// LastAgentSpokeAt returns the most recent time the given agent spoke,
// or zero time if the agent hasn't spoken.
func (s *PolyphonSession) LastAgentSpokeAt(agentId string) time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i := len(s.Transcript) - 1; i >= 0; i-- {
		e := s.Transcript[i]
		if e.SpeakerType == "agent" && e.SpeakerId == agentId {
			return e.Timestamp
		}
	}
	return time.Time{}
}

// =============================================================================
// Heartbeat System
// =============================================================================

// HeartbeatAction represents a proactive action the score engine can take
// during a heartbeat tick. The heartbeat runs continuously per active space,
// evaluating whether the score engine should intervene.
type HeartbeatAction struct {
	Type      string         `json:"type"`    // "idle", "re-engage", "notify", "proactive"
	AgentId   string         `json:"agentId"` // Which agent should act
	AgentName string         `json:"agentName"`
	Reason    string         `json:"reason"`            // Why this action was chosen
	Payload   map[string]any `json:"payload,omitempty"` // Action-specific data
}

// HeartbeatEvaluator evaluates whether the score engine should take proactive
// action during a heartbeat tick. Implementations can be swapped for
// different strategies (e.g., rule-based, AI-powered, hybrid).
type HeartbeatEvaluator interface {
	Evaluate(session *PolyphonSession, candidates []AgentCandidate) *HeartbeatAction
}

// =============================================================================
// Predictive Analysis Framework (foundations)
// =============================================================================

// PredictiveState represents the score engine's anticipation of where the
// conversation is heading. Updated asynchronously by the PredictiveAnalyzer.
//
// FUTURE: The actual prediction logic (AI-powered conversation phase
// classification, topic anticipation, intent forecasting) will be added
// in a follow-up implementation. This type and the PredictiveAnalyzer
// interface establish the extension points.
type PredictiveState struct {
	// AnticipatedTopics are subjects the conversation is likely to explore next.
	AnticipatedTopics []string `json:"anticipatedTopics,omitempty"`
	// LikelyNextAgent is the agent most likely to be needed next.
	LikelyNextAgent string `json:"likelyNextAgent,omitempty"`
	// Confidence in the prediction (0.0-1.0).
	Confidence float64 `json:"confidence,omitempty"`
	// ConversationPhase classifies the current stage of the conversation.
	// Values: "greeting", "exploration", "task", "decision", "winding-down"
	ConversationPhase string `json:"conversationPhase,omitempty"`
	// SuggestedAction is a proactive action the score engine could take.
	SuggestedAction string `json:"suggestedAction,omitempty"`
	// LastUpdated is when this prediction was last computed.
	LastUpdated time.Time `json:"lastUpdated,omitempty"`
}

// PredictiveAnalyzer runs async analysis on the conversation to anticipate
// what might happen next. The score engine uses this to make better routing
// decisions and take proactive actions.
//
// FUTURE: Initial implementation will use a cheap AI model call to classify
// conversation phase and anticipate topics. More sophisticated prediction
// (topic modeling, intent classification, conversation trajectory) can be
// layered on later.
type PredictiveAnalyzer interface {
	// Analyze processes recent conversation state and returns a predictive
	// snapshot. Called asynchronously -- must not block the main flow.
	Analyze(ctx context.Context, session *PolyphonSession, candidates []AgentCandidate) (*PredictiveState, error)
}
