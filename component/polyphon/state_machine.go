package polyphon

import (
	"sync"
	"time"
)

// ConversationPhase represents the current phase of a group conversation.
type ConversationPhase string

const (
	PhaseGreeting    ConversationPhase = "greeting"
	PhaseExploration ConversationPhase = "exploration"
	PhaseTask        ConversationPhase = "task"
	PhaseFollowUp    ConversationPhase = "follow_up"
	PhaseWindingDown ConversationPhase = "winding_down"
)

// ConversationStateMachine tracks the conversation phase and handles transitions.
type ConversationStateMachine struct {
	mu             sync.RWMutex
	phase          ConversationPhase
	lastTransition time.Time
}

// NewConversationStateMachine creates a state machine starting in the greeting phase.
func NewConversationStateMachine() *ConversationStateMachine {
	return &ConversationStateMachine{
		phase:          PhaseGreeting,
		lastTransition: time.Now(),
	}
}

// Phase returns the current conversation phase.
func (sm *ConversationStateMachine) Phase() ConversationPhase {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.phase
}

// LastTransition returns when the last phase change occurred.
func (sm *ConversationStateMachine) LastTransition() time.Time {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.lastTransition
}

// Transition updates the conversation phase based on the detected intent.
// Returns the new phase after the transition.
func (sm *ConversationStateMachine) Transition(intent IntentType) ConversationPhase {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	prev := sm.phase
	next := sm.computeTransition(prev, intent)

	if next != prev {
		sm.phase = next
		sm.lastTransition = time.Now()
	}

	return sm.phase
}

func (sm *ConversationStateMachine) computeTransition(current ConversationPhase, intent IntentType) ConversationPhase {
	// Farewell always transitions to winding down.
	if intent == IntentFarewell {
		return PhaseWindingDown
	}

	// Greeting resets to greeting phase from any state.
	if intent == IntentGreeting {
		return PhaseGreeting
	}

	switch current {
	case PhaseGreeting:
		switch intent {
		case IntentDomainQuestion, IntentTaskRequest, IntentDirectAddress:
			return PhaseTask
		case IntentCapabilityQuestion:
			return PhaseExploration
		}

	case PhaseExploration:
		switch intent {
		case IntentDomainQuestion, IntentTaskRequest, IntentDirectAddress:
			return PhaseTask
		}

	case PhaseTask:
		switch intent {
		case IntentFollowUp, IntentAffirmation:
			return PhaseFollowUp
		case IntentCapabilityQuestion:
			return PhaseExploration
		}

	case PhaseFollowUp:
		switch intent {
		case IntentDomainQuestion, IntentTaskRequest, IntentDirectAddress:
			return PhaseTask
		case IntentCapabilityQuestion:
			return PhaseExploration
		}

	case PhaseWindingDown:
		switch intent {
		case IntentDomainQuestion, IntentTaskRequest, IntentDirectAddress:
			return PhaseTask
		case IntentCapabilityQuestion:
			return PhaseExploration
		}
	}

	// No transition: stay in current phase.
	return current
}
