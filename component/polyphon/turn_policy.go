package polyphon

import (
	"math/rand"
	"time"
)

// TurnPolicy enforces turn-taking rules for multi-agent conversations.
type TurnPolicy struct {
	Config TurnPolicyConfig
}

// NewTurnPolicy creates a TurnPolicy with the given configuration.
func NewTurnPolicy(cfg TurnPolicyConfig) *TurnPolicy {
	return &TurnPolicy{Config: cfg}
}

// EvaluateScores takes the scored agents and the session state, and marks which
// agents should respond. It applies the response threshold and handles
// the speakWhen="always" case.
func (p *TurnPolicy) EvaluateScores(scores []AgentScore, session *PolyphonSession) []AgentScore {
	if len(scores) == 0 {
		return scores
	}

	for i := range scores {
		scores[i].ShouldRespond = scores[i].TotalScore >= p.Config.ResponseThreshold
	}

	return scores
}

// ShouldContinue determines whether another agent should speak after the current
// agent just finished. It checks the continuation threshold and the consecutive
// agent turn limit.
func (p *TurnPolicy) ShouldContinue(scores []AgentScore, session *PolyphonSession) (bool, *AgentScore) {
	if session == nil {
		return false, nil
	}

	// Hard cap: don't exceed max consecutive agent turns.
	if session.AgentTurnsSinceHuman() >= p.Config.MaxConsecutiveAgentTurns {
		return false, nil
	}

	// Find the highest-scoring agent that hasn't just spoken.
	for i := range scores {
		s := &scores[i]
		if s.AgentId == session.LastAgentId {
			continue // Skip the agent that just spoke
		}
		if s.TotalScore >= p.Config.ContinuationThreshold {
			return true, s
		}
	}

	return false, nil
}

// ResponseDelay returns a randomized "thinking" pause duration to make
// the agent response feel more natural.
func (p *TurnPolicy) ResponseDelay() time.Duration {
	min := p.Config.MinResponseDelayMs
	max := p.Config.MaxResponseDelayMs
	if min <= 0 {
		min = 300
	}
	if max <= min {
		max = min + 200
	}

	delay := min + rand.Intn(max-min)
	return time.Duration(delay) * time.Millisecond
}

// CanAgentRespond checks if a specific agent is allowed to respond given
// the current session state and policy constraints.
func (p *TurnPolicy) CanAgentRespond(agentId string, score float64, session *PolyphonSession) bool {
	if score < p.Config.ResponseThreshold {
		return false
	}

	if session == nil {
		return true
	}

	// Check consecutive agent turn limit.
	if session.AgentTurnsSinceHuman() >= p.Config.MaxConsecutiveAgentTurns {
		// Exception: if the agent was directly addressed, allow it regardless.
		// (Caller should check direct address separately and set a high score.)
		if score >= 90.0 {
			return true
		}
		return false
	}

	return true
}
