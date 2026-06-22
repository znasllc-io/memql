package polyphon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// SessionManager manages Polyphon sessions across spaces.
// Sessions are kept in memory (not persisted to database) because they
// represent ephemeral real-time voice state. If memQL restarts, active
// sessions are recreated when participants rejoin.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*PolyphonSession // keyed by scopeId
	logger   *slog.Logger

	// maxTranscriptEntries limits the in-memory transcript per session.
	maxTranscriptEntries int

	// Heartbeat management: one goroutine per active space.
	heartbeatMu    sync.Mutex
	heartbeatStops map[string]chan struct{} // stop channels keyed by scopeId

	// Prediction management: one goroutine per active space.
	predictionMu    sync.Mutex
	predictionStops map[string]chan struct{} // stop channels keyed by scopeId
}

// NewSessionManager creates a new SessionManager.
func NewSessionManager(logger *slog.Logger) *SessionManager {
	return &SessionManager{
		sessions:             make(map[string]*PolyphonSession),
		logger:               logger,
		maxTranscriptEntries: 200,
		heartbeatStops:       make(map[string]chan struct{}),
		predictionStops:      make(map[string]chan struct{}),
	}
}

// GetOrCreate returns the session for the given space, creating one if needed.
func (m *SessionManager) GetOrCreate(scopeId string) *PolyphonSession {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sessions[scopeId]; ok {
		return s
	}

	s := NewSession(scopeId)
	m.sessions[scopeId] = s

	if m.logger != nil {
		m.logger.Debug("polyphon: session created", "scopeId", scopeId)
	}

	return s
}

// Get returns the session for the given space, or nil if not found.
func (m *SessionManager) Get(scopeId string) *PolyphonSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[scopeId]
}

// Remove removes and returns the session for the given space.
// Also stops the heartbeat goroutine if running.
func (m *SessionManager) Remove(scopeId string) *PolyphonSession {
	// Stop heartbeat and prediction first (separate locks).
	m.StopHeartbeat(scopeId)
	m.StopPrediction(scopeId)

	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[scopeId]
	if !ok {
		return nil
	}
	delete(m.sessions, scopeId)

	if m.logger != nil {
		m.logger.Debug("polyphon: session removed",
			"scopeId", scopeId,
			"transcriptEntries", len(s.Transcript),
			"duration", time.Since(s.CreatedAt).String(),
		)
	}

	return s
}

// AddHuman registers a human participant in the session.
func (m *SessionManager) AddHuman(scopeId string, participantId, displayName string) error {
	session := m.GetOrCreate(scopeId)

	session.mu.Lock()
	defer session.mu.Unlock()

	// Check limit.
	if len(session.Humans) >= MaxHumansPerSpace {
		return fmt.Errorf("maximum humans (%d) reached for space %s", MaxHumansPerSpace, scopeId)
	}

	// Check for duplicates.
	for _, h := range session.Humans {
		if h.ParticipantId == participantId {
			return nil // Already present
		}
	}

	session.Humans = append(session.Humans, SessionParticipant{
		ParticipantId: participantId,
		DisplayName:   displayName,
		JoinedAt:      time.Now().UTC(),
	})

	return nil
}

// RemoveHuman removes a human participant from the session.
func (m *SessionManager) RemoveHuman(scopeId, participantId string) {
	session := m.Get(scopeId)
	if session == nil {
		return
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	for i, h := range session.Humans {
		if h.ParticipantId == participantId {
			session.Humans = append(session.Humans[:i], session.Humans[i+1:]...)
			break
		}
	}

	// If no humans left, the session should be cleaned up by the caller.
}

// AddAgent registers an AI agent in the session.
func (m *SessionManager) AddAgent(scopeId string, agentId, participantId, name string) error {
	session := m.GetOrCreate(scopeId)

	session.mu.Lock()
	defer session.mu.Unlock()

	// Check limit.
	if len(session.Agents) >= MaxAgentsPerSpace {
		return fmt.Errorf("maximum agents (%d) reached for space %s", MaxAgentsPerSpace, scopeId)
	}

	// Check for duplicates.
	for _, a := range session.Agents {
		if a.AgentId == agentId {
			return nil // Already present
		}
	}

	session.Agents = append(session.Agents, SessionAgent{
		AgentId:       agentId,
		ParticipantId: participantId,
		Name:          name,
		JoinedAt:      time.Now().UTC(),
	})

	return nil
}

// RemoveAgent removes an AI agent from the session.
func (m *SessionManager) RemoveAgent(scopeId, agentId string) {
	session := m.Get(scopeId)
	if session == nil {
		return
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	for i, a := range session.Agents {
		if a.AgentId == agentId {
			session.Agents = append(session.Agents[:i], session.Agents[i+1:]...)
			break
		}
	}
}

// RecordUtterance adds a transcript entry to the session and trims old entries
// if the transcript exceeds the maximum length.
func (m *SessionManager) RecordUtterance(scopeId string, entry TranscriptEntry) {
	session := m.GetOrCreate(scopeId)
	session.AddTranscript(entry)

	// Trim transcript if it exceeds max length.
	session.mu.Lock()
	if len(session.Transcript) > m.maxTranscriptEntries {
		excess := len(session.Transcript) - m.maxTranscriptEntries
		session.Transcript = session.Transcript[excess:]
	}
	session.mu.Unlock()
}

// ActiveSessions returns the number of active Polyphon sessions.
func (m *SessionManager) ActiveSessions() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// StartHeartbeat begins a background goroutine that ticks every interval and
// evaluates whether the score engine should take proactive action. The evaluator
// checks silence duration, pending events, and predictive state. The
// actionHandler callback is invoked when the evaluator decides to act.
//
// Calling StartHeartbeat on a space that already has a heartbeat is a no-op.
// Call StopHeartbeat to stop the goroutine.
func (m *SessionManager) StartHeartbeat(
	scopeId string,
	interval time.Duration,
	evaluator HeartbeatEvaluator,
	candidates []AgentCandidate,
	actionHandler func(HeartbeatAction),
) {
	m.heartbeatMu.Lock()
	defer m.heartbeatMu.Unlock()

	// Already running for this space.
	if _, exists := m.heartbeatStops[scopeId]; exists {
		return
	}

	stop := make(chan struct{})
	m.heartbeatStops[scopeId] = stop

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		if m.logger != nil {
			m.logger.Debug("polyphon: heartbeat started", "scopeId", scopeId, "interval", interval.String())
		}

		for {
			select {
			case <-stop:
				if m.logger != nil {
					m.logger.Debug("polyphon: heartbeat stopped", "scopeId", scopeId)
				}
				return
			case <-ticker.C:
				session := m.Get(scopeId)
				if session == nil {
					// Session removed -- stop heartbeat.
					return
				}

				action := evaluator.Evaluate(session, candidates)
				if action != nil && action.Type != "idle" {
					if m.logger != nil {
						m.logger.Debug("polyphon: heartbeat action",
							"scopeId", scopeId,
							"actionType", action.Type,
							"agentName", action.AgentName,
							"reason", action.Reason,
						)
					}
					actionHandler(*action)
				}
			}
		}
	}()
}

// StopHeartbeat stops the heartbeat goroutine for the given space.
func (m *SessionManager) StopHeartbeat(scopeId string) {
	m.heartbeatMu.Lock()
	defer m.heartbeatMu.Unlock()

	if stop, exists := m.heartbeatStops[scopeId]; exists {
		close(stop)
		delete(m.heartbeatStops, scopeId)
	}
}

// HasHeartbeat returns true if a heartbeat is running for the given space.
func (m *SessionManager) HasHeartbeat(scopeId string) bool {
	m.heartbeatMu.Lock()
	defer m.heartbeatMu.Unlock()
	_, exists := m.heartbeatStops[scopeId]
	return exists
}

// StartPrediction launches a background goroutine that runs the predictive analyzer
// at the given interval for the specified space. Only runs when conversation is active.
func (m *SessionManager) StartPrediction(scopeId string, interval time.Duration, analyzer PredictiveAnalyzer, candidates []AgentCandidate) {
	m.predictionMu.Lock()
	defer m.predictionMu.Unlock()

	if _, exists := m.predictionStops[scopeId]; exists {
		return // Already running
	}

	stop := make(chan struct{})
	m.predictionStops[scopeId] = stop

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		if m.logger != nil {
			m.logger.Debug("polyphon: prediction started", "scopeId", scopeId, "interval", interval.String())
		}

		for {
			select {
			case <-ticker.C:
				session := m.Get(scopeId)
				if session == nil {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				state, err := analyzer.Analyze(ctx, session, candidates)
				cancel()
				if err != nil {
					if m.logger != nil {
						m.logger.Warn("prediction analysis failed", "scopeId", scopeId, "error", err)
					}
					continue
				}
				if state != nil {
					session.SetPrediction(state)
				}
			case <-stop:
				if m.logger != nil {
					m.logger.Debug("polyphon: prediction stopped", "scopeId", scopeId)
				}
				return
			}
		}
	}()
}

// StopPrediction stops the prediction goroutine for a space.
func (m *SessionManager) StopPrediction(scopeId string) {
	m.predictionMu.Lock()
	defer m.predictionMu.Unlock()

	if stop, exists := m.predictionStops[scopeId]; exists {
		close(stop)
		delete(m.predictionStops, scopeId)
	}
}

// HasPrediction returns true if a prediction goroutine is running for the given space.
func (m *SessionManager) HasPrediction(scopeId string) bool {
	m.predictionMu.Lock()
	defer m.predictionMu.Unlock()
	_, exists := m.predictionStops[scopeId]
	return exists
}

// HasHumans returns true if the session for the given space has at least one human.
func (m *SessionManager) HasHumans(scopeId string) bool {
	session := m.Get(scopeId)
	if session == nil {
		return false
	}

	session.mu.RLock()
	defer session.mu.RUnlock()
	return len(session.Humans) > 0
}
