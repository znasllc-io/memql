package polyphon

import (
	"context"
	"log/slog"
	"time"
)

// ScoreEngine is the Polyphon multi-agent scoring engine. It receives utterances,
// It receives utterances, scores all agents, applies turn-taking policy, and returns
// a decision about which agent should respond.
type ScoreEngine struct {
	scorer   *Scorer
	policy   *TurnPolicy
	sessions *SessionManager
	logger   *slog.Logger
}

// NewScoreEngine creates a new ScoreEngine with default configuration.
func NewScoreEngine(logger *slog.Logger) *ScoreEngine {
	policy := DefaultTurnPolicy()
	return &ScoreEngine{
		scorer:   NewScorer(DefaultScoringWeights(), policy),
		policy:   NewTurnPolicy(policy),
		sessions: NewSessionManager(logger),
		logger:   logger,
	}
}

// NewScoreEngineWithConfig creates a ScoreEngine with custom scoring weights and turn policy.
func NewScoreEngineWithConfig(weights ScoringWeights, policyConfig TurnPolicyConfig, logger *slog.Logger) *ScoreEngine {
	return &ScoreEngine{
		scorer:   NewScorer(weights, policyConfig),
		policy:   NewTurnPolicy(policyConfig),
		sessions: NewSessionManager(logger),
		logger:   logger,
	}
}

// ProcessUtterance evaluates a human utterance and decides which agent (if any)
// should respond. This is the main entry point for the ScoreEngine.
func (c *ScoreEngine) ProcessUtterance(_ context.Context, utterance Utterance, candidates []AgentCandidate) *ScoreDecision {
	session := c.sessions.GetOrCreate(utterance.SpaceId)

	// Record the human utterance in the session transcript.
	c.sessions.RecordUtterance(utterance.SpaceId, TranscriptEntry{
		ID:          utterance.ID,
		SpaceId:     utterance.SpaceId,
		SpeakerId:   utterance.ParticipantId,
		SpeakerName: utterance.SpeakerName,
		SpeakerType: "human",
		Text:        utterance.Text,
		Timestamp:   utterance.Timestamp,
		UtteranceId: utterance.ID,
	})

	// Score all agent candidates.
	scores := c.scorer.ScoreAll(candidates, utterance, session)

	// Update conversational thread: if any agent was primarily addressed (value=1.0),
	// switch the thread to that agent. This is checked AFTER scoring so the thread
	// factor in this utterance reflects the PREVIOUS thread state.
	for _, score := range scores {
		for _, f := range score.Factors {
			if f.Name == "direct_address" && f.Value >= 1.0 {
				session.SetAddressedAgent(score.AgentId)
				break
			}
		}
	}

	// Apply turn-taking policy.
	scores = c.policy.EvaluateScores(scores, session)

	decision := &ScoreDecision{
		SpaceId:     utterance.SpaceId,
		UtteranceId: utterance.ID,
		Scores:      scores,
		Action:      "silence",
		Timestamp:   time.Now().UTC(),
	}

	// Find the winning agent (highest score that passes threshold).
	for i := range scores {
		if scores[i].ShouldRespond {
			decision.Winner = &scores[i]
			decision.Action = "respond"
			decision.ResponseDelay = c.policy.ResponseDelay()
			break
		}
	}

	// Transition conversation state machine based on utterance intent.
	if utterance.Intent != nil && session.StateMachine != nil {
		session.StateMachine.Transition(utterance.Intent.Primary)
		decision.ConversationPhase = session.StateMachine.Phase()
	}

	// Classify confidence to determine whether the SI router should be invoked.
	decision.Confidence = classifyConfidence(scores, candidates, utterance)

	// Always update the thread to the winning agent. This ensures follow-up
	// replies (e.g., answering a clarifying question) are routed to the same
	// agent, even if the winner wasn't directly addressed by name.
	if decision.HasWinner() {
		session.SetAddressedAgent(decision.Winner.AgentId)
	}

	if c.logger != nil {
		if decision.HasWinner() {
			c.logger.Debug("polyphon: score engine decision",
				"spaceId", utterance.SpaceId,
				"utteranceId", utterance.ID,
				"winner", decision.Winner.AgentName,
				"score", decision.Winner.TotalScore,
				"confidence", decision.Confidence,
				"reason", decision.Winner.Reason,
				"delay", decision.ResponseDelay.String(),
			)
		} else {
			topScore := 0.0
			topAgent := ""
			if len(scores) > 0 {
				topScore = scores[0].TotalScore
				topAgent = scores[0].AgentName
			}
			c.logger.Debug("polyphon: score engine decision (silence)",
				"spaceId", utterance.SpaceId,
				"utteranceId", utterance.ID,
				"confidence", decision.Confidence,
				"topAgent", topAgent,
				"topScore", topScore,
			)
		}
	}

	return decision
}

// RecordAgentResponse records that an agent produced a response. Call this after
// the agent's response has been generated and is being delivered.
// Returns a continuation decision: should another agent follow up?
func (c *ScoreEngine) RecordAgentResponse(spaceId string, agentId, agentName, responseText string, candidates []AgentCandidate) (bool, *AgentScore) {
	// Record the agent's response in the transcript.
	c.sessions.RecordUtterance(spaceId, TranscriptEntry{
		SpaceId:     spaceId,
		SpeakerId:   agentId,
		SpeakerName: agentName,
		SpeakerType: "agent",
		Text:        responseText,
		Timestamp:   time.Now().UTC(),
	})

	session := c.sessions.Get(spaceId)
	if session == nil {
		return false, nil
	}

	// Build a synthetic utterance from the agent's response to score continuation.
	syntheticUtterance := Utterance{
		SpaceId:   spaceId,
		Text:      responseText,
		Timestamp: time.Now().UTC(),
	}

	// Re-score all agents for continuation.
	scores := c.scorer.ScoreAll(candidates, syntheticUtterance, session)
	scores = c.policy.EvaluateScores(scores, session)

	shouldContinue, nextAgent := c.policy.ShouldContinue(scores, session)

	if shouldContinue && c.logger != nil {
		c.logger.Debug("polyphon: continuation triggered",
			"spaceId", spaceId,
			"previousAgent", agentName,
			"nextAgent", nextAgent.AgentName,
			"nextScore", nextAgent.TotalScore,
			"consecutiveTurns", session.AgentTurnsSinceHuman(),
		)
	}

	return shouldContinue, nextAgent
}

// Sessions returns the SessionManager for direct session operations.
func (c *ScoreEngine) Sessions() *SessionManager {
	return c.sessions
}

// Policy returns the TurnPolicy for external inspection.
func (c *ScoreEngine) Policy() *TurnPolicy {
	return c.policy
}

// Scorer returns the Scorer for external inspection.
func (c *ScoreEngine) Scorer() *Scorer {
	return c.scorer
}

// classifyConfidence determines how confident the heuristic scoring is about
// its winner. Used to decide whether the SI router should be invoked:
//   - "high": clear Tier 1 match (direct address, solo agent, continuation) -- skip SI router
//   - "low": ambiguous scores -- SI router should be invoked
func classifyConfidence(scores []AgentScore, candidates []AgentCandidate, utterance Utterance) string {
	if len(scores) == 0 {
		return "low"
	}
	top := scores[0]

	// If ANY agent (not just the top scorer) is mentioned by name, the AI
	// router must decide. The user might be asking to switch agents.
	for _, s := range scores {
		for _, f := range s.Factors {
			if f.Name == "direct_address" && f.Value > 0 && s.AgentId != top.AgentId {
				return "low"
			}
		}
	}

	// High: intent is direct_address with a clear addressee mention.
	if utterance.Intent != nil && utterance.Intent.Primary == IntentDirectAddress {
		for _, f := range top.Factors {
			if f.Name == "direct_address" && f.Value >= 1.0 {
				return "high"
			}
		}
	}

	// High: solo agent (1:1 space).
	if len(candidates) == 1 {
		return "high"
	}

	// High: follow-up or affirmation with active thread, but ONLY if the winner
	// has a clear lead. When scores are close (spread < 15), defer to the SI Router
	// for better intent-based routing.
	if utterance.Intent != nil {
		switch utterance.Intent.Primary {
		case IntentFollowUp, IntentAffirmation:
			hasContinuation := false
			for _, f := range top.Factors {
				if f.Name == "clarifying_question_continuation" || f.Name == "short_reply_continuation" ||
					(f.Name == "conversational_thread" && f.Value >= 1.0) {
					hasContinuation = true
					break
				}
			}
			if hasContinuation {
				if len(scores) >= 2 && scores[0].TotalScore-scores[1].TotalScore < 15 {
					return "low" // Close race: SI Router decides.
				}
				return "high"
			}
		}
	}

	// High: clarifying question continuation (legacy path without intent).
	// Same spread check applies.
	for _, f := range top.Factors {
		if f.Name == "clarifying_question_continuation" || f.Name == "short_reply_continuation" {
			if len(scores) >= 2 && scores[0].TotalScore-scores[1].TotalScore < 15 {
				return "low"
			}
			return "high"
		}
	}

	// Everything else: AI Router decides.
	return "low"
}
