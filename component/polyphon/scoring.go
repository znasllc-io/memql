package polyphon

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Scorer evaluates all agent candidates and returns scored results.
type Scorer struct {
	Weights ScoringWeights
	Policy  TurnPolicyConfig
}

// NewScorer creates a Scorer with the given weights and turn policy.
func NewScorer(weights ScoringWeights, policy TurnPolicyConfig) *Scorer {
	return &Scorer{Weights: weights, Policy: policy}
}

// ScoreAll evaluates every agent candidate against the utterance and session state.
// Returns a slice of AgentScore sorted by TotalScore descending.
func (s *Scorer) ScoreAll(candidates []AgentCandidate, utterance Utterance, session *PolyphonSession) []AgentScore {
	if len(candidates) == 0 {
		return nil
	}

	scores := make([]AgentScore, 0, len(candidates))
	for _, c := range candidates {
		score := s.scoreAgent(c, utterance, session)
		scores = append(scores, score)
	}

	// Apply contextual boosts (solo-agent, speakWhen="always" floor, personal assistant).
	s.applyContextualBoosts(scores, candidates, utterance.Text)

	// Clarifying question continuation: when the last agent asked a clarifying
	// question (message ends with "?") and the human replies, the agent that
	// asked should respond. This handles short answers like "in this space",
	// "yes", "the second one" that don't match any domain or question pattern.
	if session != nil {
		recent := session.RecentTranscript(2)
		if len(recent) >= 2 {
			prevEntry := recent[len(recent)-2] // Entry before the current human utterance
			isShortReply := len(strings.Fields(utterance.Text)) <= 10
			prevIsAgent := prevEntry.SpeakerType == "agent"
			prevAskedQuestion := strings.HasSuffix(strings.TrimSpace(prevEntry.Text), "?")

			if prevIsAgent && (prevAskedQuestion || isShortReply) {
				const continuationFloor = 35.0
				for i := range scores {
					if scores[i].AgentId == prevEntry.SpeakerId && scores[i].TotalScore < continuationFloor {
						boost := continuationFloor - scores[i].TotalScore
						scores[i].TotalScore = continuationFloor
						detail := fmt.Sprintf("Replying to agent's clarifying question (boosted by %.1f)", boost)
						if !prevAskedQuestion {
							detail = fmt.Sprintf("Short reply continuation to agent (boosted by %.1f)", boost)
						}
						scores[i].Factors = append(scores[i].Factors, ScoringFactor{
							Name:   "clarifying_question_continuation",
							Weight: boost,
							Value:  1.0,
							Score:  boost,
							Detail: detail,
						})
					}
				}
			}
		}
	}

	// Sort descending by TotalScore.
	sortScoresDescending(scores)
	return scores
}

// applyContextualBoosts adjusts scores based on conversation context.
// Three boosts are applied:
//   - Solo-agent boost: When only one agent is present (1:1 conversation), floor
//     at 40.0 so the agent reliably clears the response threshold.
//   - speakWhen="always" floor: If an agent has speakWhen="always" and scored below
//     the default response threshold (30.0), boost to reach 30.0.
//   - Personal assistant floor: The user's assistant (Role="assistant")
//     is boosted to 35.0 when the utterance is a question, task, or greeting AND no
//     domain specialist already clears the threshold. This ensures questions and
//     greetings always get answered without the assistant dominating over specialists.
func (s *Scorer) applyContextualBoosts(scores []AgentScore, candidates []AgentCandidate, utteranceText string) {
	soloAgent := len(candidates) == 1

	// Build a lookup for candidate config by agent ID.
	candidateMap := make(map[string]*AgentCandidate, len(candidates))
	for i := range candidates {
		candidateMap[candidates[i].ID] = &candidates[i]
	}

	for i := range scores {
		candidate := candidateMap[scores[i].AgentId]
		if candidate == nil {
			continue
		}

		// Solo-agent boost: in 1:1 conversations, the single agent must
		// reliably clear the response threshold (30.0). The base score can
		// drop as low as 7.5 when the agent just spoke (recency ≈ 0) and
		// the message has no domain keywords or question markers. A fixed
		// +20 boost only reaches 27.5 in that worst case, so we use a
		// floor-based boost that guarantees the agent reaches 40.0.
		// Guard: only apply when TotalScore > 0 so speakWhen="asked"
		// agents that were zeroed out remain silent.
		if soloAgent && scores[i].TotalScore > 0 {
			const soloFloor = 40.0
			if scores[i].TotalScore < soloFloor {
				boost := soloFloor - scores[i].TotalScore
				scores[i].TotalScore = soloFloor
				scores[i].Factors = append(scores[i].Factors, ScoringFactor{
					Name:   "solo_agent_boost",
					Weight: boost,
					Value:  1.0,
					Score:  boost,
					Detail: fmt.Sprintf("Solo agent floor (boosted by %.1f to %.1f)", boost, soloFloor),
				})
			}
		}

		// speakWhen="always" floor: agents configured to always speak should
		// meet the response threshold even for low-signal utterances.
		const alwaysFloor = 30.0
		if candidate.SpeakWhen == "always" && scores[i].TotalScore < alwaysFloor {
			boost := alwaysFloor - scores[i].TotalScore
			scores[i].TotalScore = alwaysFloor
			scores[i].Factors = append(scores[i].Factors, ScoringFactor{
				Name:   "speak_always_floor",
				Weight: boost,
				Value:  1.0,
				Score:  boost,
				Detail: fmt.Sprintf("speakWhen=always floor (boosted by %.1f)", boost),
			})
		}
	}

	// The personal-assistant floor used to boost assistant agents
	// to 35.0 on questions and greetings whenever no specialist already
	// cleared 30.0. That was decision-layer logic ("default to the general
	// assistant when nothing else fits") implemented as a silent +N score
	// boost, which hid the preference inside a number and caused the
	// assistant to dominate anytime specialists had mild keyword
	// matches. The cognitionRouting SI prompt now owns that fallback
	// decision and states it explicitly as a principle ("general
	// assistant is a last resort"); the score engine should not override
	// what the router sees.
}

// scoreAgent computes the total score for a single agent candidate.
func (s *Scorer) scoreAgent(agent AgentCandidate, utterance Utterance, session *PolyphonSession) AgentScore {
	factors := make([]ScoringFactor, 0, 6)

	// Factor 1: Direct address
	da := s.scoreDirectAddress(agent, utterance)
	factors = append(factors, da)

	// Factor 2: Domain relevance
	dr := s.scoreDomainRelevance(agent, utterance)
	factors = append(factors, dr)

	// Factor 3: Conversational thread (who was the user just talking to?)
	ct := s.scoreConversationalThread(agent, utterance, session, da.Value > 0)
	factors = append(factors, ct)

	// Factor 4: Conversation recency (anti-monopoly)
	cr := s.scoreConversationRecency(agent, session)
	factors = append(factors, cr)

	// Factor 5: Question detection
	qd := s.scoreQuestionDetection(agent, utterance)
	factors = append(factors, qd)

	// Factor 6: Continuation relevance
	cont := s.scoreContinuationRelevance(agent, session)
	factors = append(factors, cont)

	// Sum weighted scores.
	var total float64
	for _, f := range factors {
		total += f.Score
	}

	// Agents with speakWhen="asked" that aren't directly addressed get zeroed.
	if agent.SpeakWhen == "asked" && da.Value == 0 {
		total = 0
	}

	result := AgentScore{
		AgentId:    agent.ID,
		AgentName:  agent.Name,
		TotalScore: total,
		Factors:    factors,
	}

	// Build reason from highest contributing factor.
	if len(factors) > 0 {
		best := factors[0]
		for _, f := range factors[1:] {
			if f.Score > best.Score {
				best = f
			}
		}
		if best.Score > 0 {
			result.Reason = best.Detail
		}
	}

	return result
}

// scoreDirectAddress checks if the utterance explicitly mentions the agent by name.
// Distinguishes between primary address (name at start of utterance or with @prefix)
// and secondary mention (name appears elsewhere). Primary = 1.0, secondary = 0.4.
func (s *Scorer) scoreDirectAddress(agent AgentCandidate, utterance Utterance) ScoringFactor {
	value := 0.0
	detail := "Not addressed"

	// Fast path: use structured mentions if available.
	if len(utterance.Mentions) > 0 {
		for _, m := range utterance.Mentions {
			if m.ParticipantId == agent.ID || strings.EqualFold(m.Name, agent.Name) {
				if m.Role == MentionRoleAddressee {
					value = 1.0
					detail = "Directly addressed via @mention"
				} else {
					value = 0.4
					detail = "Referenced via @mention (not addressee)"
				}
				break
			}
		}
		return ScoringFactor{
			Name:   "direct_address",
			Weight: s.Weights.DirectAddress,
			Value:  value,
			Score:  s.Weights.DirectAddress * value,
			Detail: detail,
		}
	}

	// Fallback: token-based name matching (no structured mentions).
	textLower := strings.ToLower(utterance.Text)
	textWords := tokenizeWords(textLower)

	// Check if the agent's name appears in the utterance.
	nameLower := strings.ToLower(agent.Name)
	mentioned := false
	if nameLower != "" {
		for _, token := range strings.Fields(nameLower) {
			if token != "" && (textWords[token] || textWords["@"+token]) {
				mentioned = true
				break
			}
		}
	}

	if mentioned {
		// Determine if this is a primary address (name at start) or secondary mention.
		// Primary: "Aria, help me" / "@Aria what do you think" / "hey Aria"
		// Secondary: "can you say hi to Aria" / "ask Aria about it"
		trimmed := strings.TrimSpace(textLower)
		nameTokens := strings.Fields(nameLower)
		isPrimary := false

		for _, token := range nameTokens {
			// Name (or @name) appears as the first or second word
			if strings.HasPrefix(trimmed, token) || strings.HasPrefix(trimmed, "@"+token) {
				isPrimary = true
				break
			}
			// "hey/hi/hello {name}" pattern
			greetPrefixes := []string{"hey ", "hi ", "hello ", "dear "}
			for _, g := range greetPrefixes {
				if strings.HasPrefix(trimmed, g+token) {
					isPrimary = true
					break
				}
			}
			if isPrimary {
				break
			}
		}

		if isPrimary {
			value = 1.0
			detail = fmt.Sprintf("Directly addressed: '%s'", agent.Name)
		} else {
			value = 0.4
			detail = fmt.Sprintf("Mentioned (not addressed): '%s'", agent.Name)
		}
	}

	return ScoringFactor{
		Name:   "direct_address",
		Weight: s.Weights.DirectAddress,
		Value:  value,
		Score:  s.Weights.DirectAddress * value,
		Detail: detail,
	}
}

// scoreConversationalThread gives a boost to the agent the user was recently talking to.
// This creates natural conversational continuity -- follow-up messages without a name
// mention go to the same agent. The thread decays after 60 seconds of inactivity.
// anyoneAddressed indicates whether any agent was directly addressed in this utterance.
func (s *Scorer) scoreConversationalThread(agent AgentCandidate, utterance Utterance, session *PolyphonSession, anyoneAddressed bool) ScoringFactor {
	value := 0.0
	detail := "No active thread"

	if session == nil {
		return ScoringFactor{
			Name:   "conversational_thread",
			Weight: s.Weights.ConversationalThread,
			Value:  value,
			Score:  0,
			Detail: detail,
		}
	}

	addressedId, addressedAt := session.GetAddressedAgent()

	// Only apply thread boost when no one is explicitly addressed in this utterance.
	// If someone IS addressed, the thread will be updated after scoring.
	if addressedId != "" && !anyoneAddressed {
		// Determine thread timeout based on input modality.
		threadTimeout := s.Policy.TextThreadTimeout
		if threadTimeout == 0 {
			threadTimeout = 120 * time.Second
		}
		if utterance.IsVoice {
			threadTimeout = s.Policy.VoiceThreadTimeout
			if threadTimeout == 0 {
				threadTimeout = 60 * time.Second
			}
		}

		elapsed := utterance.Timestamp.Sub(addressedAt)
		if elapsed <= 0 || utterance.Timestamp.IsZero() {
			elapsed = time.Since(addressedAt)
		}

		if elapsed < threadTimeout {
			if agent.ID == addressedId {
				value = 1.0
				detail = "Active conversation thread"
			}
		} else {
			detail = fmt.Sprintf("Thread expired (>%ds)", int(threadTimeout.Seconds()))
		}
	}

	return ScoringFactor{
		Name:   "conversational_thread",
		Weight: s.Weights.ConversationalThread,
		Value:  value,
		Score:  s.Weights.ConversationalThread * value,
		Detail: detail,
	}
}

// scoreDomainRelevance checks how well the utterance topic matches the agent's domain.
// When both the utterance and agent have pre-computed embeddings, vector cosine
// similarity is used for a semantic match. Otherwise falls back to keyword matching.
func (s *Scorer) scoreDomainRelevance(agent AgentCandidate, utterance Utterance) ScoringFactor {
	// Vector similarity path: use embeddings when both are available.
	if len(utterance.Embedding) > 0 && len(agent.ProfileEmbedding) > 0 {
		similarity := cosineSimilarity(utterance.Embedding, agent.ProfileEmbedding)
		return ScoringFactor{
			Name:   "domain_relevance",
			Weight: s.Weights.DomainRelevance,
			Value:  similarity,
			Score:  s.Weights.DomainRelevance * similarity,
			Detail: fmt.Sprintf("Vector similarity: %.3f", similarity),
		}
	}

	// Fallback: keyword matching.
	return s.scoreDomainRelevanceKeywords(agent, utterance)
}

// scoreDomainRelevanceKeywords is the keyword-based domain relevance scorer.
// It matches utterance text against the agent's configured domains, keywords,
// and description terms.
func (s *Scorer) scoreDomainRelevanceKeywords(agent AgentCandidate, utterance Utterance) ScoringFactor {
	if len(agent.Domains) == 0 && len(agent.Keywords) == 0 && agent.Description == "" {
		return ScoringFactor{
			Name:   "domain_relevance",
			Weight: s.Weights.DomainRelevance,
			Value:  0.3, // Benefit of the doubt
			Score:  s.Weights.DomainRelevance * 0.3,
			Detail: "No domain configured",
		}
	}

	textLower := strings.ToLower(utterance.Text)
	textWords := tokenizeWords(textLower)

	// Pre-compute stemmed forms of utterance words for fuzzy matching.
	stemmedTextWords := make(map[string]bool, len(textWords))
	for w := range textWords {
		stemmedTextWords[stemWord(w)] = true
	}

	matches := 0
	totalTerms := 0

	// Check explicit keywords.
	for _, kw := range agent.Keywords {
		kwLower := strings.ToLower(kw)
		totalTerms++
		if strings.Contains(textLower, kwLower) {
			matches++
		} else if stemmedTextWords[stemWord(kwLower)] {
			matches++ // Stemmed match: "troubleshooting" matches "troubleshoot"
		}
	}

	// Check domain terms.
	for _, domain := range agent.Domains {
		domainLower := strings.ToLower(domain)
		totalTerms++
		// Check both exact word and substring.
		if textWords[domainLower] || strings.Contains(textLower, domainLower) {
			matches++
		} else if stemmedTextWords[stemWord(domainLower)] {
			matches++ // Stemmed match
		}
	}

	// Fallback to description keyword overlap (existing pattern).
	if totalTerms == 0 && agent.Description != "" {
		descWords := tokenizeWords(strings.ToLower(agent.Description))
		for w := range textWords {
			if len(w) < 4 {
				continue
			}
			totalTerms++
			for d := range descWords {
				if strings.Contains(d, w) || strings.Contains(w, d) {
					matches++
					break
				}
			}
		}
	}

	value := 0.0
	if totalTerms > 0 {
		value = float64(matches) / float64(totalTerms)
	}
	if value > 1.0 {
		value = 1.0
	}

	detail := fmt.Sprintf("%d/%d terms matched", matches, totalTerms)

	return ScoringFactor{
		Name:   "domain_relevance",
		Weight: s.Weights.DomainRelevance,
		Value:  value,
		Score:  s.Weights.DomainRelevance * value,
		Detail: detail,
	}
}

// cosineSimilarity computes the cosine similarity between two vectors.
// Returns 0.0 if vectors are empty or have different lengths.
// Result is clamped to [0, 1] for use as a scoring value.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	sim := dot / (math.Sqrt(normA) * math.Sqrt(normB))
	// Clamp to [0, 1] since cosine similarity can be negative for opposing vectors,
	// but scoring values must be non-negative.
	if sim < 0 {
		return 0
	}
	if sim > 1 {
		return 1
	}
	return sim
}

// scoreConversationRecency penalizes agents that spoke recently (anti-monopoly).
// An agent that hasn't spoken gets a high score; one that just spoke gets a low score.
func (s *Scorer) scoreConversationRecency(agent AgentCandidate, session *PolyphonSession) ScoringFactor {
	if session == nil {
		return ScoringFactor{
			Name:   "conversation_recency",
			Weight: s.Weights.ConversationRecency,
			Value:  1.0,
			Score:  s.Weights.ConversationRecency,
			Detail: "No session context",
		}
	}

	lastSpoke := session.LastAgentSpokeAt(agent.ID)
	if lastSpoke.IsZero() {
		return ScoringFactor{
			Name:   "conversation_recency",
			Weight: s.Weights.ConversationRecency,
			Value:  1.0,
			Score:  s.Weights.ConversationRecency,
			Detail: "Has not spoken yet",
		}
	}

	elapsed := time.Since(lastSpoke)

	// Agents that spoke within the last 10 seconds get penalized.
	// After 30 seconds, the penalty is fully decayed.
	const penaltyWindowSec = 30.0
	elapsedSec := elapsed.Seconds()

	value := elapsedSec / penaltyWindowSec
	if value > 1.0 {
		value = 1.0
	}

	detail := fmt.Sprintf("Last spoke %.0fs ago", elapsedSec)

	return ScoringFactor{
		Name:   "conversation_recency",
		Weight: s.Weights.ConversationRecency,
		Value:  value,
		Score:  s.Weights.ConversationRecency * value,
		Detail: detail,
	}
}

// scoreQuestionDetection checks if the utterance is a question that fits
// the agent's domain expertise.
func (s *Scorer) scoreQuestionDetection(agent AgentCandidate, utterance Utterance) ScoringFactor {
	isQuestion := looksLikeQuestion(utterance.Text)
	if !isQuestion {
		return ScoringFactor{
			Name:   "question_detection",
			Weight: s.Weights.QuestionDetection,
			Value:  0.0,
			Score:  0.0,
			Detail: "Not a question",
		}
	}

	// It's a question. Check if it relates to the agent's domain.
	value := 0.5 // Base value: it's a question but maybe not for this agent

	// Boost if question contains agent's domain keywords.
	textLower := strings.ToLower(utterance.Text)
	for _, kw := range agent.Keywords {
		if strings.Contains(textLower, strings.ToLower(kw)) {
			value = 1.0
			break
		}
	}
	for _, domain := range agent.Domains {
		if strings.Contains(textLower, strings.ToLower(domain)) {
			value = 1.0
			break
		}
	}

	return ScoringFactor{
		Name:   "question_detection",
		Weight: s.Weights.QuestionDetection,
		Value:  value,
		Score:  s.Weights.QuestionDetection * value,
		Detail: fmt.Sprintf("Question detected (relevance: %.0f%%)", value*100),
	}
}

// scoreContinuationRelevance checks if this agent has something to add
// after the last agent spoke. Only relevant when the last speaker was an agent.
func (s *Scorer) scoreContinuationRelevance(agent AgentCandidate, session *PolyphonSession) ScoringFactor {
	if session == nil || session.LastSpeakerType != "agent" || session.LastAgentId == agent.ID {
		return ScoringFactor{
			Name:   "continuation_relevance",
			Weight: s.Weights.ContinuationRelevance,
			Value:  0.0,
			Score:  0.0,
			Detail: "Not a continuation context",
		}
	}

	// Check recent transcript for topics this agent could add to.
	recent := session.RecentTranscript(3)
	if len(recent) == 0 {
		return ScoringFactor{
			Name:   "continuation_relevance",
			Weight: s.Weights.ContinuationRelevance,
			Value:  0.0,
			Score:  0.0,
			Detail: "No recent context",
		}
	}

	// Build combined text from recent entries.
	var recentText strings.Builder
	for _, e := range recent {
		recentText.WriteString(e.Text)
		recentText.WriteString(" ")
	}

	// Score based on keyword/domain overlap with recent discussion.
	recentLower := strings.ToLower(recentText.String())
	matches := 0
	total := len(agent.Domains) + len(agent.Keywords)
	if total == 0 {
		return ScoringFactor{
			Name:   "continuation_relevance",
			Weight: s.Weights.ContinuationRelevance,
			Value:  0.0,
			Score:  0.0,
			Detail: "No domain/keywords configured",
		}
	}

	for _, domain := range agent.Domains {
		if strings.Contains(recentLower, strings.ToLower(domain)) {
			matches++
		}
	}
	for _, kw := range agent.Keywords {
		if strings.Contains(recentLower, strings.ToLower(kw)) {
			matches++
		}
	}

	value := float64(matches) / float64(total)
	if value > 1.0 {
		value = 1.0
	}

	return ScoringFactor{
		Name:   "continuation_relevance",
		Weight: s.Weights.ContinuationRelevance,
		Value:  value,
		Score:  s.Weights.ContinuationRelevance * value,
		Detail: fmt.Sprintf("Continuation: %d/%d terms match recent discussion", matches, total),
	}
}

// tokenizeWords splits text into a set of lowercase word tokens.
// Keeps '@' prefix for @mentions. Shared word-boundary tokenizer for mention matching.
func tokenizeWords(text string) map[string]bool {
	words := make(map[string]bool)
	for _, w := range strings.FieldsFunc(text, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '@')
	}) {
		words[strings.ToLower(w)] = true
	}
	return words
}

// looksLikeQuestion returns true if the text appears to be a question or
// a task/request that warrants a response.
func looksLikeQuestion(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	if strings.HasSuffix(t, "?") {
		return true
	}
	l := strings.ToLower(t)
	questionStarts := []string{
		"who ", "what ", "when ", "where ", "why ", "how ",
		"can you ", "could you ", "would you ",
		"do we ", "did we ", "is ", "are ",
	}
	for _, p := range questionStarts {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	// Task and request patterns (imperative sentences that expect action).
	taskStarts := []string{
		"help ", "help me ", "tell me ", "show me ", "find ",
		"explain ", "describe ", "list ", "give me ", "get me ",
		"please ", "can we ", "let's ", "shall we ",
		"i need ", "i want ", "i'd like ",
		"send ", "create ", "update ", "delete ", "remove ",
		"check ", "look up ", "search ", "summarize ",
	}
	for _, p := range taskStarts {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// looksLikeGreeting returns true if the text is a conversational greeting
// that typically warrants acknowledgment.
func looksLikeGreeting(text string) bool {
	l := strings.ToLower(strings.TrimSpace(text))
	if l == "" {
		return false
	}
	greetings := []string{
		"hello", "hi", "hey", "good morning", "good afternoon",
		"good evening", "howdy", "greetings", "hola", "buenos dias",
		"buenas tardes", "buenas noches",
	}
	for _, g := range greetings {
		if l == g || strings.HasPrefix(l, g+" ") || strings.HasPrefix(l, g+",") ||
			strings.HasPrefix(l, g+"!") || strings.HasPrefix(l, g+".") {
			return true
		}
	}
	return false
}

// sortScoresDescending sorts agent scores by TotalScore in descending order.
func sortScoresDescending(scores []AgentScore) {
	// Simple insertion sort — at most 3 agents.
	for i := 1; i < len(scores); i++ {
		key := scores[i]
		j := i - 1
		for j >= 0 && scores[j].TotalScore < key.TotalScore {
			scores[j+1] = scores[j]
			j--
		}
		scores[j+1] = key
	}
}
