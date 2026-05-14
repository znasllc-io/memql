package polyphon

import (
	"testing"
	"time"
)

func TestScoreDirectAddress(t *testing.T) {
	scorer := NewScorer(DefaultScoringWeights(), DefaultTurnPolicy())

	tests := []struct {
		name      string
		agentName string
		text      string
		wantValue float64 // Expected: 1.0 (primary), 0.4 (secondary mention), 0.0 (none)
	}{
		{
			name:      "primary address: name at start with greeting",
			agentName: "Sofia",
			text:      "Hey Sofia, what do you think?",
			wantValue: 1.0,
		},
		{
			name:      "secondary mention: @mention in middle",
			agentName: "Sofia",
			text:      "What about this @sofia?",
			wantValue: 0.4,
		},
		{
			name:      "primary address: @mention at start",
			agentName: "Sofia",
			text:      "@sofia what do you think?",
			wantValue: 1.0,
		},
		{
			name:      "primary address: name at start",
			agentName: "Rex",
			text:      "REX can you help?",
			wantValue: 1.0,
		},
		{
			name:      "secondary mention: name in middle",
			agentName: "Rex",
			text:      "can you say hi to Rex?",
			wantValue: 0.4,
		},
		{
			name:      "no mention",
			agentName: "Sofia",
			text:      "I think we should change the design",
			wantValue: 0.0,
		},
		{
			name:      "partial name no match",
			agentName: "Sofia",
			text:      "The sofa is comfortable",
			wantValue: 0.0,
		},
		{
			name:      "primary address: multi-word name with greeting",
			agentName: "Sofia AI",
			text:      "Hey Sofia, check this",
			wantValue: 1.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := AgentCandidate{Name: tt.agentName}
			utterance := Utterance{Text: tt.text}
			factor := scorer.scoreDirectAddress(agent, utterance)

			const tolerance = 0.05
			if factor.Value < tt.wantValue-tolerance || factor.Value > tt.wantValue+tolerance {
				t.Errorf("expected value %.2f, got %.2f (detail: %s)", tt.wantValue, factor.Value, factor.Detail)
			}
		})
	}
}

func TestScoreDomainRelevance(t *testing.T) {
	scorer := NewScorer(DefaultScoringWeights(), DefaultTurnPolicy())

	tests := []struct {
		name     string
		agent    AgentCandidate
		text     string
		wantHigh bool
	}{
		{
			name: "keyword match",
			agent: AgentCandidate{
				Keywords: []string{"design", "layout", "ux"},
			},
			text:     "Let's change the design of this layout",
			wantHigh: true,
		},
		{
			name: "domain match",
			agent: AgentCandidate{
				Domains: []string{"engineering", "architecture"},
			},
			text:     "We need to refactor the architecture",
			wantHigh: true,
		},
		{
			name: "no match",
			agent: AgentCandidate{
				Domains:  []string{"finance", "accounting"},
				Keywords: []string{"budget", "revenue"},
			},
			text:     "Let's change the design of the homepage",
			wantHigh: false,
		},
		{
			name: "no domains or keywords",
			agent: AgentCandidate{
				Description: "A helpful assistant for general questions",
			},
			text:     "What time is the meeting?",
			wantHigh: false,
		},
		{
			name: "description fallback match",
			agent: AgentCandidate{
				Description: "Expert in database design and query optimization",
			},
			text:     "How do we optimize this database query?",
			wantHigh: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			utterance := Utterance{Text: tt.text}
			factor := scorer.scoreDomainRelevance(tt.agent, utterance)

			if tt.wantHigh && factor.Value < 0.3 {
				t.Errorf("expected domain relevance > 0.3, got %.2f", factor.Value)
			}
			if !tt.wantHigh && factor.Value > 0.5 {
				t.Errorf("expected domain relevance <= 0.5, got %.2f", factor.Value)
			}
		})
	}
}

func TestScoreConversationRecency(t *testing.T) {
	scorer := NewScorer(DefaultScoringWeights(), DefaultTurnPolicy())

	t.Run("agent has not spoken", func(t *testing.T) {
		session := NewSession("space-1")
		agent := AgentCandidate{ID: "agent-1", Name: "Sofia"}
		factor := scorer.scoreConversationRecency(agent, session)

		if factor.Value != 1.0 {
			t.Errorf("expected value 1.0 for agent that hasn't spoken, got %.2f", factor.Value)
		}
	})

	t.Run("agent spoke recently penalized", func(t *testing.T) {
		session := NewSession("space-1")
		// Add a recent agent utterance.
		session.AddTranscript(TranscriptEntry{
			SpeakerId:   "agent-1",
			SpeakerType: "agent",
			Timestamp:   time.Now().Add(-2 * time.Second),
		})

		agent := AgentCandidate{ID: "agent-1", Name: "Sofia"}
		factor := scorer.scoreConversationRecency(agent, session)

		if factor.Value > 0.2 {
			t.Errorf("expected low recency value for agent that just spoke, got %.2f", factor.Value)
		}
	})

	t.Run("nil session defaults high", func(t *testing.T) {
		agent := AgentCandidate{ID: "agent-1", Name: "Sofia"}
		factor := scorer.scoreConversationRecency(agent, nil)

		if factor.Value != 1.0 {
			t.Errorf("expected value 1.0 for nil session, got %.2f", factor.Value)
		}
	})
}

func TestScoreQuestionDetection(t *testing.T) {
	scorer := NewScorer(DefaultScoringWeights(), DefaultTurnPolicy())

	tests := []struct {
		name     string
		text     string
		keywords []string
		wantHigh bool
	}{
		{
			name:     "question mark with domain match",
			text:     "How do we improve the design?",
			keywords: []string{"design", "layout"},
			wantHigh: true,
		},
		{
			name:     "question without domain match",
			text:     "What time is lunch?",
			keywords: []string{"engineering", "code"},
			wantHigh: true, // Base question detection = 0.5 (question detected but no domain match)
		},
		{
			name:     "not a question",
			text:     "I agree with that completely",
			keywords: []string{"topic"},
			wantHigh: false,
		},
		{
			name:     "starts with question word",
			text:     "How should we approach this problem",
			keywords: []string{"problem"},
			wantHigh: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := AgentCandidate{Keywords: tt.keywords}
			utterance := Utterance{Text: tt.text}
			factor := scorer.scoreQuestionDetection(agent, utterance)

			if tt.wantHigh && factor.Value < 0.5 {
				t.Errorf("expected question detection value >= 0.5, got %.2f", factor.Value)
			}
			if !tt.wantHigh && factor.Value > 0.1 {
				t.Errorf("expected question detection value <= 0.1, got %.2f", factor.Value)
			}
		})
	}
}

func TestScoreAll(t *testing.T) {
	scorer := NewScorer(DefaultScoringWeights(), DefaultTurnPolicy())

	candidates := []AgentCandidate{
		{
			ID:       "agent-sofia",
			Name:     "Sofia",
			Domains:  []string{"design", "ux", "collaboration"},
			Keywords: []string{"layout", "interface", "user experience"},
		},
		{
			ID:       "agent-rex",
			Name:     "Rex",
			Domains:  []string{"engineering", "architecture", "performance"},
			Keywords: []string{"code", "refactor", "database"},
		},
		{
			ID:       "agent-kai",
			Name:     "Kai",
			Domains:  []string{"creative", "marketing", "content"},
			Keywords: []string{"brand", "story", "campaign"},
		},
	}

	t.Run("directly addressed agent wins", func(t *testing.T) {
		utterance := Utterance{
			ID:      "utt-1",
			SpaceId: "space-1",
			Text:    "Hey Sofia, what do you think about this design?",
		}

		session := NewSession("space-1")
		scores := scorer.ScoreAll(candidates, utterance, session)

		if len(scores) != 3 {
			t.Fatalf("expected 3 scores, got %d", len(scores))
		}

		// Sofia should be first (highest score).
		if scores[0].AgentId != "agent-sofia" {
			t.Errorf("expected Sofia to win, got %s (score: %.1f)", scores[0].AgentName, scores[0].TotalScore)
		}
	})

	t.Run("domain relevance wins when no direct address", func(t *testing.T) {
		utterance := Utterance{
			ID:      "utt-2",
			SpaceId: "space-1",
			Text:    "We need to refactor the database and improve code performance",
		}

		session := NewSession("space-1")
		scores := scorer.ScoreAll(candidates, utterance, session)

		// Rex should score highest due to engineering/code/database keywords.
		if scores[0].AgentId != "agent-rex" {
			t.Errorf("expected Rex to win on domain relevance, got %s (score: %.1f)", scores[0].AgentName, scores[0].TotalScore)
		}
	})

	t.Run("scores sorted descending", func(t *testing.T) {
		utterance := Utterance{
			ID:      "utt-3",
			SpaceId: "space-1",
			Text:    "Hello everyone",
		}

		session := NewSession("space-1")
		scores := scorer.ScoreAll(candidates, utterance, session)

		for i := 1; i < len(scores); i++ {
			if scores[i].TotalScore > scores[i-1].TotalScore {
				t.Errorf("scores not sorted descending: [%d]=%f > [%d]=%f",
					i, scores[i].TotalScore, i-1, scores[i-1].TotalScore)
			}
		}
	})
}

func TestSpeakWhenAskedAgent(t *testing.T) {
	scorer := NewScorer(DefaultScoringWeights(), DefaultTurnPolicy())

	agent := AgentCandidate{
		ID:        "agent-1",
		Name:      "Sofia",
		SpeakWhen: "asked",
		Domains:   []string{"design"},
		Keywords:  []string{"layout"},
	}

	t.Run("not addressed gets zero", func(t *testing.T) {
		utterance := Utterance{Text: "Let's change the design layout"}
		scores := scorer.ScoreAll([]AgentCandidate{agent}, utterance, nil)

		if scores[0].TotalScore > 0 {
			t.Errorf("expected 0 score for 'asked' agent not addressed, got %.1f", scores[0].TotalScore)
		}
	})

	t.Run("addressed gets score", func(t *testing.T) {
		utterance := Utterance{Text: "Hey Sofia, what about the design?"}
		scores := scorer.ScoreAll([]AgentCandidate{agent}, utterance, nil)

		if scores[0].TotalScore == 0 {
			t.Error("expected non-zero score for 'asked' agent when addressed")
		}
	})
}

func TestTokenizeWords(t *testing.T) {
	words := tokenizeWords("hey sofia, what's up? @rex check this")

	expected := []string{"hey", "sofia", "what", "s", "up", "@rex", "check", "this"}
	for _, e := range expected {
		if !words[e] {
			t.Errorf("expected word '%s' in tokenized output", e)
		}
	}
}

func TestLooksLikeQuestion(t *testing.T) {
	questions := []string{
		"What do you think?",
		"How should we approach this",
		"Can you help me with this?",
		"Is this the right approach?",
		"Who is responsible for this?",
	}

	notQuestions := []string{
		"I agree with that",
		"Sounds good",
		"",
	}

	for _, q := range questions {
		if !looksLikeQuestion(q) {
			t.Errorf("expected '%s' to be detected as question", q)
		}
	}

	for _, nq := range notQuestions {
		if looksLikeQuestion(nq) {
			t.Errorf("expected '%s' to NOT be detected as question", nq)
		}
	}
}

func TestSoloAgentBoost(t *testing.T) {
	scorer := NewScorer(DefaultScoringWeights(), DefaultTurnPolicy())

	soloAgent := AgentCandidate{
		ID:        "agent-sofia",
		Name:      "Sofia",
		SpeakWhen: "relevant",
	}

	t.Run("solo agent scores above threshold", func(t *testing.T) {
		utterance := Utterance{Text: "I think we should change the approach"}
		scores := scorer.ScoreAll([]AgentCandidate{soloAgent}, utterance, nil)

		if len(scores) != 1 {
			t.Fatalf("expected 1 score, got %d", len(scores))
		}
		// Without solo boost, a generic utterance with no domains/keywords
		// and no direct address scores ~27.5. With the +20 boost it should
		// clear the 30.0 response threshold.
		if scores[0].TotalScore < 30.0 {
			t.Errorf("expected solo agent score >= 30.0, got %.1f", scores[0].TotalScore)
		}

		// Verify the boost factor is recorded.
		found := false
		for _, f := range scores[0].Factors {
			if f.Name == "solo_agent_boost" {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected solo_agent_boost factor in scoring factors")
		}
	})

	t.Run("multi-agent gets no solo boost", func(t *testing.T) {
		multiCandidates := []AgentCandidate{
			{ID: "agent-1", Name: "Sofia", SpeakWhen: "relevant"},
			{ID: "agent-2", Name: "Rex", SpeakWhen: "relevant"},
			{ID: "agent-3", Name: "Kai", SpeakWhen: "relevant"},
		}
		utterance := Utterance{Text: "I think we should change the approach"}
		scores := scorer.ScoreAll(multiCandidates, utterance, nil)

		for _, s := range scores {
			for _, f := range s.Factors {
				if f.Name == "solo_agent_boost" {
					t.Errorf("unexpected solo_agent_boost for agent %s in multi-agent scenario", s.AgentName)
				}
			}
		}
	})

	t.Run("solo boost does not override speakWhen=asked zeroing", func(t *testing.T) {
		askedAgent := AgentCandidate{
			ID:        "agent-sofia",
			Name:      "Sofia",
			SpeakWhen: "asked",
			Domains:   []string{"design"},
			Keywords:  []string{"layout"},
		}
		// Not addressed by name -> score gets zeroed for speakWhen="asked".
		utterance := Utterance{Text: "Let's change the design layout"}
		scores := scorer.ScoreAll([]AgentCandidate{askedAgent}, utterance, nil)

		if scores[0].TotalScore > 0 {
			t.Errorf("expected 0 score for speakWhen=asked agent not addressed (solo), got %.1f", scores[0].TotalScore)
		}
	})
}

func TestSpeakWhenAlwaysFloor(t *testing.T) {
	scorer := NewScorer(DefaultScoringWeights(), DefaultTurnPolicy())

	alwaysAgent := AgentCandidate{
		ID:        "agent-sofia",
		Name:      "Sofia",
		SpeakWhen: "always",
	}

	t.Run("always agent meets threshold on low-signal utterance", func(t *testing.T) {
		utterance := Utterance{Text: "ok"}
		scores := scorer.ScoreAll([]AgentCandidate{alwaysAgent}, utterance, nil)

		if scores[0].TotalScore < 30.0 {
			t.Errorf("expected speakWhen=always agent to meet 30.0 threshold, got %.1f", scores[0].TotalScore)
		}
	})

	t.Run("always agent floor recorded as factor", func(t *testing.T) {
		utterance := Utterance{Text: "ok"}
		// Use multiple candidates so solo boost doesn't apply.
		candidates := []AgentCandidate{
			alwaysAgent,
			{ID: "agent-rex", Name: "Rex", SpeakWhen: "relevant"},
		}
		scores := scorer.ScoreAll(candidates, utterance, nil)

		var alwaysScore *AgentScore
		for i := range scores {
			if scores[i].AgentId == "agent-sofia" {
				alwaysScore = &scores[i]
				break
			}
		}
		if alwaysScore == nil {
			t.Fatal("could not find always agent in scores")
		}
		if alwaysScore.TotalScore < 30.0 {
			t.Errorf("expected speakWhen=always score >= 30.0, got %.1f", alwaysScore.TotalScore)
		}

		found := false
		for _, f := range alwaysScore.Factors {
			if f.Name == "speak_always_floor" {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected speak_always_floor factor in scoring factors")
		}
	})
}

// TestGeneralAssistantNoSilentFloor pins the design choice that the
// heuristic scorer does NOT boost general_assistant agents with a silent
// +N floor. Before, a "personal_assistant_floor" factor forced the general
// assistant above the response threshold on questions and greetings
// whenever no specialist cleared it, which caused the general assistant
// to dominate in multi-agent spaces even when specialists were a mild
// match. That decision ("fall back to general assistant when nothing
// else fits") is now owned by the cognitionRouting SI prompt, which can
// reason about it explicitly with full context, not a silent number.
func TestGeneralAssistantNoSilentFloor(t *testing.T) {
	scorer := NewScorer(DefaultScoringWeights(), DefaultTurnPolicy())

	paAgent := AgentCandidate{
		ID:        "agent-pa",
		Name:      "Aria",
		Role:      "general_assistant",
		SpeakWhen: "relevant",
	}
	financeAgent := AgentCandidate{
		ID:        "agent-fin",
		Name:      "Rex",
		Role:      "accounting_finance",
		SpeakWhen: "relevant",
		Domains:   []string{"finance", "accounting", "budget"},
		Keywords:  []string{"budget", "revenue", "expense", "profit"},
	}

	t.Run("no personal_assistant_floor factor is ever emitted", func(t *testing.T) {
		candidates := []AgentCandidate{paAgent, financeAgent}
		utterances := []string{"hello", "what time is the meeting?", "help me find the latest report", "ok"}
		for _, text := range utterances {
			scores := scorer.ScoreAll(candidates, Utterance{Text: text}, nil)
			for _, s := range scores {
				for _, f := range s.Factors {
					if f.Name == "personal_assistant_floor" {
						t.Errorf("personal_assistant_floor should no longer exist; seen for agent %q on %q", s.AgentId, text)
					}
				}
			}
		}
	})

	t.Run("specialist still outscores general assistant when domain matches", func(t *testing.T) {
		candidates := []AgentCandidate{paAgent, financeAgent}
		utterance := Utterance{Text: "Rex, help me with the budget and revenue expense profit forecast"}
		scores := scorer.ScoreAll(candidates, utterance, nil)

		var paScore, finScore *AgentScore
		for i := range scores {
			switch scores[i].AgentId {
			case "agent-pa":
				paScore = &scores[i]
			case "agent-fin":
				finScore = &scores[i]
			}
		}
		if finScore == nil || paScore == nil {
			t.Fatal("missing agent scores")
		}
		if finScore.TotalScore <= paScore.TotalScore {
			t.Errorf("expected finance agent (%.1f) to outscore PA (%.1f)", finScore.TotalScore, paScore.TotalScore)
		}
	})

	t.Run("general assistant scores low on pure acknowledgments", func(t *testing.T) {
		candidates := []AgentCandidate{paAgent, financeAgent}
		scores := scorer.ScoreAll(candidates, Utterance{Text: "ok"}, nil)

		var paScore *AgentScore
		for i := range scores {
			if scores[i].AgentId == "agent-pa" {
				paScore = &scores[i]
				break
			}
		}
		if paScore == nil {
			t.Fatal("could not find PA agent in scores")
		}
		if paScore.TotalScore >= 30.0 {
			t.Errorf("expected PA below threshold for 'ok', got %.1f", paScore.TotalScore)
		}
	})
}

// TestConversationalThread_WhenEnabled verifies the thread-continuity
// factor's computation when callers explicitly enable it via a non-zero
// weight. The shipped DefaultScoringWeights zeroes this factor because
// the cognitionRouting SI prompt reasons about previous-responder
// continuity directly from the transcript; an implicit +N boost on the
// heuristic side caused runaway agent momentum in production. These
// tests preserve coverage of the factor's detection logic for any
// deployment that chooses to re-enable it with custom weights.
func TestConversationalThread(t *testing.T) {
	weights := DefaultScoringWeights()
	weights.ConversationalThread = 15.0 // explicit override: factor disabled by default
	scorer := NewScorer(weights, DefaultTurnPolicy())

	aria := AgentCandidate{ID: "agent-aria", Name: "Aria", Role: "general_assistant", SpeakWhen: "relevant"}
	lyra := AgentCandidate{ID: "agent-lyra", Name: "Lyra", Role: "customer_success", SpeakWhen: "relevant",
		Domains: []string{"support", "onboarding"}, Keywords: []string{"customer", "ticket"}}

	t.Run("thread continuity: follow-up goes to addressed agent", func(t *testing.T) {
		session := NewSession("space-thread-1")
		candidates := []AgentCandidate{aria, lyra}

		// Step 1: User addresses Aria -> sets thread
		utt1 := Utterance{Text: "Aria, hello!", Timestamp: time.Now().UTC(), SpaceId: "space-thread-1"}
		scores1 := scorer.ScoreAll(candidates, utt1, session)
		// Simulate cognition updating thread
		for _, s := range scores1 {
			for _, f := range s.Factors {
				if f.Name == "direct_address" && f.Value >= 1.0 {
					session.SetAddressedAgent(s.AgentId)
				}
			}
		}

		// Step 2: User asks follow-up without naming anyone
		utt2 := Utterance{Text: "what do you think about the project?", Timestamp: time.Now().UTC(), SpaceId: "space-thread-1"}
		scores2 := scorer.ScoreAll(candidates, utt2, session)

		var ariaScore, lyraScore float64
		for _, s := range scores2 {
			if s.AgentId == "agent-aria" {
				ariaScore = s.TotalScore
			}
			if s.AgentId == "agent-lyra" {
				lyraScore = s.TotalScore
			}
		}
		if ariaScore <= lyraScore {
			t.Errorf("expected Aria (%.1f) to outscore Lyra (%.1f) due to thread continuity", ariaScore, lyraScore)
		}
	})

	t.Run("thread switch: new primary address switches thread", func(t *testing.T) {
		session := NewSession("space-thread-2")
		session.SetAddressedAgent("agent-aria")

		candidates := []AgentCandidate{aria, lyra}
		// User now addresses Lyra by name at start
		utt := Utterance{Text: "Lyra, can you check the support tickets?", Timestamp: time.Now().UTC(), SpaceId: "space-thread-2"}
		scores := scorer.ScoreAll(candidates, utt, session)

		var lyraScore float64
		for _, s := range scores {
			if s.AgentId == "agent-lyra" {
				lyraScore = s.TotalScore
			}
		}
		// Lyra should win: primary DirectAddress (30) + domain match + question detection
		if lyraScore < 30.0 {
			t.Errorf("expected Lyra to clear threshold when directly addressed, got %.1f", lyraScore)
		}
	})

	t.Run("mention does not steal thread", func(t *testing.T) {
		session := NewSession("space-thread-3")
		session.SetAddressedAgent("agent-aria")

		candidates := []AgentCandidate{aria, lyra}
		// User asks Aria to say hi to Lyra -- Lyra is mentioned, not addressed
		utt := Utterance{Text: "can you say hi to Lyra?", Timestamp: time.Now().UTC(), SpaceId: "space-thread-3"}
		scores := scorer.ScoreAll(candidates, utt, session)

		var ariaScore, lyraScore float64
		for _, s := range scores {
			if s.AgentId == "agent-aria" {
				ariaScore = s.TotalScore
			}
			if s.AgentId == "agent-lyra" {
				lyraScore = s.TotalScore
			}
		}
		// Aria should outscore Lyra: thread boost (20) vs Lyra's secondary mention (12)
		if ariaScore <= lyraScore {
			t.Errorf("expected Aria (%.1f) to outscore Lyra (%.1f) -- mention should not steal thread", ariaScore, lyraScore)
		}
	})
}

func TestLooksLikeGreeting(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"hello", true},
		{"Hello!", true},
		{"hi there", true},
		{"hey, how are you?", true},
		{"good morning everyone", true},
		{"hola", true},
		{"ok", false},
		{"thanks", false},
		{"the hello world program", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			if got := looksLikeGreeting(tt.text); got != tt.want {
				t.Errorf("looksLikeGreeting(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestLooksLikeQuestionExpanded(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		// Original question patterns
		{"what time is it?", true},
		{"how do we proceed", true},
		{"can you help", true},
		// New task patterns
		{"help me with this", true},
		{"tell me about the project", true},
		{"show me the results", true},
		{"find the latest data", true},
		{"explain how this works", true},
		{"please send the report", true},
		{"i need the financial data", true},
		{"summarize the meeting", true},
		// Should NOT match
		{"ok", false},
		{"thanks", false},
		{"hello", false},
		{"sounds good", false},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			if got := looksLikeQuestion(tt.text); got != tt.want {
				t.Errorf("looksLikeQuestion(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestClassifyConfidence(t *testing.T) {
	t.Run("high on direct address with intent", func(t *testing.T) {
		scores := []AgentScore{{
			AgentId: "a1", TotalScore: 40,
			Factors: []ScoringFactor{{Name: "direct_address", Value: 1.0, Score: 25}},
		}}
		utt := Utterance{Intent: &IntentResult{Primary: IntentDirectAddress, Confidence: 0.95}}
		got := classifyConfidence(scores, []AgentCandidate{{ID: "a1"}, {ID: "a2"}}, utt)
		if got != "high" {
			t.Errorf("expected high, got %s", got)
		}
	})

	t.Run("high on solo agent", func(t *testing.T) {
		scores := []AgentScore{{AgentId: "a1", TotalScore: 20}}
		got := classifyConfidence(scores, []AgentCandidate{{ID: "a1"}}, Utterance{})
		if got != "high" {
			t.Errorf("expected high, got %s", got)
		}
	})

	t.Run("high on clarifying question", func(t *testing.T) {
		scores := []AgentScore{{
			AgentId: "a1", TotalScore: 35,
			Factors: []ScoringFactor{{Name: "clarifying_question_continuation", Value: 1.0, Score: 30}},
		}}
		got := classifyConfidence(scores, []AgentCandidate{{ID: "a1"}, {ID: "a2"}}, Utterance{})
		if got != "high" {
			t.Errorf("expected high, got %s", got)
		}
	})

	t.Run("high on follow-up with thread", func(t *testing.T) {
		scores := []AgentScore{{
			AgentId: "a1", TotalScore: 35,
			Factors: []ScoringFactor{{Name: "conversational_thread", Value: 1.0, Score: 15}},
		}}
		utt := Utterance{Intent: &IntentResult{Primary: IntentFollowUp, Confidence: 0.8}}
		got := classifyConfidence(scores, []AgentCandidate{{ID: "a1"}, {ID: "a2"}}, utt)
		if got != "high" {
			t.Errorf("expected high, got %s", got)
		}
	})

	t.Run("low on strong winner without direct address (SI router decides)", func(t *testing.T) {
		scores := []AgentScore{
			{AgentId: "a1", TotalScore: 60},
			{AgentId: "a2", TotalScore: 20},
		}
		got := classifyConfidence(scores, []AgentCandidate{{ID: "a1"}, {ID: "a2"}}, Utterance{})
		if got != "low" {
			t.Errorf("expected low (SI router decides), got %s", got)
		}
	})

	t.Run("low on ambiguous scores", func(t *testing.T) {
		scores := []AgentScore{
			{AgentId: "a1", TotalScore: 25},
			{AgentId: "a2", TotalScore: 20},
		}
		got := classifyConfidence(scores, []AgentCandidate{{ID: "a1"}, {ID: "a2"}}, Utterance{})
		if got != "low" {
			t.Errorf("expected low, got %s", got)
		}
	})

	t.Run("low on empty scores", func(t *testing.T) {
		got := classifyConfidence(nil, nil, Utterance{})
		if got != "low" {
			t.Errorf("expected low, got %s", got)
		}
	})
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want float64
		tol  float64
	}{
		{"identical vectors", []float32{1, 0, 0}, []float32{1, 0, 0}, 1.0, 0.01},
		{"orthogonal vectors", []float32{1, 0, 0}, []float32{0, 1, 0}, 0.0, 0.01},
		{"opposing vectors clamped to 0", []float32{1, 0}, []float32{-1, 0}, 0.0, 0.01},
		{"empty vectors", []float32{}, []float32{}, 0.0, 0.01},
		{"different lengths", []float32{1, 0}, []float32{1, 0, 0}, 0.0, 0.01},
		{"zero vector", []float32{0, 0}, []float32{1, 0}, 0.0, 0.01},
		{"similar vectors", []float32{1, 1, 0}, []float32{1, 0, 0}, 0.707, 0.02},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cosineSimilarity(tt.a, tt.b)
			diff := got - tt.want
			if diff < 0 {
				diff = -diff
			}
			if diff > tt.tol {
				t.Errorf("cosineSimilarity: got %.4f, want %.4f (tol %.4f)", got, tt.want, tt.tol)
			}
		})
	}
}

func TestScoreDomainRelevance_VectorPath(t *testing.T) {
	scorer := NewScorer(DefaultScoringWeights(), DefaultTurnPolicy())

	// Agent and utterance both have embeddings -> vector path should be used.
	agent := AgentCandidate{
		Name:             "Sofia",
		Domains:          []string{"design"},
		ProfileEmbedding: []float32{1, 0, 0},
	}
	utterance := Utterance{
		Text:      "help with design",
		Embedding: []float32{0.9, 0.1, 0},
	}

	factor := scorer.scoreDomainRelevance(agent, utterance)

	// Should use vector similarity, not keyword matching.
	if factor.Value < 0.9 {
		t.Errorf("expected high similarity (>0.9), got %.3f", factor.Value)
	}
	if factor.Detail == "" || factor.Detail[:6] != "Vector" {
		t.Errorf("expected vector similarity detail, got: %s", factor.Detail)
	}
}

func TestScoreDomainRelevance_KeywordFallback(t *testing.T) {
	scorer := NewScorer(DefaultScoringWeights(), DefaultTurnPolicy())

	// No embeddings -> should fall back to keyword matching.
	agent := AgentCandidate{
		Name:    "Rex",
		Domains: []string{"engineering", "code"},
	}
	utterance := Utterance{
		Text: "help me with the code refactoring",
	}

	factor := scorer.scoreDomainRelevance(agent, utterance)

	// Should use keyword matching and find "code".
	if factor.Value < 0.3 {
		t.Errorf("expected keyword match, got value %.3f", factor.Value)
	}
	if factor.Detail == "" {
		t.Error("expected non-empty detail")
	}
}

func TestHeartbeatEvaluator(t *testing.T) {
	evaluator := NewDefaultHeartbeatEvaluator()

	aria := AgentCandidate{ID: "agent-aria", Name: "Aria", Role: "general_assistant"}
	lyra := AgentCandidate{ID: "agent-lyra", Name: "Lyra", Role: "customer_success"}
	candidates := []AgentCandidate{aria, lyra}

	t.Run("idle when recent activity", func(t *testing.T) {
		session := NewSession("space-hb-1")
		session.LastActivityAt = time.Now().UTC() // Just active
		action := evaluator.Evaluate(session, candidates)
		if action != nil {
			t.Errorf("expected nil (idle), got %+v", action)
		}
	})

	t.Run("re-engage after silence", func(t *testing.T) {
		session := NewSession("space-hb-2")
		session.LastActivityAt = time.Now().UTC().Add(-5 * time.Minute) // 5 min silence
		action := evaluator.Evaluate(session, candidates)
		if action == nil {
			t.Fatal("expected re-engage action, got nil")
		}
		if action.Type != "re-engage" {
			t.Errorf("expected type re-engage, got %s", action.Type)
		}
		if action.AgentName != "Aria" {
			t.Errorf("expected general assistant Aria, got %s", action.AgentName)
		}
	})

	t.Run("notify on pending events", func(t *testing.T) {
		session := NewSession("space-hb-3")
		session.PendingEvents = []map[string]any{{"type": "data_update"}}
		action := evaluator.Evaluate(session, candidates)
		if action == nil {
			t.Fatal("expected notify action, got nil")
		}
		if action.Type != "notify" {
			t.Errorf("expected type notify, got %s", action.Type)
		}
	})
}
