package polyphon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// NoOpPredictiveAnalyzer is a placeholder that returns no prediction.
// Used when no AI engine is available (tests, standalone mode).
type NoOpPredictiveAnalyzer struct{}

func (n *NoOpPredictiveAnalyzer) Analyze(_ context.Context, _ *PolyphonSession, _ []AgentCandidate) (*PredictiveState, error) {
	return nil, nil
}

// ModelPredictiveAnalyzer uses a fast AI model to predict conversation trajectory.
// Runs asynchronously via the prediction goroutine (every 30s per active space).
type ModelPredictiveAnalyzer struct {
	// invokeAI is a function that calls the AI engine to run a prediction prompt.
	// Injected to avoid direct engine dependency in the polyphon package.
	invokeAI func(ctx context.Context, templateId string, data map[string]any) (any, error)

	// embedFunc is an optional function that computes a vector embedding for text.
	// When set, enables vector-based phase detection for faster classification.
	embedFunc func(ctx context.Context, text string) ([]float32, error)

	// phaseLibrary holds pre-embedded conversation phase patterns for vector detection.
	phaseLibrary *PhasePatternLibrary
}

// NewModelPredictiveAnalyzer creates an analyzer that uses AI for prediction.
// The embedFunc parameter is optional (pass nil to disable vector-based phase detection).
func NewModelPredictiveAnalyzer(
	invokeAI func(ctx context.Context, templateId string, data map[string]any) (any, error),
	embedFunc func(ctx context.Context, text string) ([]float32, error),
) *ModelPredictiveAnalyzer {
	lib := NewPhasePatternLibrary()
	return &ModelPredictiveAnalyzer{
		invokeAI:     invokeAI,
		embedFunc:    embedFunc,
		phaseLibrary: lib,
	}
}

func (a *ModelPredictiveAnalyzer) Analyze(ctx context.Context, session *PolyphonSession, candidates []AgentCandidate) (*PredictiveState, error) {
	if a.invokeAI == nil {
		return nil, nil
	}
	if session == nil {
		return nil, nil
	}

	// Only analyze if conversation was recently active (within 5 minutes).
	session.mu.RLock()
	lastActivity := session.LastActivityAt

	// Collect enriched session metadata under the same lock.
	agentTurnCounts := make(map[string]int, len(session.AgentTurnCounts))
	for k, v := range session.AgentTurnCounts {
		agentTurnCounts[k] = v
	}
	threadHolder := session.LastAddressedAgentId
	threadAt := session.LastAddressedAt

	lastHuman := ""
	for i := len(session.Transcript) - 1; i >= 0; i-- {
		if session.Transcript[i].SpeakerType == "human" {
			lastHuman = session.Transcript[i].SpeakerName
			break
		}
	}
	session.mu.RUnlock()

	if time.Since(lastActivity) > 5*time.Minute {
		return nil, nil
	}

	// Build input data from session state.
	recent := session.RecentTranscript(5)
	if len(recent) == 0 {
		return nil, nil
	}

	transcript := make([]map[string]any, 0, len(recent))
	for _, entry := range recent {
		transcript = append(transcript, map[string]any{
			"speakerName": entry.SpeakerName,
			"speakerType": entry.SpeakerType,
			"text":        entry.Text,
		})
	}

	agents := make([]map[string]any, 0, len(candidates))
	for _, c := range candidates {
		agents = append(agents, map[string]any{
			"name":    c.Name,
			"role":    c.Role,
			"domains": c.Domains,
		})
	}

	phase := ""
	if session.StateMachine != nil {
		phase = string(session.StateMachine.Phase())
	}

	data := map[string]any{
		"transcript":      transcript,
		"agents":          agents,
		"phase":           phase,
		"agentTurnCounts": agentTurnCounts,
	}

	if threadHolder != "" {
		data["threadHolder"] = threadHolder
		_ = threadAt // available for future use
	}
	if lastHuman != "" {
		data["lastHumanSpeaker"] = lastHuman
	}
	data["timeSinceLastHuman"] = time.Since(lastActivity).Seconds()

	// Try vector-based phase detection first (fast, ~2-5ms).
	if a.embedFunc != nil && len(recent) > 0 {
		lastText := recent[len(recent)-1].Text
		embedding, err := a.embedFunc(ctx, lastText)
		if err == nil && len(embedding) > 0 {
			detectedPhase, confidence := a.phaseLibrary.DetectPhase(embedding)
			if confidence > 0.8 && detectedPhase != "" {
				// High-confidence vector detection -- use it for phase.
				// Still call AI for topic anticipation and agent prediction.
				data["detectedPhase"] = string(detectedPhase)
				data["phaseConfidence"] = confidence
			}
		}
	}

	result, err := a.invokeAI(ctx, "cognitionPrediction", data)
	if err != nil {
		// Fallback: pattern-based prediction.
		return patternBasedPrediction(session, candidates), nil
	}

	return parsePredictionResult(result)
}

// patternBasedPrediction provides a simple rule-based fallback when the AI model
// is unavailable. Uses conversation phase and turn counts to produce a basic prediction.
func patternBasedPrediction(session *PolyphonSession, candidates []AgentCandidate) *PredictiveState {
	state := &PredictiveState{
		LastUpdated: time.Now(),
		Confidence:  0.4,
	}

	// Determine phase from state machine if available.
	if session.StateMachine != nil {
		state.ConversationPhase = string(session.StateMachine.Phase())
	}

	// Anticipate based on current phase.
	switch ConversationPhase(state.ConversationPhase) {
	case PhaseGreeting:
		state.AnticipatedTopics = []string{"introductions", "capabilities", "what brings you here"}
		state.SuggestedAction = "introduce available agents"
	case PhaseExploration:
		state.AnticipatedTopics = []string{"domain questions", "specific help requests"}
	case PhaseTask:
		state.AnticipatedTopics = []string{"follow-up questions", "clarifications", "next steps"}
	case PhaseFollowUp:
		state.AnticipatedTopics = []string{"new topic", "additional questions", "wrap up"}
	case PhaseWindingDown:
		state.SuggestedAction = "summarize discussion"
	}

	// Find agent with fewest turns as likely next (anti-monopoly).
	minTurns := int(^uint(0) >> 1) // MaxInt
	session.mu.RLock()
	turnCounts := session.AgentTurnCounts
	session.mu.RUnlock()
	for _, c := range candidates {
		turns := 0
		if turnCounts != nil {
			turns = turnCounts[c.ID]
		}
		if turns < minTurns {
			minTurns = turns
			state.LikelyNextAgent = c.Name
		}
	}

	return state
}

// ---------------------------------------------------------------------------
// ExternalPredictiveAnalyzer (Phase 3.5)
// ---------------------------------------------------------------------------

// ExternalPredictiveAnalyzer calls an external HTTP endpoint for prediction.
// Activated when MEMQL_POLYPHON_PREDICTION_ENGINE_URL is set.
type ExternalPredictiveAnalyzer struct {
	endpoint string
	client   *http.Client
	timeout  time.Duration
	fallback PredictiveAnalyzer // Falls back to this if external fails
}

// NewExternalPredictiveAnalyzer creates an analyzer that calls an external service.
func NewExternalPredictiveAnalyzer(endpoint string, fallback PredictiveAnalyzer) *ExternalPredictiveAnalyzer {
	return &ExternalPredictiveAnalyzer{
		endpoint: endpoint,
		client:   &http.Client{},
		timeout:  5 * time.Second,
		fallback: fallback,
	}
}

func (a *ExternalPredictiveAnalyzer) Analyze(ctx context.Context, session *PolyphonSession, candidates []AgentCandidate) (*PredictiveState, error) {
	if a.endpoint == "" {
		if a.fallback != nil {
			return a.fallback.Analyze(ctx, session, candidates)
		}
		return nil, nil
	}

	// Build request payload.
	recent := session.RecentTranscript(5)
	transcript := make([]map[string]any, 0, len(recent))
	for _, entry := range recent {
		transcript = append(transcript, map[string]any{
			"speakerName": entry.SpeakerName,
			"speakerType": entry.SpeakerType,
			"text":        entry.Text,
		})
	}
	agents := make([]map[string]any, 0, len(candidates))
	for _, c := range candidates {
		agents = append(agents, map[string]any{
			"name": c.Name, "role": c.Role, "domains": c.Domains,
		})
	}
	phase := ""
	if session.StateMachine != nil {
		phase = string(session.StateMachine.Phase())
	}

	payload := map[string]any{
		"transcript": transcript,
		"agents":     agents,
		"phase":      phase,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return a.fallbackAnalyze(ctx, session, candidates, err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "POST", a.endpoint, bytes.NewReader(body))
	if err != nil {
		return a.fallbackAnalyze(ctx, session, candidates, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return a.fallbackAnalyze(ctx, session, candidates, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return a.fallbackAnalyze(ctx, session, candidates, err)
	}

	if resp.StatusCode != http.StatusOK {
		return a.fallbackAnalyze(ctx, session, candidates, fmt.Errorf("external prediction returned %d", resp.StatusCode))
	}

	return parsePredictionResult(string(respBody))
}

func (a *ExternalPredictiveAnalyzer) fallbackAnalyze(ctx context.Context, session *PolyphonSession, candidates []AgentCandidate, err error) (*PredictiveState, error) {
	if a.fallback != nil {
		return a.fallback.Analyze(ctx, session, candidates)
	}
	return nil, err
}

// ---------------------------------------------------------------------------
// Vector-Based Phase Detection (Phase 3.7)
// ---------------------------------------------------------------------------

// PhasePatternLibrary holds pre-embedded conversation phase patterns for fast detection.
type PhasePatternLibrary struct {
	patterns map[ConversationPhase][]phasePattern
}

type phasePattern struct {
	text      string
	embedding []float32
}

// NewPhasePatternLibrary creates a pattern library. Call InitEmbeddings() after creation
// to pre-compute embeddings for all patterns.
func NewPhasePatternLibrary() *PhasePatternLibrary {
	return &PhasePatternLibrary{
		patterns: map[ConversationPhase][]phasePattern{
			PhaseGreeting: {
				{text: "hello everyone"},
				{text: "hi team good morning"},
				{text: "hey there how is everyone"},
				{text: "good afternoon everyone"},
			},
			PhaseExploration: {
				{text: "who can help me with this"},
				{text: "what agents are available here"},
				{text: "what can you all do"},
				{text: "who handles technical issues"},
			},
			PhaseTask: {
				{text: "show me the quarterly report"},
				{text: "help me fix this bug in the code"},
				{text: "can you check the server logs"},
				{text: "create a summary of the meeting"},
			},
			PhaseFollowUp: {
				{text: "yes that sounds right"},
				{text: "the second option please"},
				{text: "in this space not the other one"},
				{text: "ok go ahead with that"},
			},
			PhaseWindingDown: {
				{text: "thanks everyone that is all"},
				{text: "goodbye see you later"},
				{text: "that covers everything thanks"},
				{text: "great work team talk soon"},
			},
		},
	}
}

// InitEmbeddings pre-computes embeddings for all patterns using the given embed function.
// Call this once at startup.
func (lib *PhasePatternLibrary) InitEmbeddings(ctx context.Context, embedFunc func(ctx context.Context, text string) ([]float32, error)) error {
	if embedFunc == nil {
		return nil
	}
	for phase, patterns := range lib.patterns {
		for i, p := range patterns {
			embedding, err := embedFunc(ctx, p.text)
			if err != nil {
				return fmt.Errorf("embed phase pattern %q: %w", p.text, err)
			}
			lib.patterns[phase][i].embedding = embedding
		}
	}
	return nil
}

// DetectPhase compares an utterance embedding against pattern clusters and returns
// the best-matching phase with a confidence score.
func (lib *PhasePatternLibrary) DetectPhase(utteranceEmbedding []float32) (ConversationPhase, float64) {
	if len(utteranceEmbedding) == 0 {
		return "", 0
	}

	bestPhase := ConversationPhase("")
	bestSimilarity := 0.0

	for phase, patterns := range lib.patterns {
		maxSim := 0.0
		for _, p := range patterns {
			if len(p.embedding) == 0 {
				continue
			}
			sim := cosineSimilarityF32(utteranceEmbedding, p.embedding)
			if sim > maxSim {
				maxSim = sim
			}
		}
		if maxSim > bestSimilarity {
			bestSimilarity = maxSim
			bestPhase = phase
		}
	}

	return bestPhase, bestSimilarity
}

// cosineSimilarityF32 computes cosine similarity between two float32 vectors.
func cosineSimilarityF32(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// ---------------------------------------------------------------------------
// Shared parsing
// ---------------------------------------------------------------------------

func parsePredictionResult(result any) (*PredictiveState, error) {
	if result == nil {
		return nil, nil
	}

	// Handle string result (may be JSON).
	var m map[string]any
	switch v := result.(type) {
	case string:
		v = strings.TrimSpace(v)
		// Strip markdown fences if present.
		v = strings.TrimPrefix(v, "```json")
		v = strings.TrimPrefix(v, "```")
		v = strings.TrimSuffix(v, "```")
		v = strings.TrimSpace(v)
		if err := json.Unmarshal([]byte(v), &m); err != nil {
			return nil, fmt.Errorf("failed to parse prediction JSON: %w", err)
		}
	case map[string]any:
		m = v
	default:
		return nil, fmt.Errorf("unexpected prediction result type: %T", result)
	}

	state := &PredictiveState{
		LastUpdated: time.Now(),
	}

	if topics, ok := m["anticipatedTopics"].([]any); ok {
		for _, t := range topics {
			if s, ok := t.(string); ok {
				state.AnticipatedTopics = append(state.AnticipatedTopics, s)
			}
		}
	}
	if agent, ok := m["likelyNextAgent"].(string); ok {
		state.LikelyNextAgent = agent
	}
	if conf, ok := m["confidence"].(float64); ok {
		state.Confidence = conf
	}
	if phase, ok := m["conversationPhase"].(string); ok {
		state.ConversationPhase = phase
	}
	if action, ok := m["suggestedAction"].(string); ok {
		state.SuggestedAction = action
	}

	return state, nil
}
