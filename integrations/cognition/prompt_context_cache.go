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
	defaultSIParticipantCacheTTL = 5 * time.Second
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

type cachedSIParticipant struct {
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
	// purpose is the short one-liner set by the Configure-manually
	// path in CreateSpaceModal ("Plan Q3 roadmap", "Talk to ops
	// about the Houston facility"). Exposed in prompt context so
	// cognition routing + agent replies can key turn-taking and
	// role-playing decisions on what the space is FOR, not just on
	// whatever long-form description the AI-describe path stored.
	purpose string
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
// validator doesn't reject a blank string. The newer `purpose`
// field (short one-liner) is preferred by templates that offer a
// fallback chain; legacy `description` still surfaces verbatim for
// spaces that predate purpose.
func buildSpaceData(spaceId string, si spaceInfo) map[string]any {
	m := map[string]any{
		"id":        spaceId,
		"spaceType": si.spaceType,
	}
	if si.name != "" {
		m["name"] = si.name
	}
	if si.description != "" {
		m["description"] = si.description
	}
	if si.purpose != "" {
		m["purpose"] = si.purpose
	}
	return m
}

type cachedAttachments struct {
	expiresAt time.Time
	value     []map[string]any
}

func (c *CognitionIntegration) invalidatePromptCachesForSpace(spaceId string) {
	if c == nil {
		return
	}
	spaceId = strings.TrimSpace(spaceId)
	if spaceId == "" {
		return
	}
	c.promptCacheMu.Lock()
	delete(c.participantsCache, spaceId)
	delete(c.spaceContextCache, spaceId)
	delete(c.siParticipantCache, spaceId)
	delete(c.spaceInfoCache, spaceId)
	delete(c.attachmentsCache, spaceId)
	c.promptCacheMu.Unlock()
}

func (c *CognitionIntegration) getParticipantsForPromptCached(ctx context.Context, spaceId string) ([]map[string]any, error) {
	if c == nil {
		return nil, fmt.Errorf("cognition integration not configured")
	}
	spaceId = strings.TrimSpace(spaceId)
	if spaceId == "" {
		return nil, fmt.Errorf("spaceId is empty")
	}

	now := time.Now()
	c.promptCacheMu.Lock()
	if entry, ok := c.participantsCache[spaceId]; ok && !entry.expiresAt.IsZero() && now.Before(entry.expiresAt) {
		val := entry.value
		c.promptCacheMu.Unlock()
		return val, nil
	}
	c.promptCacheMu.Unlock()

	// Prevent thundering herd on cache miss (multiple concurrent responders / turn state evaluations).
	key := "participants:" + spaceId
	anyVal, err, _ := c.participantsSF.Do(key, func() (any, error) {
		val, err := c.getParticipantsForPrompt(ctx, spaceId)
		if err != nil {
			return nil, err
		}
		c.promptCacheMu.Lock()
		c.participantsCache[spaceId] = cachedParticipants{expiresAt: time.Now().Add(defaultParticipantsCacheTTL), value: val}
		c.promptCacheMu.Unlock()
		return val, nil
	})
	if err != nil {
		return nil, err
	}
	val, _ := anyVal.([]map[string]any)
	return val, nil
}

func (c *CognitionIntegration) getSpaceContextForPromptCached(ctx context.Context, spaceId string) map[string]any {
	if c == nil {
		return nil
	}
	spaceId = strings.TrimSpace(spaceId)
	if spaceId == "" {
		return nil
	}

	now := time.Now()
	c.promptCacheMu.Lock()
	if entry, ok := c.spaceContextCache[spaceId]; ok && !entry.expiresAt.IsZero() && now.Before(entry.expiresAt) {
		val := entry.value
		c.promptCacheMu.Unlock()
		return val
	}
	c.promptCacheMu.Unlock()

	key := "spacectx:" + spaceId
	anyVal, _, _ := c.spaceContextSF.Do(key, func() (any, error) {
		val := c.getSpaceContextForPrompt(ctx, spaceId)
		if val == nil {
			return nil, nil
		}
		c.promptCacheMu.Lock()
		c.spaceContextCache[spaceId] = cachedSpaceContext{expiresAt: time.Now().Add(defaultSpaceContextCacheTTL), value: val}
		c.promptCacheMu.Unlock()
		return val, nil
	})
	if anyVal == nil {
		return nil
	}
	val, _ := anyVal.(map[string]any)
	return val
}

func (c *CognitionIntegration) findSIParticipantCached(ctx context.Context, spaceId string) (*participantPayload, error) {
	if c == nil {
		return nil, fmt.Errorf("cognition integration not configured")
	}
	spaceId = strings.TrimSpace(spaceId)
	if spaceId == "" {
		return nil, fmt.Errorf("spaceId is empty")
	}

	now := time.Now()
	c.promptCacheMu.Lock()
	if entry, ok := c.siParticipantCache[spaceId]; ok && entry.value != nil && !entry.expiresAt.IsZero() && now.Before(entry.expiresAt) {
		val := entry.value
		c.promptCacheMu.Unlock()
		return val, nil
	}
	c.promptCacheMu.Unlock()

	key := "siparticipant:" + spaceId
	anyVal, err, _ := c.siParticipantSF.Do(key, func() (any, error) {
		val, err := c.findSIParticipant(ctx, spaceId)
		if err != nil {
			return nil, err
		}
		c.promptCacheMu.Lock()
		c.siParticipantCache[spaceId] = cachedSIParticipant{expiresAt: time.Now().Add(defaultSIParticipantCacheTTL), value: val}
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
func (c *CognitionIntegration) recordRecentUtteranceForPrompt(spaceId, utteranceId, participantId, utteranceType, text string) {
	if c == nil {
		return
	}
	spaceId = strings.TrimSpace(spaceId)
	utteranceId = strings.TrimSpace(utteranceId)
	if spaceId == "" || utteranceId == "" {
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

	cur := c.recentUtterCache[spaceId]
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
	c.recentUtterCache[spaceId] = cur
}

// getRecentUtterancesForPromptCached returns recent utterances from the in-memory buffer when warm.
// Falls back to the DB query when the buffer is empty or missing for the space.
func (c *CognitionIntegration) getRecentUtterancesForPromptCached(ctx context.Context, spaceId string, limit int) ([]map[string]any, error) {
	if c == nil {
		return nil, fmt.Errorf("cognition integration not configured")
	}
	spaceId = strings.TrimSpace(spaceId)
	if spaceId == "" {
		return nil, fmt.Errorf("spaceId is empty")
	}
	if limit <= 0 {
		limit = 1
	}

	// Fast path: in-memory buffer.
	c.recentUtterMu.Lock()
	cur := c.recentUtterCache[spaceId]
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
	key := fmt.Sprintf("recent:%s:%d", spaceId, limit)
	anyVal, err, _ := c.recentUtterSF.Do(key, func() (any, error) {
		val, err := c.getRecentUtterancesForPrompt(ctx, spaceId, limit)
		if err != nil {
			return nil, err
		}
		// Best-effort warm.
		c.recentUtterMu.Lock()
		cur := c.recentUtterCache[spaceId]
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
		c.recentUtterCache[spaceId] = cur
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
func (c *CognitionIntegration) getSpaceInfoCached(ctx context.Context, spaceId string) spaceInfo {
	if c == nil || c.engine == nil {
		return spaceInfo{}
	}
	spaceId = strings.TrimSpace(spaceId)
	if spaceId == "" {
		return spaceInfo{}
	}

	// Fast path: check cache.
	now := time.Now()
	c.promptCacheMu.Lock()
	if entry, ok := c.spaceInfoCache[spaceId]; ok && !entry.expiresAt.IsZero() && now.Before(entry.expiresAt) {
		val := entry.value
		c.promptCacheMu.Unlock()
		return val
	}
	c.promptCacheMu.Unlock()

	// Slow path: DB (singleflight to avoid herd).
	anyVal, _, _ := c.spaceInfoSF.Do(spaceId, func() (any, error) {
		query := fmt.Sprintf(`shape(
			paginate(concept==v1:cognition:space;id==%s, 1),
			"spaceFull"
		)`, escapeJSONString(spaceId))

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
				if p, ok := m["purpose"].(string); ok {
					val.purpose = p
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
		c.spaceInfoCache[spaceId] = cachedSpaceInfo{expiresAt: time.Now().Add(defaultSpaceInfoCacheTTL), value: val}
		c.promptCacheMu.Unlock()
		return val, nil
	})
	val, _ := anyVal.(spaceInfo)
	return val
}


// getAttachmentsForPromptCached returns attachment summaries for a space with TTL caching.
// Only summaries (not full transcriptions) are returned so prompt size stays bounded.
func (c *CognitionIntegration) getAttachmentsForPromptCached(ctx context.Context, spaceId string) []map[string]any {
	if c == nil || c.engine == nil {
		return nil
	}
	spaceId = strings.TrimSpace(spaceId)
	if spaceId == "" {
		return nil
	}

	now := time.Now()
	c.promptCacheMu.Lock()
	if entry, ok := c.attachmentsCache[spaceId]; ok && !entry.expiresAt.IsZero() && now.Before(entry.expiresAt) {
		val := entry.value
		c.promptCacheMu.Unlock()
		return val
	}
	c.promptCacheMu.Unlock()

	key := "attachments:" + spaceId
	anyVal, _, _ := c.attachmentsSF.Do(key, func() (any, error) {
		val := c.getAttachmentsForPrompt(ctx, spaceId)
		c.promptCacheMu.Lock()
		c.attachmentsCache[spaceId] = cachedAttachments{expiresAt: time.Now().Add(defaultAttachmentsCacheTTL), value: val}
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
// slice of summary-only maps for inclusion in the SI prompt context.
// Dispatches through the querySpaceAttachments MemQL function so
// cognition Go code stays free of direct copresent concept references.
func (c *CognitionIntegration) getAttachmentsForPrompt(ctx context.Context, spaceId string) []map[string]any {
	if c == nil || c.engine == nil {
		return nil
	}
	query := fmt.Sprintf(`querySpaceAttachments({spaceId: %s, status: "ready"})`,
		escapeJSONString(spaceId))

	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		c.Logger.Debug("fetch space attachments for prompt", "error", err, "spaceId", spaceId)
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

