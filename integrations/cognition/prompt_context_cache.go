package cognition

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	defaultParticipantsCacheTTL  = 5 * time.Second
	defaultSpaceContextCacheTTL  = 5 * time.Second
	defaultAIParticipantCacheTTL = 5 * time.Second
	defaultAgentCacheTTL         = 60 * time.Second
	defaultSpaceInfoCacheTTL     = 60 * time.Second
	defaultAttachmentsCacheTTL   = 30 * time.Second
	defaultRecentUtteranceLimit  = 80
)

type cachedParticipants struct {
	expiresAt time.Time
	value     []map[string]any
}

type cachedSpaceContext struct {
	expiresAt time.Time
	value     map[string]any
}

type cachedAIParticipant struct {
	expiresAt time.Time
	value     *participantPayload
}

type cachedAgent struct {
	expiresAt time.Time
	value     *agentPayload
}

type spaceInfo struct {
	spaceType   string
	description string
	// goalStatement / goalTimeframe are the structured space goal set
	// by the Configure-manually path in CreateSpaceModal ("Plan Q3
	// roadmap", "Talk to ops about the Houston facility"; timeframe is
	// one of daily / weekly / monthly). Exposed in prompt context so
	// cognition routing + agent replies can key turn-taking and
	// role-playing decisions on what the space is FOR, not just on
	// whatever long-form description the AI-describe path stored.
	// timeframe is descriptive grounding only -- NOT tied to
	// space.kind=daily or status=scheduled (separate concerns).
	goalStatement string
	goalTimeframe string
	// name is the space's user-visible title. Without this in the
	// prompt context, agents truthfully answer "I can't see the
	// title" when asked, even though the spaceFull shape already
	// returns it. Plumbing the title through closes that gap so
	// agents can reference what the room is called.
	name string
}

type cachedSpaceInfo struct {
	expiresAt time.Time
	value     spaceInfo
}

// buildSpaceData constructs the space data map for prompt templates.
// Each field is only included when non-empty so the prompt-schema
// validator doesn't reject a blank string. The structured `goal`
// {statement, timeframe} is preferred by templates that offer a
// fallback chain; `description` still surfaces verbatim for spaces
// created via the AI-describe path that produces no goal.
func buildSpaceData(partitionId string, si spaceInfo) map[string]any {
	m := map[string]any{
		"id":        partitionId,
		"spaceType": si.spaceType,
	}
	if si.name != "" {
		m["name"] = si.name
	}
	if si.description != "" {
		m["description"] = si.description
	}
	if goal := buildGoalData(si); goal != nil {
		m["goal"] = goal
	}
	return m
}

// buildGoalData assembles the structured goal sub-map for prompt
// templates, returning nil when the space has no goal statement so
// the prompt-schema validator doesn't reject an empty object.
func buildGoalData(si spaceInfo) map[string]any {
	if si.goalStatement == "" {
		return nil
	}
	goal := map[string]any{"statement": si.goalStatement}
	if si.goalTimeframe != "" {
		goal["timeframe"] = si.goalTimeframe
	}
	return goal
}

type cachedAttachments struct {
	expiresAt time.Time
	value     []map[string]any
}

func (c *CognitionIntegration) invalidatePromptCachesForSpace(partitionId string) {
	if c == nil {
		return
	}
	partitionId = strings.TrimSpace(partitionId)
	if partitionId == "" {
		return
	}
	c.promptCacheMu.Lock()
	delete(c.participantsCache, partitionId)
	delete(c.spaceContextCache, partitionId)
	delete(c.aiParticipantCache, partitionId)
	delete(c.spaceInfoCache, partitionId)
	delete(c.attachmentsCache, partitionId)
	c.promptCacheMu.Unlock()
}

func (c *CognitionIntegration) getParticipantsForPromptCached(ctx context.Context, partitionId string) ([]map[string]any, error) {
	if c == nil {
		return nil, fmt.Errorf("cognition integration not configured")
	}
	partitionId = strings.TrimSpace(partitionId)
	if partitionId == "" {
		return nil, fmt.Errorf("partitionId is empty")
	}

	now := time.Now()
	c.promptCacheMu.Lock()
	if entry, ok := c.participantsCache[partitionId]; ok && !entry.expiresAt.IsZero() && now.Before(entry.expiresAt) {
		val := entry.value
		c.promptCacheMu.Unlock()
		return val, nil
	}
	c.promptCacheMu.Unlock()

	// Prevent thundering herd on cache miss (multiple concurrent responders / turn state evaluations).
	key := "participants:" + partitionId
	anyVal, err, _ := c.participantsSF.Do(key, func() (any, error) {
		val, err := c.getParticipantsForPrompt(ctx, partitionId)
		if err != nil {
			return nil, err
		}
		c.promptCacheMu.Lock()
		c.participantsCache[partitionId] = cachedParticipants{expiresAt: time.Now().Add(defaultParticipantsCacheTTL), value: val}
		c.promptCacheMu.Unlock()
		return val, nil
	})
	if err != nil {
		return nil, err
	}
	val, _ := anyVal.([]map[string]any)
	return val, nil
}

func (c *CognitionIntegration) getSpaceContextForPromptCached(ctx context.Context, partitionId string) map[string]any {
	if c == nil {
		return nil
	}
	partitionId = strings.TrimSpace(partitionId)
	if partitionId == "" {
		return nil
	}

	now := time.Now()
	c.promptCacheMu.Lock()
	if entry, ok := c.spaceContextCache[partitionId]; ok && !entry.expiresAt.IsZero() && now.Before(entry.expiresAt) {
		val := entry.value
		c.promptCacheMu.Unlock()
		return val
	}
	c.promptCacheMu.Unlock()

	key := "spacectx:" + partitionId
	anyVal, _, _ := c.spaceContextSF.Do(key, func() (any, error) {
		val := c.getSpaceContextForPrompt(ctx, partitionId)
		if val == nil {
			return nil, nil
		}
		c.promptCacheMu.Lock()
		c.spaceContextCache[partitionId] = cachedSpaceContext{expiresAt: time.Now().Add(defaultSpaceContextCacheTTL), value: val}
		c.promptCacheMu.Unlock()
		return val, nil
	})
	if anyVal == nil {
		return nil
	}
	val, _ := anyVal.(map[string]any)
	return val
}

func (c *CognitionIntegration) findAIParticipantCached(ctx context.Context, partitionId string) (*participantPayload, error) {
	if c == nil {
		return nil, fmt.Errorf("cognition integration not configured")
	}
	partitionId = strings.TrimSpace(partitionId)
	if partitionId == "" {
		return nil, fmt.Errorf("partitionId is empty")
	}

	now := time.Now()
	c.promptCacheMu.Lock()
	if entry, ok := c.aiParticipantCache[partitionId]; ok && entry.value != nil && !entry.expiresAt.IsZero() && now.Before(entry.expiresAt) {
		val := entry.value
		c.promptCacheMu.Unlock()
		return val, nil
	}
	c.promptCacheMu.Unlock()

	key := "siparticipant:" + partitionId
	anyVal, err, _ := c.aiParticipantSF.Do(key, func() (any, error) {
		val, err := c.findAIParticipant(ctx, partitionId)
		if err != nil {
			return nil, err
		}
		c.promptCacheMu.Lock()
		c.aiParticipantCache[partitionId] = cachedAIParticipant{expiresAt: time.Now().Add(defaultAIParticipantCacheTTL), value: val}
		c.promptCacheMu.Unlock()
		return val, nil
	})
	if err != nil {
		return nil, err
	}
	val, _ := anyVal.(*participantPayload)
	return val, nil
}

func (c *CognitionIntegration) getAgentCached(ctx context.Context, agentId string) (*agentPayload, error) {
	if c == nil {
		return nil, fmt.Errorf("cognition integration not configured")
	}
	agentId = strings.TrimSpace(agentId)
	if agentId == "" {
		return nil, fmt.Errorf("agentId is empty")
	}

	now := time.Now()
	c.promptCacheMu.Lock()
	if entry, ok := c.agentCache[agentId]; ok && entry.value != nil && !entry.expiresAt.IsZero() && now.Before(entry.expiresAt) {
		val := entry.value
		c.promptCacheMu.Unlock()
		return val, nil
	}
	c.promptCacheMu.Unlock()

	key := "agent:" + agentId
	anyVal, err, _ := c.agentSF.Do(key, func() (any, error) {
		val, err := c.getAgent(ctx, agentId)
		if err != nil {
			return nil, err
		}
		c.promptCacheMu.Lock()
		c.agentCache[agentId] = cachedAgent{expiresAt: time.Now().Add(defaultAgentCacheTTL), value: val}
		c.promptCacheMu.Unlock()
		return val, nil
	})
	if err != nil {
		return nil, err
	}
	val, _ := anyVal.(*agentPayload)
	return val, nil
}

// recordRecentUtteranceForPrompt stores a best-effort recent-utterance record in memory.
// This is used to avoid a DB query for prompt-building when the buffer is warm.
//
// Non-conversational utterance types (system) are skipped because their
// text field carries payload data (system metadata) that is not useful for
// prompt context and would waste buffer space.
func (c *CognitionIntegration) recordRecentUtteranceForPrompt(partitionId, utteranceId, participantId, utteranceType, text string) {
	if c == nil {
		return
	}
	partitionId = strings.TrimSpace(partitionId)
	utteranceId = strings.TrimSpace(utteranceId)
	if partitionId == "" || utteranceId == "" {
		return
	}

	// Skip non-conversational utterance types whose text is payload data, not dialogue.
	utteranceType = strings.TrimSpace(utteranceType)
	switch utteranceType {
	case "system":
		return
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	item := map[string]any{
		"id":            utteranceId,
		"participantId": strings.TrimSpace(participantId),
		"utteranceType": strings.TrimSpace(utteranceType),
		"text":          text,
		"createdAt":     "",
	}

	c.recentUtterMu.Lock()
	defer c.recentUtterMu.Unlock()

	cur := c.recentUtterCache[partitionId]
	// Dedupe within the small buffer to avoid duplicates on retries.
	for _, existing := range cur {
		if asString(existing["id"]) == utteranceId {
			return
		}
	}
	cur = append(cur, item)
	if len(cur) > defaultRecentUtteranceLimit {
		cur = cur[len(cur)-defaultRecentUtteranceLimit:]
	}
	c.recentUtterCache[partitionId] = cur
}

// getRecentUtterancesForPromptCached returns recent utterances from the in-memory buffer when warm.
// Falls back to the DB query when the buffer is empty or missing for the space.
func (c *CognitionIntegration) getRecentUtterancesForPromptCached(ctx context.Context, partitionId string, limit int) ([]map[string]any, error) {
	if c == nil {
		return nil, fmt.Errorf("cognition integration not configured")
	}
	partitionId = strings.TrimSpace(partitionId)
	if partitionId == "" {
		return nil, fmt.Errorf("partitionId is empty")
	}
	if limit <= 0 {
		limit = 1
	}

	// Fast path: in-memory buffer.
	c.recentUtterMu.Lock()
	cur := c.recentUtterCache[partitionId]
	if len(cur) > 0 {
		n := limit
		if n > len(cur) {
			n = len(cur)
		}
		start := len(cur) - n
		out := make([]map[string]any, 0, n)
		for _, it := range cur[start:] {
			m := make(map[string]any, len(it))
			for k, v := range it {
				m[k] = v
			}
			out = append(out, m)
		}
		c.recentUtterMu.Unlock()
		return out, nil
	}
	c.recentUtterMu.Unlock()

	// Slow path: DB (singleflight per space+limit to avoid herd).
	key := fmt.Sprintf("recent:%s:%d", partitionId, limit)
	anyVal, err, _ := c.recentUtterSF.Do(key, func() (any, error) {
		val, err := c.getRecentUtterancesForPrompt(ctx, partitionId, limit)
		if err != nil {
			return nil, err
		}
		// Best-effort warm.
		c.recentUtterMu.Lock()
		cur := c.recentUtterCache[partitionId]
		for _, it := range val {
			if it == nil {
				continue
			}
			id := strings.TrimSpace(asString(it["id"]))
			text := strings.TrimSpace(asString(it["text"]))
			if id == "" || text == "" {
				continue
			}
			dup := false
			for _, existing := range cur {
				if asString(existing["id"]) == id {
					dup = true
					break
				}
			}
			if dup {
				continue
			}
			cur = append(cur, it)
			if len(cur) > defaultRecentUtteranceLimit {
				cur = cur[len(cur)-defaultRecentUtteranceLimit:]
			}
		}
		c.recentUtterCache[partitionId] = cur
		c.recentUtterMu.Unlock()
		return val, nil
	})
	if err != nil {
		return nil, err
	}
	val, _ := anyVal.([]map[string]any)
	return val, nil
}

// getSpaceInfoCached retrieves spaceType and description from the
// space payload with caching. Returns zero-value spaceInfo if not
// found.
func (c *CognitionIntegration) getSpaceInfoCached(ctx context.Context, partitionId string) spaceInfo {
	if c == nil || c.engine == nil {
		return spaceInfo{}
	}
	partitionId = strings.TrimSpace(partitionId)
	if partitionId == "" {
		return spaceInfo{}
	}

	// Fast path: check cache.
	now := time.Now()
	c.promptCacheMu.Lock()
	if entry, ok := c.spaceInfoCache[partitionId]; ok && !entry.expiresAt.IsZero() && now.Before(entry.expiresAt) {
		val := entry.value
		c.promptCacheMu.Unlock()
		return val
	}
	c.promptCacheMu.Unlock()

	// Slow path: DB (singleflight to avoid herd).
	anyVal, _, _ := c.spaceInfoSF.Do(partitionId, func() (any, error) {
		// memql#289 (sub-epic #286): migrated from a handwritten
		// `shape(paginate(filter;id==%s, 1), "spaceFull")` runtime
		// query to the existing DSL-defined querySpaceMeta. The
		// id== filter naturally returns 0-or-1 rows so no
		// sort/paginate directives are needed (unlike sibling
		// children B + D which required the #294 directive
		// wiring). Downstream spaceFull-shape extraction is
		// unchanged. Unblocks #250.
		query := fmt.Sprintf(`query querySpaceMeta(partitionId: %s)`, escapeJSONString(partitionId))
		result, err := c.engine.Execute(ctx, query)
		if err != nil {
			return spaceInfo{}, nil
		}
		data, err := extractDataFromResult(result)
		if err != nil {
			return spaceInfo{}, nil
		}
		var val spaceInfo
		if arr, ok := data.([]any); ok && len(arr) > 0 {
			if m, ok := arr[0].(map[string]any); ok {
				if st, ok := m["spaceType"].(string); ok {
					val.spaceType = st
				}
				if desc, ok := m["description"].(string); ok {
					val.description = desc
				}
				if goal, ok := m["goal"].(map[string]any); ok {
					if s, ok := goal["statement"].(string); ok {
						val.goalStatement = s
					}
					if tf, ok := goal["timeframe"].(string); ok {
						val.goalTimeframe = tf
					}
				}
				// `name` is the user-visible space title. Without
				// extracting it here, agents can't ground answers
				// to "what is this space called" or "can you see
				// the title" -- they just say "no" because the
				// prompt has no name to reference.
				if n, ok := m["name"].(string); ok {
					val.name = n
				}
			}
		}
		// Cache result (even empty to avoid repeated lookups).
		c.promptCacheMu.Lock()
		c.spaceInfoCache[partitionId] = cachedSpaceInfo{expiresAt: time.Now().Add(defaultSpaceInfoCacheTTL), value: val}
		c.promptCacheMu.Unlock()
		return val, nil
	})
	val, _ := anyVal.(spaceInfo)
	return val
}

// getAttachmentsForPromptCached returns attachment summaries for a space with TTL caching.
// Only summaries (not full transcriptions) are returned so prompt size stays bounded.
func (c *CognitionIntegration) getAttachmentsForPromptCached(ctx context.Context, partitionId string) []map[string]any {
	if c == nil || c.engine == nil {
		return nil
	}
	partitionId = strings.TrimSpace(partitionId)
	if partitionId == "" {
		return nil
	}

	now := time.Now()
	c.promptCacheMu.Lock()
	if entry, ok := c.attachmentsCache[partitionId]; ok && !entry.expiresAt.IsZero() && now.Before(entry.expiresAt) {
		val := entry.value
		c.promptCacheMu.Unlock()
		return val
	}
	c.promptCacheMu.Unlock()

	key := "attachments:" + partitionId
	anyVal, _, _ := c.attachmentsSF.Do(key, func() (any, error) {
		val := c.getAttachmentsForPrompt(ctx, partitionId)
		c.promptCacheMu.Lock()
		c.attachmentsCache[partitionId] = cachedAttachments{expiresAt: time.Now().Add(defaultAttachmentsCacheTTL), value: val}
		c.promptCacheMu.Unlock()
		return val, nil
	})
	if anyVal == nil {
		return nil
	}
	val, _ := anyVal.([]map[string]any)
	return val
}

// getAttachmentsForPrompt returns ready attachments for the space as a
// slice of summary-only maps for inclusion in the AI prompt context.
// Dispatches through the querySpaceAttachments MemQL function so
// cognition Go code stays free of direct copresent concept references.
func (c *CognitionIntegration) getAttachmentsForPrompt(ctx context.Context, partitionId string) []map[string]any {
	if c == nil || c.engine == nil {
		return nil
	}
	query := fmt.Sprintf(`query querySpaceAttachments(partitionId: %s, status: "ready")`,
		escapeJSONString(partitionId))

	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		c.Logger.Debug("fetch space attachments for prompt", "error", err, "partitionId", partitionId)
		return nil
	}
	data, err := extractDataFromResult(result)
	if err != nil || data == nil {
		return nil
	}

	var items []any
	switch v := data.(type) {
	case []any:
		items = v
	case map[string]any:
		items = []any{v}
	default:
		return nil
	}

	summaries := make([]map[string]any, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		fileName, _ := m["fileName"].(string)
		if strings.TrimSpace(fileName) == "" {
			continue
		}
		entry := map[string]any{
			"fileName": strings.TrimSpace(fileName),
			"mimeType": strings.TrimSpace(asString(m["mimeType"])),
		}
		if summary := strings.TrimSpace(asString(m["summary"])); summary != "" {
			entry["summary"] = summary
		}
		summaries = append(summaries, entry)
	}
	return summaries
}
