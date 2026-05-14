package polyphon

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// CompactedContext is an agent-scoped view of the conversation history.
// Instead of passing all messages to the AI, it provides 3 zones:
// Zone 1 (Summary): compressed old history
// Zone 2 (Recent): last N messages verbatim
// Zone 3 (Agent-Relevant): this agent's own exchanges
type CompactedContext struct {
	Summary        string            // Zone 1
	RecentMessages []TranscriptEntry // Zone 2
	AgentMessages  []TranscriptEntry // Zone 3
	TotalTokensEst int
}

// ContextBuilder constructs agent-scoped context views from session transcripts.
type ContextBuilder struct {
	recentWindowSize  int // Default: 8
	maxAgentExchanges int // Default: 10
}

// NewContextBuilder creates a context builder with defaults.
func NewContextBuilder() *ContextBuilder {
	return &ContextBuilder{
		recentWindowSize:  8,
		maxAgentExchanges: 10,
	}
}

// BuildForAgent constructs a compacted context for a specific agent.
func (cb *ContextBuilder) BuildForAgent(session *PolyphonSession, agentId string) *CompactedContext {
	if session == nil {
		return &CompactedContext{}
	}

	session.mu.RLock()
	transcript := make([]TranscriptEntry, len(session.Transcript))
	copy(transcript, session.Transcript)
	summary := session.CompactedSummary
	compactionIdx := session.CompactionIndex
	session.mu.RUnlock()

	if len(transcript) == 0 {
		return &CompactedContext{}
	}

	ctx := &CompactedContext{
		Summary: summary,
	}

	// Zone 2: Recent messages (last N entries, full text).
	recentStart := len(transcript) - cb.recentWindowSize
	if recentStart < 0 {
		recentStart = 0
	}
	ctx.RecentMessages = transcript[recentStart:]

	// Zone 3: Agent-relevant exchanges (this agent's messages + preceding human message).
	// Look through entries BEFORE the recent window to avoid duplicates.
	agentExchanges := make([]TranscriptEntry, 0, cb.maxAgentExchanges*2)
	for i := 0; i < recentStart; i++ {
		entry := transcript[i]
		if entry.SpeakerId == agentId {
			// Include the preceding human message if available.
			if i > 0 && transcript[i-1].SpeakerType == "human" {
				agentExchanges = append(agentExchanges, transcript[i-1])
			}
			agentExchanges = append(agentExchanges, entry)
		}
	}

	// Cap agent exchanges to most recent N.
	if len(agentExchanges) > cb.maxAgentExchanges*2 {
		agentExchanges = agentExchanges[len(agentExchanges)-cb.maxAgentExchanges*2:]
	}
	ctx.AgentMessages = agentExchanges

	// Estimate total tokens.
	ctx.TotalTokensEst = estimateContextTokens(ctx)

	// If no summary exists yet but there are old entries not covered, note it.
	if summary == "" && compactionIdx < recentStart {
		// Summary will be generated async by TriggerCompaction.
		// For now, include a brief note about uncovered history.
		oldCount := recentStart - compactionIdx
		if oldCount > 0 {
			ctx.Summary = buildQuickSummary(transcript[:recentStart])
		}
	}

	return ctx
}

// BuildForAgentSemantic constructs a context where Zone 3 uses semantic similarity
// instead of speaker-ID filtering. Falls back to BuildForAgent when embeddings are unavailable.
//
// The findRelevant function queries node_vectors for utterances similar to the combined
// agent profile + current utterance embedding.
func (cb *ContextBuilder) BuildForAgentSemantic(
	session *PolyphonSession,
	agentId string,
	utteranceEmbedding []float32,
	agentProfileEmbedding []float32,
	findRelevant func(combinedEmbedding []float32, limit int) ([]TranscriptEntry, error),
) *CompactedContext {
	// If embeddings aren't available, fall back to non-semantic version.
	if len(utteranceEmbedding) == 0 || findRelevant == nil {
		return cb.BuildForAgent(session, agentId)
	}

	if session == nil {
		return &CompactedContext{}
	}

	session.mu.RLock()
	transcript := make([]TranscriptEntry, len(session.Transcript))
	copy(transcript, session.Transcript)
	summary := session.CompactedSummary
	session.mu.RUnlock()

	ctx := &CompactedContext{Summary: summary}

	// Zone 2: Recent messages (same as non-semantic).
	recentStart := len(transcript) - cb.recentWindowSize
	if recentStart < 0 {
		recentStart = 0
	}
	ctx.RecentMessages = transcript[recentStart:]

	// Zone 3: Semantic retrieval instead of speaker-ID filtering.
	// Combine agent profile (what agent knows) + utterance (what's being discussed).
	combined := combineEmbeddings(agentProfileEmbedding, utteranceEmbedding, 0.3, 0.7)

	semanticEntries, err := findRelevant(combined, cb.maxAgentExchanges)
	if err != nil || len(semanticEntries) == 0 {
		// Fallback to speaker-ID filtering.
		ctx.AgentMessages = cb.getAgentExchangesById(transcript[:recentStart], agentId)
	} else {
		ctx.AgentMessages = semanticEntries
	}

	ctx.TotalTokensEst = estimateContextTokens(ctx)
	return ctx
}

// combineEmbeddings creates a weighted average of two embeddings.
func combineEmbeddings(a, b []float32, weightA, weightB float64) []float32 {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	if len(a) != len(b) {
		return b // Mismatched dimensions, use utterance only
	}

	result := make([]float32, len(a))
	for i := range a {
		result[i] = float32(float64(a[i])*weightA + float64(b[i])*weightB)
	}
	return result
}

// getAgentExchangesById extracts agent exchanges by speaker ID (existing fallback logic).
func (cb *ContextBuilder) getAgentExchangesById(transcript []TranscriptEntry, agentId string) []TranscriptEntry {
	exchanges := make([]TranscriptEntry, 0, cb.maxAgentExchanges*2)
	for i, entry := range transcript {
		if entry.SpeakerId == agentId {
			if i > 0 && transcript[i-1].SpeakerType == "human" {
				exchanges = append(exchanges, transcript[i-1])
			}
			exchanges = append(exchanges, entry)
		}
	}
	if len(exchanges) > cb.maxAgentExchanges*2 {
		exchanges = exchanges[len(exchanges)-cb.maxAgentExchanges*2:]
	}
	return exchanges
}

// buildQuickSummary creates a brief non-SI summary by listing speakers and topics.
// Used as a fallback before SI compaction runs.
func buildQuickSummary(entries []TranscriptEntry) string {
	if len(entries) == 0 {
		return ""
	}
	speakers := make(map[string]bool)
	for _, e := range entries {
		speakers[e.SpeakerName] = true
	}
	names := make([]string, 0, len(speakers))
	for name := range speakers {
		if name != "" {
			names = append(names, name)
		}
	}
	return "Previous conversation (" + strconv.Itoa(len(entries)) + " messages) between " + strings.Join(names, ", ") + "."
}

// estimateContextTokens provides a fast token estimate (characters / 4).
func estimateContextTokens(ctx *CompactedContext) int {
	total := len(ctx.Summary) / 4
	for _, m := range ctx.RecentMessages {
		total += (len(m.Text) + len(m.SpeakerName) + 10) / 4
	}
	for _, m := range ctx.AgentMessages {
		total += (len(m.Text) + len(m.SpeakerName) + 10) / 4
	}
	return total
}

// BuildForAgentWithBudget constructs a compacted context that fits within the
// given token budget. Uses 4-level compaction escalation.
func (cb *ContextBuilder) BuildForAgentWithBudget(session *PolyphonSession, agentId string, budget TokenBudget) *CompactedContext {
	if session == nil || budget.Available <= 0 {
		return &CompactedContext{}
	}

	session.mu.RLock()
	transcript := make([]TranscriptEntry, len(session.Transcript))
	copy(transcript, session.Transcript)
	summary := session.CompactedSummary
	session.mu.RUnlock()

	if len(transcript) == 0 {
		return &CompactedContext{Summary: summary}
	}

	// Start with normal compaction and escalate if needed.
	recentWindow := cb.recentWindowSize
	maxAgentExch := cb.maxAgentExchanges

	for level := CompactionNormal; level <= CompactionReset; level++ {
		switch level {
		case CompactionAggressive:
			recentWindow = 5
			maxAgentExch = 5
		case CompactionEmergency:
			recentWindow = 3
			maxAgentExch = 2
		case CompactionReset:
			// Emergency: single-sentence summary, minimal context.
			resetSummary := summary
			if resetSummary == "" {
				resetSummary = buildQuickSummary(transcript)
			}
			// Truncate summary to fit budget.
			maxChars := budget.Available * 4 // Reverse token estimate
			if len(resetSummary) > maxChars {
				resetSummary = resetSummary[:maxChars]
			}
			return &CompactedContext{
				Summary:        resetSummary,
				RecentMessages: transcript[len(transcript)-1:], // Just the latest
				TotalTokensEst: EstimateTokens(resetSummary) + 50,
			}
		}

		ctx := cb.buildWithLimits(transcript, summary, agentId, recentWindow, maxAgentExch)

		if ctx.TotalTokensEst <= budget.Available {
			return ctx
		}
		// Escalate to next level.
	}

	// Should not reach here, but return minimal context.
	return &CompactedContext{Summary: summary}
}

// buildWithLimits constructs a CompactedContext with specific window sizes.
func (cb *ContextBuilder) buildWithLimits(transcript []TranscriptEntry, summary string, agentId string, recentWindow, maxAgentExch int) *CompactedContext {
	ctx := &CompactedContext{Summary: summary}

	// Zone 2: Recent messages.
	recentStart := len(transcript) - recentWindow
	if recentStart < 0 {
		recentStart = 0
	}
	ctx.RecentMessages = transcript[recentStart:]

	// Zone 3: Agent-relevant exchanges before the recent window.
	agentExchanges := make([]TranscriptEntry, 0, maxAgentExch*2)
	for i := 0; i < recentStart; i++ {
		if transcript[i].SpeakerId == agentId {
			if i > 0 && transcript[i-1].SpeakerType == "human" {
				agentExchanges = append(agentExchanges, transcript[i-1])
			}
			agentExchanges = append(agentExchanges, transcript[i])
		}
	}
	if len(agentExchanges) > maxAgentExch*2 {
		agentExchanges = agentExchanges[len(agentExchanges)-maxAgentExch*2:]
	}
	ctx.AgentMessages = agentExchanges

	ctx.TotalTokensEst = estimateContextTokens(ctx)
	return ctx
}

// EntriesForCompaction returns transcript entries that have not been summarized yet
// and are outside the recent window.
func (cb *ContextBuilder) EntriesForCompaction(session *PolyphonSession) []TranscriptEntry {
	session.mu.RLock()
	defer session.mu.RUnlock()

	transcript := session.Transcript
	recentStart := len(transcript) - cb.recentWindowSize
	if recentStart < 0 {
		recentStart = 0
	}

	compIdx := session.CompactionIndex
	if compIdx >= recentStart {
		return nil // Everything is already summarized or in the recent window.
	}

	entries := make([]TranscriptEntry, recentStart-compIdx)
	copy(entries, transcript[compIdx:recentStart])
	return entries
}

// UpdateSummary stores a new compacted summary in the session.
func (cb *ContextBuilder) UpdateSummary(session *PolyphonSession, summary string, upToIndex int) {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.CompactedSummary = summary
	session.CompactionIndex = upToIndex
}

// TriggerCompaction runs AI summarization on old transcript entries.
// Debounced: skips if compaction ran within the last 30 seconds.
// Non-blocking: designed to be called from a fire-and-forget goroutine.
func (cb *ContextBuilder) TriggerCompaction(
	ctx context.Context,
	session *PolyphonSession,
	invokeAI func(ctx context.Context, templateId string, data map[string]any) (any, error),
) {
	if session == nil || invokeAI == nil {
		return
	}

	// Debounce: skip if last compaction was within 30s.
	session.mu.RLock()
	lastCompaction := session.LastCompactionAt
	session.mu.RUnlock()
	if time.Since(lastCompaction) < 30*time.Second {
		return
	}

	entries := cb.EntriesForCompaction(session)
	if len(entries) < 3 {
		return // Not enough old entries to justify summarization.
	}

	// Build input for the compaction prompt.
	transcriptData := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		transcriptData = append(transcriptData, map[string]any{
			"speakerName": e.SpeakerName,
			"speakerType": e.SpeakerType,
			"text":        e.Text,
		})
	}

	session.mu.RLock()
	prevSummary := session.CompactedSummary
	session.mu.RUnlock()

	data := map[string]any{
		"entries":         transcriptData,
		"previousSummary": prevSummary,
	}

	result, err := invokeAI(ctx, "cognitionCompaction", data)
	if err != nil {
		return // Silently fail; compaction is best-effort.
	}

	summary := ""
	switch v := result.(type) {
	case string:
		summary = v
	case map[string]any:
		if s, ok := v["summary"].(string); ok {
			summary = s
		} else {
			b, _ := json.Marshal(v)
			summary = string(b)
		}
	}

	if summary == "" {
		return
	}

	// Combine with previous summary if exists.
	if prevSummary != "" {
		summary = prevSummary + " " + summary
	}

	// Update session.
	session.mu.Lock()
	session.CompactedSummary = summary
	session.CompactionIndex = len(session.Transcript) - cb.recentWindowSize
	if session.CompactionIndex < 0 {
		session.CompactionIndex = 0
	}
	session.LastCompactionAt = time.Now()
	session.mu.Unlock()
}
