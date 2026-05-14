package polyphon

import "testing"

func TestClassifyIntent(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		mentions   []Mention
		prevEntry  *TranscriptEntry
		wantIntent IntentType
		wantMinConf float64
		wantMaxConf float64
	}{
		{
			name:        "empty text returns follow_up",
			text:        "",
			wantIntent:  IntentFollowUp,
			wantMinConf: 0.2, wantMaxConf: 0.4,
		},
		{
			name:     "addressee mention returns direct_address",
			text:     "Sofia, check this",
			mentions: []Mention{{Name: "Sofia", Role: MentionRoleAddressee}},
			wantIntent:  IntentDirectAddress,
			wantMinConf: 0.9, wantMaxConf: 1.0,
		},
		{
			name:        "farewell: bye",
			text:        "bye",
			wantIntent:  IntentFarewell,
			wantMinConf: 0.8, wantMaxConf: 1.0,
		},
		{
			name:        "farewell: see you later",
			text:        "see you later",
			wantIntent:  IntentFarewell,
			wantMinConf: 0.8, wantMaxConf: 1.0,
		},
		{
			name:        "farewell: that's all",
			text:        "that's all",
			wantIntent:  IntentFarewell,
			wantMinConf: 0.8, wantMaxConf: 1.0,
		},
		{
			name:        "greeting: hello",
			text:        "hello",
			wantIntent:  IntentGreeting,
			wantMinConf: 0.8, wantMaxConf: 1.0,
		},
		{
			name:        "greeting: good morning",
			text:        "good morning everyone",
			wantIntent:  IntentGreeting,
			wantMinConf: 0.8, wantMaxConf: 1.0,
		},
		{
			name:        "affirmation: yes",
			text:        "yes",
			wantIntent:  IntentAffirmation,
			wantMinConf: 0.8, wantMaxConf: 0.9,
		},
		{
			name:        "affirmation: sounds good",
			text:        "sounds good",
			wantIntent:  IntentAffirmation,
			wantMinConf: 0.8, wantMaxConf: 0.9,
		},
		{
			name:        "affirmation: got it",
			text:        "got it",
			wantIntent:  IntentAffirmation,
			wantMinConf: 0.8, wantMaxConf: 0.9,
		},
		{
			name:      "short follow-up after agent (<=5 words)",
			text:      "in this space",
			prevEntry: &TranscriptEntry{SpeakerType: "agent"},
			wantIntent:  IntentFollowUp,
			wantMinConf: 0.7, wantMaxConf: 0.9,
		},
		{
			name:      "medium follow-up after agent (6-10 words)",
			text:      "I meant the second option on the left",
			prevEntry: &TranscriptEntry{SpeakerType: "agent"},
			wantIntent:  IntentFollowUp,
			wantMinConf: 0.5, wantMaxConf: 0.7,
		},
		{
			name:        "capability question: who can help",
			text:        "who can help me with billing?",
			wantIntent:  IntentCapabilityQuestion,
			wantMinConf: 0.8, wantMaxConf: 0.9,
		},
		{
			name:        "capability question: which agent",
			text:        "which agent handles design?",
			wantIntent:  IntentCapabilityQuestion,
			wantMinConf: 0.8, wantMaxConf: 0.9,
		},
		{
			name:        "task request: show me",
			text:        "show me the quarterly report",
			wantIntent:  IntentTaskRequest,
			wantMinConf: 0.7, wantMaxConf: 0.9,
		},
		{
			name:        "task request: create",
			text:        "create a new ticket for the bug",
			wantIntent:  IntentTaskRequest,
			wantMinConf: 0.7, wantMaxConf: 0.9,
		},
		{
			name:        "task request: deploy",
			text:        "deploy the latest version to staging",
			wantIntent:  IntentTaskRequest,
			wantMinConf: 0.7, wantMaxConf: 0.9,
		},
		{
			name:        "domain question with question mark",
			text:        "how does the authentication flow work?",
			wantIntent:  IntentDomainQuestion,
			wantMinConf: 0.6, wantMaxConf: 0.8,
		},
		{
			name:        "domain question: what",
			text:        "what is the current deployment status",
			wantIntent:  IntentDomainQuestion,
			wantMinConf: 0.6, wantMaxConf: 0.8,
		},
		{
			name:        "default: long statement without patterns",
			text:        "the project has been going well and we should continue the current approach",
			wantIntent:  IntentDomainQuestion,
			wantMinConf: 0.3, wantMaxConf: 0.5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ClassifyIntent(tt.text, tt.mentions, tt.prevEntry)
			if result == nil {
				t.Fatal("expected non-nil IntentResult")
			}
			if result.Primary != tt.wantIntent {
				t.Errorf("intent: got %s, want %s", result.Primary, tt.wantIntent)
			}
			if result.Confidence < tt.wantMinConf || result.Confidence > tt.wantMaxConf {
				t.Errorf("confidence: got %.2f, want [%.2f, %.2f]", result.Confidence, tt.wantMinConf, tt.wantMaxConf)
			}
		})
	}
}

func TestIsFarewell(t *testing.T) {
	positives := []string{"bye", "goodbye", "see you later", "that's all", "gotta go", "i'm done"}
	for _, s := range positives {
		if !isFarewell(s) {
			t.Errorf("expected isFarewell(%q) = true", s)
		}
	}

	negatives := []string{"hello", "yes", "the bye product", ""}
	for _, s := range negatives {
		if isFarewell(s) {
			t.Errorf("expected isFarewell(%q) = false", s)
		}
	}
}

func TestIsAffirmation(t *testing.T) {
	positives := []string{"yes", "yeah", "ok", "sure", "exactly", "sounds good", "got it"}
	for _, s := range positives {
		if !isAffirmation(s) {
			t.Errorf("expected isAffirmation(%q) = true", s)
		}
	}

	negatives := []string{"yes please continue", "ok let me think", "hello"}
	for _, s := range negatives {
		if isAffirmation(s) {
			t.Errorf("expected isAffirmation(%q) = false", s)
		}
	}
}

func TestIsCapabilityQuestion(t *testing.T) {
	positives := []string{"who can help me with this?", "which agent handles design?", "can anyone help with billing"}
	for _, s := range positives {
		if !isCapabilityQuestion(s) {
			t.Errorf("expected isCapabilityQuestion(%q) = true", s)
		}
	}

	negatives := []string{"how does this work?", "deploy to staging"}
	for _, s := range negatives {
		if isCapabilityQuestion(s) {
			t.Errorf("expected isCapabilityQuestion(%q) = false", s)
		}
	}
}

func TestIsTaskRequest(t *testing.T) {
	positives := []string{"show me the data", "create a new ticket", "deploy the app", "search for the file"}
	for _, s := range positives {
		if !isTaskRequest(s) {
			t.Errorf("expected isTaskRequest(%q) = true", s)
		}
	}

	negatives := []string{"hello", "what is the status?", "yes"}
	for _, s := range negatives {
		if isTaskRequest(s) {
			t.Errorf("expected isTaskRequest(%q) = false", s)
		}
	}
}
