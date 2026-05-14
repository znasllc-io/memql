package polyphon

import "testing"

func TestStateMachineTransitions(t *testing.T) {
	tests := []struct {
		name      string
		start     ConversationPhase
		intent    IntentType
		wantPhase ConversationPhase
	}{
		// From Greeting
		{"greeting + domain_question -> task", PhaseGreeting, IntentDomainQuestion, PhaseTask},
		{"greeting + task_request -> task", PhaseGreeting, IntentTaskRequest, PhaseTask},
		{"greeting + direct_address -> task", PhaseGreeting, IntentDirectAddress, PhaseTask},
		{"greeting + capability_question -> exploration", PhaseGreeting, IntentCapabilityQuestion, PhaseExploration},
		{"greeting + follow_up -> greeting (no transition)", PhaseGreeting, IntentFollowUp, PhaseGreeting},
		{"greeting + affirmation -> greeting (no transition)", PhaseGreeting, IntentAffirmation, PhaseGreeting},

		// From Exploration
		{"exploration + domain_question -> task", PhaseExploration, IntentDomainQuestion, PhaseTask},
		{"exploration + task_request -> task", PhaseExploration, IntentTaskRequest, PhaseTask},
		{"exploration + direct_address -> task", PhaseExploration, IntentDirectAddress, PhaseTask},
		{"exploration + follow_up -> exploration (no transition)", PhaseExploration, IntentFollowUp, PhaseExploration},
		{"exploration + affirmation -> exploration (no transition)", PhaseExploration, IntentAffirmation, PhaseExploration},

		// From Task
		{"task + follow_up -> follow_up", PhaseTask, IntentFollowUp, PhaseFollowUp},
		{"task + affirmation -> follow_up", PhaseTask, IntentAffirmation, PhaseFollowUp},
		{"task + capability_question -> exploration", PhaseTask, IntentCapabilityQuestion, PhaseExploration},
		{"task + domain_question -> task (no transition)", PhaseTask, IntentDomainQuestion, PhaseTask},
		{"task + direct_address -> task (no transition)", PhaseTask, IntentDirectAddress, PhaseTask},

		// From FollowUp
		{"follow_up + domain_question -> task", PhaseFollowUp, IntentDomainQuestion, PhaseTask},
		{"follow_up + task_request -> task", PhaseFollowUp, IntentTaskRequest, PhaseTask},
		{"follow_up + direct_address -> task", PhaseFollowUp, IntentDirectAddress, PhaseTask},
		{"follow_up + capability_question -> exploration", PhaseFollowUp, IntentCapabilityQuestion, PhaseExploration},
		{"follow_up + affirmation -> follow_up (no transition)", PhaseFollowUp, IntentAffirmation, PhaseFollowUp},

		// From WindingDown
		{"winding_down + domain_question -> task", PhaseWindingDown, IntentDomainQuestion, PhaseTask},
		{"winding_down + task_request -> task", PhaseWindingDown, IntentTaskRequest, PhaseTask},
		{"winding_down + direct_address -> task", PhaseWindingDown, IntentDirectAddress, PhaseTask},
		{"winding_down + capability_question -> exploration", PhaseWindingDown, IntentCapabilityQuestion, PhaseExploration},

		// Universal transitions
		{"any + farewell -> winding_down (from greeting)", PhaseGreeting, IntentFarewell, PhaseWindingDown},
		{"any + farewell -> winding_down (from task)", PhaseTask, IntentFarewell, PhaseWindingDown},
		{"any + farewell -> winding_down (from exploration)", PhaseExploration, IntentFarewell, PhaseWindingDown},
		{"any + greeting -> greeting (from task)", PhaseTask, IntentGreeting, PhaseGreeting},
		{"any + greeting -> greeting (from winding_down)", PhaseWindingDown, IntentGreeting, PhaseGreeting},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sm := NewConversationStateMachine()
			// Set to start phase by transitioning through known paths.
			sm.mu.Lock()
			sm.phase = tt.start
			sm.mu.Unlock()

			got := sm.Transition(tt.intent)
			if got != tt.wantPhase {
				t.Errorf("Transition(%s, %s): got %s, want %s", tt.start, tt.intent, got, tt.wantPhase)
			}
		})
	}
}

func TestStateMachineMultiTurnSequence(t *testing.T) {
	sm := NewConversationStateMachine()

	// Start in greeting.
	if sm.Phase() != PhaseGreeting {
		t.Fatalf("expected initial phase greeting, got %s", sm.Phase())
	}

	steps := []struct {
		intent    IntentType
		wantPhase ConversationPhase
	}{
		{IntentCapabilityQuestion, PhaseExploration},
		{IntentDomainQuestion, PhaseTask},
		{IntentAffirmation, PhaseFollowUp},
		{IntentTaskRequest, PhaseTask},
		{IntentFarewell, PhaseWindingDown},
		{IntentGreeting, PhaseGreeting}, // Reset
	}

	for i, step := range steps {
		got := sm.Transition(step.intent)
		if got != step.wantPhase {
			t.Errorf("step %d (%s): got %s, want %s", i, step.intent, got, step.wantPhase)
		}
	}
}

func TestStateMachineInitialPhase(t *testing.T) {
	sm := NewConversationStateMachine()
	if sm.Phase() != PhaseGreeting {
		t.Errorf("expected initial phase greeting, got %s", sm.Phase())
	}
	if sm.LastTransition().IsZero() {
		t.Error("expected non-zero LastTransition")
	}
}
