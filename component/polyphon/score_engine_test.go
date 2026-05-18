package polyphon

import (
	"context"
	"testing"
	"time"
)

var testCandidates = []AgentCandidate{
	{
		ID:        "agent-sofia",
		Name:      "Sofia",
		SpeakWhen: "relevant",
		Domains:   []string{"design", "ux", "collaboration"},
		Keywords:  []string{"layout", "interface", "user experience"},
	},
	{
		ID:        "agent-rex",
		Name:      "Rex",
		SpeakWhen: "relevant",
		Domains:   []string{"engineering", "architecture", "performance"},
		Keywords:  []string{"code", "refactor", "database", "api"},
	},
	{
		ID:        "agent-kai",
		Name:      "Kai",
		SpeakWhen: "relevant",
		Domains:   []string{"creative", "marketing", "content"},
		Keywords:  []string{"brand", "story", "campaign"},
	},
}

func TestScoreEngineProcessUtterance(t *testing.T) {
	scoreEngine := NewScoreEngine(nil)

	t.Run("directly addressed agent wins", func(t *testing.T) {
		utterance := Utterance{
			ID:            "utt-1",
			SpaceId:       "space-test-1",
			ParticipantId: "human-1",
			SpeakerName:   "Alice",
			Text:          "Hey Sofia, what do you think about this interface design?",
			IsFinal:       true,
			Timestamp:     time.Now().UTC(),
		}

		decision := scoreEngine.ProcessUtterance(context.Background(), utterance, testCandidates)

		if !decision.HasWinner() {
			t.Fatal("expected a winning agent")
		}
		if decision.Winner.AgentId != "agent-sofia" {
			t.Errorf("expected Sofia to win, got %s", decision.Winner.AgentName)
		}
		if decision.Action != "respond" {
			t.Errorf("expected action 'respond', got '%s'", decision.Action)
		}
	})

	t.Run("domain relevance selects best agent", func(t *testing.T) {
		utterance := Utterance{
			ID:            "utt-2",
			SpaceId:       "space-test-2",
			ParticipantId: "human-1",
			SpeakerName:   "Alice",
			Text:          "Can you help me refactor the database API and improve code performance?",
			IsFinal:       true,
			Timestamp:     time.Now().UTC(),
		}

		decision := scoreEngine.ProcessUtterance(context.Background(), utterance, testCandidates)

		if !decision.HasWinner() {
			t.Fatal("expected a winning agent")
		}
		if decision.Winner.AgentId != "agent-rex" {
			t.Errorf("expected Rex to win on engineering topic, got %s (score: %.1f)", decision.Winner.AgentName, decision.Winner.TotalScore)
		}
	})

	t.Run("silence when no agent matches", func(t *testing.T) {
		utterance := Utterance{
			ID:            "utt-3",
			SpaceId:       "space-test-3",
			ParticipantId: "human-1",
			SpeakerName:   "Alice",
			Text:          "ok",
			IsFinal:       true,
			Timestamp:     time.Now().UTC(),
		}

		decision := scoreEngine.ProcessUtterance(context.Background(), utterance, testCandidates)

		if decision.HasWinner() {
			t.Errorf("expected silence for generic utterance, got winner: %s (score: %.1f)",
				decision.Winner.AgentName, decision.Winner.TotalScore)
		}
		if decision.Action != "silence" {
			t.Errorf("expected action 'silence', got '%s'", decision.Action)
		}
	})

	t.Run("transcript is recorded", func(t *testing.T) {
		spaceId := "space-test-4"
		utterance := Utterance{
			ID:            "utt-4",
			SpaceId:       spaceId,
			ParticipantId: "human-1",
			SpeakerName:   "Alice",
			Text:          "Hello everyone",
			IsFinal:       true,
			Timestamp:     time.Now().UTC(),
		}

		scoreEngine.ProcessUtterance(context.Background(), utterance, testCandidates)

		session := scoreEngine.Sessions().Get(spaceId)
		if session == nil {
			t.Fatal("expected session to be created")
		}

		transcript := session.RecentTranscript(10)
		if len(transcript) == 0 {
			t.Fatal("expected transcript to have entries")
		}
		if transcript[0].Text != "Hello everyone" {
			t.Errorf("expected first transcript entry text 'Hello everyone', got '%s'", transcript[0].Text)
		}
		if transcript[0].SpeakerType != "human" {
			t.Errorf("expected speaker type 'human', got '%s'", transcript[0].SpeakerType)
		}
	})
}

func TestScoreEngineRecordAgentResponse(t *testing.T) {
	scoreEngine := NewScoreEngine(nil)
	spaceId := "space-cont-1"

	// First, process a human utterance.
	utterance := Utterance{
		ID:            "utt-1",
		SpaceId:       spaceId,
		ParticipantId: "human-1",
		SpeakerName:   "Alice",
		Text:          "Hey Sofia, can you explain the design approach for the interface?",
		IsFinal:       true,
		Timestamp:     time.Now().UTC(),
	}
	scoreEngine.ProcessUtterance(context.Background(), utterance, testCandidates)

	t.Run("agent response is recorded", func(t *testing.T) {
		shouldContinue, _ := scoreEngine.RecordAgentResponse(
			spaceId,
			"agent-sofia",
			"Sofia",
			"The interface design follows a card-based layout with responsive grid.",
			testCandidates,
		)

		session := scoreEngine.Sessions().Get(spaceId)
		if session == nil {
			t.Fatal("expected session to exist")
		}

		transcript := session.RecentTranscript(10)
		lastEntry := transcript[len(transcript)-1]
		if lastEntry.SpeakerType != "agent" {
			t.Errorf("expected last entry to be agent, got '%s'", lastEntry.SpeakerType)
		}
		if lastEntry.SpeakerId != "agent-sofia" {
			t.Errorf("expected speaker to be agent-sofia, got '%s'", lastEntry.SpeakerId)
		}

		// First continuation shouldn't be blocked (under max turns).
		_ = shouldContinue
	})

	t.Run("consecutive agent turns are tracked", func(t *testing.T) {
		session := scoreEngine.Sessions().Get(spaceId)
		if session == nil {
			t.Fatal("expected session to exist")
		}

		turns := session.AgentTurnsSinceHuman()
		if turns != 1 {
			t.Errorf("expected 1 consecutive agent turn, got %d", turns)
		}

		// Record another agent response.
		scoreEngine.RecordAgentResponse(
			spaceId,
			"agent-rex",
			"Rex",
			"From an engineering perspective, we should also consider the API structure.",
			testCandidates,
		)

		turns = session.AgentTurnsSinceHuman()
		if turns != 2 {
			t.Errorf("expected 2 consecutive agent turns, got %d", turns)
		}
	})

	t.Run("human utterance resets agent turn counter", func(t *testing.T) {
		humanUtterance := Utterance{
			ID:            "utt-2",
			SpaceId:       spaceId,
			ParticipantId: "human-1",
			SpeakerName:   "Alice",
			Text:          "Great points, both of you",
			IsFinal:       true,
			Timestamp:     time.Now().UTC(),
		}
		scoreEngine.ProcessUtterance(context.Background(), humanUtterance, testCandidates)

		session := scoreEngine.Sessions().Get(spaceId)
		turns := session.AgentTurnsSinceHuman()
		if turns != 0 {
			t.Errorf("expected 0 consecutive agent turns after human spoke, got %d", turns)
		}
	})
}

func TestTurnPolicyContinuationLimit(t *testing.T) {
	cfg := DefaultTurnPolicy()
	cfg.MaxConsecutiveAgentTurns = 2
	scoreEngine := NewScoreEngineWithConfig(DefaultScoringWeights(), cfg, nil)
	spaceId := "space-limit-1"

	// Process initial human utterance.
	utterance := Utterance{
		ID:            "utt-1",
		SpaceId:       spaceId,
		ParticipantId: "human-1",
		SpeakerName:   "Alice",
		Text:          "Hey Sofia, what about the design and code architecture?",
		IsFinal:       true,
		Timestamp:     time.Now().UTC(),
	}
	scoreEngine.ProcessUtterance(context.Background(), utterance, testCandidates)

	// Agent 1 responds.
	scoreEngine.RecordAgentResponse(spaceId, "agent-sofia", "Sofia", "The design uses a modular approach.", testCandidates)

	// Agent 2 responds.
	scoreEngine.RecordAgentResponse(spaceId, "agent-rex", "Rex", "The architecture supports that modular design.", testCandidates)

	// At this point, we've hit MaxConsecutiveAgentTurns=2.
	session := scoreEngine.Sessions().Get(spaceId)
	if session == nil {
		t.Fatal("expected session to exist")
	}

	turns := session.AgentTurnsSinceHuman()
	if turns != 2 {
		t.Errorf("expected 2 consecutive agent turns, got %d", turns)
	}

	// Policy should block further continuation.
	scores := []AgentScore{
		{AgentId: "agent-kai", TotalScore: 80.0, ShouldRespond: true},
	}
	shouldContinue, _ := scoreEngine.Policy().ShouldContinue(scores, session)
	if shouldContinue {
		t.Error("expected continuation to be blocked at max consecutive turns")
	}
}

func TestSessionManagement(t *testing.T) {
	scoreEngine := NewScoreEngine(nil)

	t.Run("add and remove humans", func(t *testing.T) {
		spaceId := "space-sm-1"
		err := scoreEngine.Sessions().AddHuman(spaceId, "human-1", "Alice")
		if err != nil {
			t.Fatalf("unexpected error adding human: %v", err)
		}

		err = scoreEngine.Sessions().AddHuman(spaceId, "human-2", "Bob")
		if err != nil {
			t.Fatalf("unexpected error adding human: %v", err)
		}

		if !scoreEngine.Sessions().HasHumans(spaceId) {
			t.Error("expected HasHumans to return true")
		}

		scoreEngine.Sessions().RemoveHuman(spaceId, "human-1")
		scoreEngine.Sessions().RemoveHuman(spaceId, "human-2")

		session := scoreEngine.Sessions().Get(spaceId)
		if len(session.Humans) != 0 {
			t.Errorf("expected 0 humans after removal, got %d", len(session.Humans))
		}
	})

	t.Run("add and remove agents", func(t *testing.T) {
		spaceId := "space-sm-2"
		err := scoreEngine.Sessions().AddAgent(spaceId, "agent-1", "part-1", "Sofia")
		if err != nil {
			t.Fatalf("unexpected error adding agent: %v", err)
		}

		err = scoreEngine.Sessions().AddAgent(spaceId, "agent-2", "part-2", "Rex")
		if err != nil {
			t.Fatalf("unexpected error adding agent: %v", err)
		}

		session := scoreEngine.Sessions().Get(spaceId)
		if len(session.Agents) != 2 {
			t.Errorf("expected 2 agents, got %d", len(session.Agents))
		}

		scoreEngine.Sessions().RemoveAgent(spaceId, "agent-1")
		session = scoreEngine.Sessions().Get(spaceId)
		if len(session.Agents) != 1 {
			t.Errorf("expected 1 agent after removal, got %d", len(session.Agents))
		}
	})

	t.Run("max humans limit", func(t *testing.T) {
		spaceId := "space-sm-3"
		for i := 0; i < MaxHumansPerSpace; i++ {
			err := scoreEngine.Sessions().AddHuman(spaceId, "human-"+string(rune('A'+i)), "Human "+string(rune('A'+i)))
			if err != nil {
				t.Fatalf("unexpected error adding human %d: %v", i, err)
			}
		}

		err := scoreEngine.Sessions().AddHuman(spaceId, "human-overflow", "Overflow")
		if err == nil {
			t.Error("expected error when exceeding max humans")
		}
	})

	t.Run("max agents limit", func(t *testing.T) {
		spaceId := "space-sm-4"
		for i := 0; i < MaxAgentsPerSpace; i++ {
			err := scoreEngine.Sessions().AddAgent(spaceId, "agent-"+string(rune('A'+i)), "part-"+string(rune('A'+i)), "Agent "+string(rune('A'+i)))
			if err != nil {
				t.Fatalf("unexpected error adding agent %d: %v", i, err)
			}
		}

		err := scoreEngine.Sessions().AddAgent(spaceId, "agent-overflow", "part-overflow", "Overflow")
		if err == nil {
			t.Error("expected error when exceeding max agents")
		}
	})

	t.Run("duplicate add is idempotent", func(t *testing.T) {
		spaceId := "space-sm-5"
		_ = scoreEngine.Sessions().AddHuman(spaceId, "human-1", "Alice")
		_ = scoreEngine.Sessions().AddHuman(spaceId, "human-1", "Alice")

		session := scoreEngine.Sessions().Get(spaceId)
		if len(session.Humans) != 1 {
			t.Errorf("expected 1 human after duplicate add, got %d", len(session.Humans))
		}
	})

	t.Run("session cleanup", func(t *testing.T) {
		spaceId := "space-sm-6"
		scoreEngine.Sessions().GetOrCreate(spaceId)

		if scoreEngine.Sessions().ActiveSessions() == 0 {
			t.Error("expected at least 1 active session")
		}

		removed := scoreEngine.Sessions().Remove(spaceId)
		if removed == nil {
			t.Error("expected removed session to be returned")
		}
		if scoreEngine.Sessions().Get(spaceId) != nil {
			t.Error("expected session to be nil after removal")
		}
	})
}

func TestTranscriptManagement(t *testing.T) {
	scoreEngine := NewScoreEngine(nil)
	spaceId := "space-transcript-1"

	t.Run("recent transcript returns correct entries", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			scoreEngine.Sessions().RecordUtterance(spaceId, TranscriptEntry{
				SpaceId:     spaceId,
				SpeakerId:   "human-1",
				SpeakerType: "human",
				Text:        "Message " + string(rune('0'+i)),
				Timestamp:   time.Now().UTC(),
			})
		}

		session := scoreEngine.Sessions().Get(spaceId)
		recent := session.RecentTranscript(3)
		if len(recent) != 3 {
			t.Errorf("expected 3 recent entries, got %d", len(recent))
		}
	})

	t.Run("agent turn count tracking", func(t *testing.T) {
		session := scoreEngine.Sessions().Get(spaceId)
		if session.AgentTurnsSinceHuman() != 0 {
			t.Errorf("expected 0 agent turns, got %d", session.AgentTurnsSinceHuman())
		}

		scoreEngine.Sessions().RecordUtterance(spaceId, TranscriptEntry{
			SpeakerId:   "agent-1",
			SpeakerType: "agent",
			Timestamp:   time.Now().UTC(),
		})
		if session.AgentTurnsSinceHuman() != 1 {
			t.Errorf("expected 1 agent turn, got %d", session.AgentTurnsSinceHuman())
		}

		scoreEngine.Sessions().RecordUtterance(spaceId, TranscriptEntry{
			SpeakerId:   "human-1",
			SpeakerType: "human",
			Timestamp:   time.Now().UTC(),
		})
		if session.AgentTurnsSinceHuman() != 0 {
			t.Errorf("expected 0 agent turns after human spoke, got %d", session.AgentTurnsSinceHuman())
		}
	})
}

func TestResponseDelay(t *testing.T) {
	policy := NewTurnPolicy(DefaultTurnPolicy())

	// Run multiple times to verify range.
	for i := 0; i < 20; i++ {
		delay := policy.ResponseDelay()
		if delay < 300*time.Millisecond || delay > 500*time.Millisecond {
			t.Errorf("delay %v outside expected range [300ms, 500ms]", delay)
		}
	}
}

func TestClarifyingQuestionContinuation(t *testing.T) {
	scorer := NewScorer(DefaultScoringWeights(), DefaultTurnPolicy())

	aria := AgentCandidate{ID: "agent-aria", Name: "Aria", Role: "assistant", SpeakWhen: "relevant"}
	lyra := AgentCandidate{ID: "agent-lyra", Name: "Lyra", Role: "customer_success", SpeakWhen: "relevant",
		Domains: []string{"support"}, Keywords: []string{"ticket"}}
	candidates := []AgentCandidate{aria, lyra}

	t.Run("short reply after agent question gets continuation boost", func(t *testing.T) {
		session := NewSession("space-cqc-1")
		// Agent Aria asked a question.
		session.AddTranscript(TranscriptEntry{
			SpeakerId:   "agent-aria",
			SpeakerName: "Aria",
			SpeakerType: "agent",
			Text:        "Do you mean in this space or across all spaces?",
			Timestamp:   time.Now().UTC(),
		})
		// Record human reply in transcript (cognition does this before ScoreAll).
		session.AddTranscript(TranscriptEntry{
			SpeakerId:   "human-1",
			SpeakerName: "Alice",
			SpeakerType: "human",
			Text:        "in this space",
			Timestamp:   time.Now().UTC(),
		})

		// Human gives a short reply.
		utterance := Utterance{Text: "in this space", SpaceId: "space-cqc-1", Timestamp: time.Now().UTC()}
		scores := scorer.ScoreAll(candidates, utterance, session)

		var ariaScore float64
		for _, s := range scores {
			if s.AgentId == "agent-aria" {
				ariaScore = s.TotalScore
				break
			}
		}
		if ariaScore < 30.0 {
			t.Errorf("expected Aria to clear threshold via continuation boost, got %.1f", ariaScore)
		}
	})
}

func TestShortReplyContinuation(t *testing.T) {
	scorer := NewScorer(DefaultScoringWeights(), DefaultTurnPolicy())

	aria := AgentCandidate{ID: "agent-aria", Name: "Aria", SpeakWhen: "relevant"}
	rex := AgentCandidate{ID: "agent-rex", Name: "Rex", SpeakWhen: "relevant"}
	candidates := []AgentCandidate{aria, rex}

	t.Run("yes after agent gets continuation", func(t *testing.T) {
		session := NewSession("space-src-1")
		session.AddTranscript(TranscriptEntry{
			SpeakerId:   "agent-aria",
			SpeakerName: "Aria",
			SpeakerType: "agent",
			Text:        "I can set that up for you. Should I proceed?",
			Timestamp:   time.Now().UTC(),
		})
		// Record human reply in transcript (cognition does this before ScoreAll).
		session.AddTranscript(TranscriptEntry{
			SpeakerId:   "human-1",
			SpeakerName: "Alice",
			SpeakerType: "human",
			Text:        "yes",
			Timestamp:   time.Now().UTC(),
		})

		utterance := Utterance{Text: "yes", SpaceId: "space-src-1", Timestamp: time.Now().UTC()}
		scores := scorer.ScoreAll(candidates, utterance, session)

		var ariaScore float64
		for _, s := range scores {
			if s.AgentId == "agent-aria" {
				ariaScore = s.TotalScore
				break
			}
		}
		if ariaScore < 30.0 {
			t.Errorf("expected Aria to clear threshold via continuation, got %.1f", ariaScore)
		}
	})
}

func TestClassifyConfidence_AmbiguousScores(t *testing.T) {
	t.Run("continuation with close scores returns low", func(t *testing.T) {
		// Two agents with continuation factor but close scores (spread < 15).
		// SI Router should decide in ambiguous cases.
		scores := []AgentScore{
			{
				AgentId: "a1", TotalScore: 38,
				Factors: []ScoringFactor{{Name: "clarifying_question_continuation", Value: 1.0, Score: 30}},
			},
			{AgentId: "a2", TotalScore: 30},
		}
		utt := Utterance{Intent: &IntentResult{Primary: IntentFollowUp, Confidence: 0.8}}
		got := classifyConfidence(scores, []AgentCandidate{{ID: "a1"}, {ID: "a2"}}, utt)
		if got != "low" {
			t.Errorf("expected low for close-race continuation (spread=%0.f < 15), got %s",
				scores[0].TotalScore-scores[1].TotalScore, got)
		}
	})

	t.Run("continuation with wide spread returns high", func(t *testing.T) {
		scores := []AgentScore{
			{
				AgentId: "a1", TotalScore: 55,
				Factors: []ScoringFactor{{Name: "clarifying_question_continuation", Value: 1.0, Score: 30}},
			},
			{AgentId: "a2", TotalScore: 20},
		}
		utt := Utterance{Intent: &IntentResult{Primary: IntentFollowUp, Confidence: 0.8}}
		got := classifyConfidence(scores, []AgentCandidate{{ID: "a1"}, {ID: "a2"}}, utt)
		if got != "high" {
			t.Errorf("expected high for wide spread, got %s", got)
		}
	})
}

func TestScoreEngine1to1SoloAgentResponds(t *testing.T) {
	scoreEngine := NewScoreEngine(nil)

	// Single agent — simulates a 1:1 voice conversation.
	soloCandidate := []AgentCandidate{
		{
			ID:        "agent-sofia",
			Name:      "Sofia",
			SpeakWhen: "relevant",
		},
	}

	t.Run("solo agent responds to unaddressed speech", func(t *testing.T) {
		utterance := Utterance{
			ID:            "utt-solo-1",
			SpaceId:       "space-solo-1",
			ParticipantId: "human-1",
			SpeakerName:   "Alice",
			Text:          "I think we should change the approach",
			IsFinal:       true,
			Timestamp:     time.Now().UTC(),
		}

		decision := scoreEngine.ProcessUtterance(context.Background(), utterance, soloCandidate)

		if !decision.HasWinner() {
			t.Fatalf("expected solo agent to respond, got silence (top score: %.1f)", decision.Scores[0].TotalScore)
		}
		if decision.Winner.AgentId != "agent-sofia" {
			t.Errorf("expected Sofia to win, got %s", decision.Winner.AgentName)
		}
		if decision.Action != "respond" {
			t.Errorf("expected action 'respond', got '%s'", decision.Action)
		}
	})

	t.Run("solo agent responds to question without direct address", func(t *testing.T) {
		utterance := Utterance{
			ID:            "utt-solo-2",
			SpaceId:       "space-solo-2",
			ParticipantId: "human-1",
			SpeakerName:   "Alice",
			Text:          "What do you think about this?",
			IsFinal:       true,
			Timestamp:     time.Now().UTC(),
		}

		decision := scoreEngine.ProcessUtterance(context.Background(), utterance, soloCandidate)

		if !decision.HasWinner() {
			t.Fatalf("expected solo agent to respond to question, got silence (top score: %.1f)", decision.Scores[0].TotalScore)
		}
	})
}
