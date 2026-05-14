package polyphon

import (
	"strings"
	"time"
)

// DefaultHeartbeatEvaluator implements HeartbeatEvaluator with rule-based
// proactive behavior. It checks for silence, pending events, and predictive
// state to decide whether the score engine should take action.
type DefaultHeartbeatEvaluator struct {
	// SilenceThreshold is how long silence must last before re-engagement.
	// Default: 3 minutes.
	SilenceThreshold time.Duration
	// ClarifyingQuestionGrace is how long to wait after an agent asks a
	// question before considering re-engagement (the human may be typing).
	// Default: 30 seconds.
	ClarifyingQuestionGrace time.Duration
}

// NewDefaultHeartbeatEvaluator creates a heartbeat evaluator with sensible defaults.
func NewDefaultHeartbeatEvaluator() *DefaultHeartbeatEvaluator {
	return &DefaultHeartbeatEvaluator{
		SilenceThreshold:        3 * time.Minute,
		ClarifyingQuestionGrace: 30 * time.Second,
	}
}

// Evaluate checks the session state and returns a HeartbeatAction.
// Returns nil if no action should be taken (idle).
func (e *DefaultHeartbeatEvaluator) Evaluate(session *PolyphonSession, candidates []AgentCandidate) *HeartbeatAction {
	if session == nil || len(candidates) == 0 {
		return nil
	}

	now := time.Now().UTC()
	silenceDuration := now.Sub(session.LastActivityAt)

	// Check pending events queue.
	// FUTURE: When notifications, data updates, or external events arrive,
	// they are queued in session.PendingEvents. The heartbeat surfaces them
	// to the appropriate agent.
	if len(session.PendingEvents) > 0 {
		// Find the general assistant (or first agent) to deliver notifications.
		agent := findGeneralAssistant(candidates)
		if agent == nil && len(candidates) > 0 {
			agent = &candidates[0]
		}
		if agent != nil {
			return &HeartbeatAction{
				Type:      "notify",
				AgentId:   agent.ID,
				AgentName: agent.Name,
				Reason:    "Pending events to surface",
				Payload:   map[string]any{"eventCount": len(session.PendingEvents)},
			}
		}
	}

	// Check predictive state for proactive actions.
	// FUTURE: The PredictiveAnalyzer will populate session.Prediction with
	// anticipated topics, conversation phase, and suggested actions. The
	// heartbeat can use this to proactively prepare or offer information.
	if pred := session.GetPrediction(); pred != nil && pred.SuggestedAction != "" {
		agent := findAgentById(candidates, pred.LikelyNextAgent)
		if agent == nil {
			agent = findGeneralAssistant(candidates)
		}
		if agent != nil {
			return &HeartbeatAction{
				Type:      "proactive",
				AgentId:   agent.ID,
				AgentName: agent.Name,
				Reason:    pred.SuggestedAction,
				Payload: map[string]any{
					"conversationPhase": pred.ConversationPhase,
					"confidence":        pred.Confidence,
				},
			}
		}
	}

	// Silence detection: should the agent re-engage?
	if silenceDuration >= e.SilenceThreshold {
		// Check if the last agent message was a question -- if so, give the
		// human more grace time (they may be thinking/typing).
		recent := session.RecentTranscript(1)
		if len(recent) > 0 {
			last := recent[0]
			if last.SpeakerType == "agent" && strings.HasSuffix(strings.TrimSpace(last.Text), "?") {
				// Agent asked a question. Only re-engage after the grace period
				// on top of the silence threshold.
				if silenceDuration < e.SilenceThreshold+e.ClarifyingQuestionGrace {
					return nil // Still within grace period.
				}
			}
		}

		// Pick the general assistant for re-engagement.
		agent := findGeneralAssistant(candidates)
		if agent == nil && len(candidates) > 0 {
			agent = &candidates[0]
		}
		if agent != nil {
			return &HeartbeatAction{
				Type:      "re-engage",
				AgentId:   agent.ID,
				AgentName: agent.Name,
				Reason:    "Extended silence in active conversation",
				Payload:   map[string]any{"silenceSeconds": int(silenceDuration.Seconds())},
			}
		}
	}

	// No action needed.
	return nil
}

// findGeneralAssistant returns the first candidate with Role "general_assistant".
func findGeneralAssistant(candidates []AgentCandidate) *AgentCandidate {
	for i := range candidates {
		if candidates[i].Role == "general_assistant" {
			return &candidates[i]
		}
	}
	return nil
}

// findAgentById returns the candidate matching the given ID.
func findAgentById(candidates []AgentCandidate, id string) *AgentCandidate {
	for i := range candidates {
		if candidates[i].ID == id {
			return &candidates[i]
		}
	}
	return nil
}
