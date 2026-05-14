package polyphon

import (
	"strings"
)

// IntentType classifies the purpose of a human utterance.
type IntentType string

const (
	IntentGreeting           IntentType = "greeting"
	IntentFollowUp           IntentType = "follow_up"
	IntentDomainQuestion     IntentType = "domain_question"
	IntentCapabilityQuestion IntentType = "capability_question"
	IntentTaskRequest        IntentType = "task_request"
	IntentDirectAddress      IntentType = "direct_address"
	IntentAffirmation        IntentType = "affirmation"
	IntentFarewell           IntentType = "farewell"
)

// IntentResult holds the classification output.
type IntentResult struct {
	Primary    IntentType `json:"primary"`
	Secondary  IntentType `json:"secondary,omitempty"`
	Confidence float64    `json:"confidence"`
}

// ClassifyIntent determines the intent of an utterance using rule-based heuristics.
// Sub-10ms, no model call. Upgradeable to model-based later.
func ClassifyIntent(text string, mentions []Mention, prevEntry *TranscriptEntry) *IntentResult {
	if text == "" {
		return &IntentResult{Primary: IntentFollowUp, Confidence: 0.3}
	}

	lower := strings.ToLower(strings.TrimSpace(text))
	words := strings.Fields(lower)
	wordCount := len(words)

	// Direct address: @mention at start position
	for _, m := range mentions {
		if m.Role == MentionRoleAddressee {
			return &IntentResult{Primary: IntentDirectAddress, Confidence: 0.95}
		}
	}

	// Farewell patterns
	if isFarewell(lower) {
		return &IntentResult{Primary: IntentFarewell, Confidence: 0.9}
	}

	// Greeting patterns (reuses existing looksLikeGreeting logic)
	if looksLikeGreeting(lower) {
		return &IntentResult{Primary: IntentGreeting, Confidence: 0.9}
	}

	// Affirmation: very short positive/negative replies
	if wordCount <= 3 && isAffirmation(lower) {
		return &IntentResult{Primary: IntentAffirmation, Confidence: 0.85}
	}

	// Follow-up: short reply after agent message
	if wordCount <= 5 && prevEntry != nil && prevEntry.SpeakerType == "agent" {
		return &IntentResult{Primary: IntentFollowUp, Confidence: 0.8}
	}

	// Capability question: asking about what agents can do
	if isCapabilityQuestion(lower) {
		return &IntentResult{Primary: IntentCapabilityQuestion, Confidence: 0.85}
	}

	// Task request: action verbs
	if isTaskRequest(lower) {
		return &IntentResult{Primary: IntentTaskRequest, Confidence: 0.8}
	}

	// Domain question: question words present
	if looksLikeQuestion(lower) {
		return &IntentResult{Primary: IntentDomainQuestion, Confidence: 0.7}
	}

	// Short reply (6-10 words) after agent = follow-up
	if wordCount <= 10 && prevEntry != nil && prevEntry.SpeakerType == "agent" {
		return &IntentResult{Primary: IntentFollowUp, Confidence: 0.6}
	}

	// Default: domain question (most utterances are questions or statements)
	return &IntentResult{Primary: IntentDomainQuestion, Confidence: 0.4}
}

func isFarewell(lower string) bool {
	farewells := []string{"bye", "goodbye", "good bye", "see you", "thanks bye",
		"that's all", "thats all", "i'm done", "im done", "thank you bye",
		"gotta go", "have a good", "talk later", "catch you later"}
	for _, f := range farewells {
		if lower == f || strings.HasPrefix(lower, f+" ") || strings.HasPrefix(lower, f+",") {
			return true
		}
	}
	return false
}

func isAffirmation(lower string) bool {
	affirmations := []string{"yes", "yeah", "yep", "yup", "no", "nope", "nah",
		"ok", "okay", "sure", "exactly", "correct", "right", "agreed",
		"definitely", "absolutely", "of course", "not really", "i think so",
		"sounds good", "got it", "understood", "perfect", "great"}
	for _, a := range affirmations {
		if lower == a {
			return true
		}
	}
	return false
}

func isCapabilityQuestion(lower string) bool {
	patterns := []string{"who can help", "what can you do", "what agents",
		"who handles", "who is responsible", "who knows about",
		"which agent", "anyone here who", "is there someone",
		"can anyone help", "who should i ask", "who deals with"}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func isTaskRequest(lower string) bool {
	taskPrefixes := []string{"show me", "create ", "update ", "delete ", "run ",
		"execute ", "deploy ", "build ", "generate ", "send ",
		"upload ", "download ", "search for", "look up", "pull up",
		"set up", "configure ", "install ", "start ", "stop ",
		"schedule ", "export ", "import "}
	for _, p := range taskPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}
