package cognition

import (
	"context"
	"fmt"
	"strings"
	"time"

	memoryNodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	"github.com/visionarys-io/memql/component/events"
)

const (
	spaceContextIdPrefix = "spacectx-"
)

func spaceContextRecordId(spaceId string) string {
	sid := strings.TrimSpace(spaceId)
	if sid == "" {
		return ""
	}
	return spaceContextIdPrefix + sid
}

// handleParticipantChanged updates the space context snapshot whenever participants change.
func (c *CognitionIntegration) handleParticipantChanged(event events.Event) {
	ctx := contextWithSystemActor(context.Background())
	spaceId, _ := event.Payload["spaceId"].(string)
	if strings.TrimSpace(spaceId) == "" {
		if nested, ok := event.Payload["payload"].(map[string]any); ok {
			spaceId, _ = nested["spaceId"].(string)
		}
	}
	if strings.TrimSpace(spaceId) == "" {
		return
	}
	c.invalidatePromptCachesForSpace(spaceId)
	_ = c.recomputeAndUpsertSpaceContext(ctx, spaceId)
}

// handleSessionChanged updates the space context snapshot whenever sessions change.
func (c *CognitionIntegration) handleSessionChanged(event events.Event) {
	ctx := contextWithSystemActor(context.Background())
	spaceId, _ := event.Payload["spaceId"].(string)
	if strings.TrimSpace(spaceId) == "" {
		if nested, ok := event.Payload["payload"].(map[string]any); ok {
			spaceId, _ = nested["spaceId"].(string)
		}
	}
	if strings.TrimSpace(spaceId) == "" {
		return
	}
	c.invalidatePromptCachesForSpace(spaceId)
	_ = c.recomputeAndUpsertSpaceContext(ctx, spaceId)
}

// handleSpaceCreated bootstraps a space context record after a space is created.
func (c *CognitionIntegration) handleSpaceCreated(event events.Event) {
	ctx := contextWithSystemActor(context.Background())
	spaceId, _ := event.Payload["nodeId"].(string)
	if strings.TrimSpace(spaceId) == "" {
		spaceId, _ = event.Payload["id"].(string)
	}
	if strings.TrimSpace(spaceId) == "" {
		if nested, ok := event.Payload["payload"].(map[string]any); ok {
			spaceId, _ = nested["id"].(string)
		}
	}
	if strings.TrimSpace(spaceId) == "" {
		return
	}
	c.invalidatePromptCachesForSpace(spaceId)
	_ = c.recomputeAndUpsertSpaceContext(ctx, spaceId)
}

func (c *CognitionIntegration) recomputeAndUpsertSpaceContext(ctx context.Context, spaceId string) error {
	if c == nil || c.engine == nil {
		return fmt.Errorf("engine not configured")
	}
	spaceId = strings.TrimSpace(spaceId)
	if spaceId == "" {
		return fmt.Errorf("spaceId is empty")
	}

	snap, err := c.computeSpaceContextSnapshot(ctx, spaceId)
	if err != nil {
		return err
	}

	// Load latest snapshot for change detection (compare only snapshot object).
	prev, _ := c.getLatestSpaceContextSnapshot(ctx, spaceId)
	if prev != nil {
		prevJSON := mustJSON(prev)
		nextJSON := mustJSON(snap)
		if prevJSON == nextJSON {
			return nil // no change
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	payload := map[string]any{
		"spaceId":       spaceId,
		"snapshot":      snap,
		"computedAt":    now,
		"lastUpdatedAt": now,
	}
	id := spaceContextRecordId(spaceId)
	if id == "" {
		return fmt.Errorf("invalid context record id")
	}
	query := fmt.Sprintf(`insert("%s", id=%s, payload=%s)`,
		memoryNodes.ConceptCognitionSpaceContext,
		escapeJSONString(id),
		mustJSON(payload),
	)
	_, err = c.engine.Execute(ctx, query)
	if err == nil {
		// Best-effort: update in-memory cache so prompt builders don't need to re-query.
		c.promptCacheMu.Lock()
		c.spaceContextCache[spaceId] = cachedSpaceContext{
			expiresAt: time.Now().Add(defaultSpaceContextCacheTTL),
			value: map[string]any{
				"spaceId":     spaceId,
				"snapshot":    snap,
				"computedAt":  now,
				"lastUpdated": now,
			},
		}
		c.promptCacheMu.Unlock()
	}
	return err
}

func (c *CognitionIntegration) getLatestSpaceContextSnapshot(ctx context.Context, spaceId string) (map[string]any, error) {
	if c == nil || c.engine == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	spaceId = strings.TrimSpace(spaceId)
	if spaceId == "" {
		return nil, fmt.Errorf("spaceId is empty")
	}
	// Shape only the snapshot field for deterministic comparison.
	query := fmt.Sprintf(`shape(
  paginate(sort(concept==v1:cognition:space:context;payload.spaceId==%s, "createdAt", "desc"), 1),
  {"snapshot": node("payload.snapshot")}
)`, escapeJSONString(spaceId))
	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		return nil, err
	}
	data, err := extractDataFromResult(result)
	if err != nil {
		return nil, err
	}
	switch v := data.(type) {
	case []any:
		if len(v) == 0 {
			return nil, nil
		}
		if m, ok := v[0].(map[string]any); ok {
			if snap, ok := m["snapshot"].(map[string]any); ok {
				return snap, nil
			}
		}
	case map[string]any:
		if snap, ok := v["snapshot"].(map[string]any); ok {
			return snap, nil
		}
	}
	return nil, nil
}

func (c *CognitionIntegration) computeSpaceContextSnapshot(ctx context.Context, spaceId string) (map[string]any, error) {
	participants, err := c.loadParticipantsForContext(ctx, spaceId)
	if err != nil {
		return nil, err
	}
	sessions, err := c.loadSessionsForContext(ctx, spaceId)
	if err != nil {
		return nil, err
	}

	participantCounts := map[string]any{
		"total":  0,
		"humans": 0,
		"sis":    0,
		"active": 0,
		"idle":   0,
		"left":   0,
	}
	for _, p := range participants {
		participantCounts["total"] = participantCounts["total"].(int) + 1
		pt, _ := p["participantType"].(string)
		switch strings.TrimSpace(pt) {
		case "human":
			participantCounts["humans"] = participantCounts["humans"].(int) + 1
		case "si":
			participantCounts["sis"] = participantCounts["sis"].(int) + 1
		}
		status, _ := p["status"].(string)
		switch strings.TrimSpace(status) {
		case "active":
			participantCounts["active"] = participantCounts["active"].(int) + 1
		case "idle":
			participantCounts["idle"] = participantCounts["idle"].(int) + 1
		case "left":
			participantCounts["left"] = participantCounts["left"].(int) + 1
		}
	}

	// Session aggregation.
	tierCounts := map[string]any{}
	humanAgg := map[string]any{
		"microphoneEnabled": 0,
		"cameraEnabled":     0,
		"isSpeaking":        0,
		"audioActive":       0,
		"videoActive":       0,
	}
	siAgg := map[string]any{
		"voiceEnabled":  0,
		"avatarEnabled": 0,
		"visionEnabled": 0,
		"avatarActive":  0,
	}

	const activeWindow = 30 * time.Second
	now := time.Now().UTC()

	for _, s := range sessions {
		// tier counts
		if tier, _ := s["activeTier"].(string); strings.TrimSpace(tier) != "" {
			cur := 0
			if v, ok := tierCounts[tier].(int); ok {
				cur = v
			}
			tierCounts[tier] = cur + 1
		}

		lastActivity := parseBestEffortTime(s["lastActivityAt"], s["startedAt"])
		isRecent := false
		if !lastActivity.IsZero() && now.Sub(lastActivity) <= activeWindow {
			isRecent = true
		}

		if hi, ok := s["humanInput"].(map[string]any); ok {
			if b, _ := hi["microphoneEnabled"].(bool); b {
				humanAgg["microphoneEnabled"] = humanAgg["microphoneEnabled"].(int) + 1
			}
			if b, _ := hi["cameraEnabled"].(bool); b {
				humanAgg["cameraEnabled"] = humanAgg["cameraEnabled"].(int) + 1
			}
			if b, _ := hi["isSpeaking"].(bool); b {
				humanAgg["isSpeaking"] = humanAgg["isSpeaking"].(int) + 1
			}
			if streams, ok := s["streams"].(map[string]any); ok && isRecent {
				if strings.TrimSpace(asString(streams["audioTrackId"])) != "" {
					humanAgg["audioActive"] = humanAgg["audioActive"].(int) + 1
				}
				if strings.TrimSpace(asString(streams["videoTrackId"])) != "" {
					humanAgg["videoActive"] = humanAgg["videoActive"].(int) + 1
				}
			}
		}

		if ao, ok := s["aiOutput"].(map[string]any); ok {
			if b, _ := ao["voiceEnabled"].(bool); b {
				siAgg["voiceEnabled"] = siAgg["voiceEnabled"].(int) + 1
			}
			if b, _ := ao["avatarEnabled"].(bool); b {
				siAgg["avatarEnabled"] = siAgg["avatarEnabled"].(int) + 1
			}
			if b, _ := ao["visionEnabled"].(bool); b {
				siAgg["visionEnabled"] = siAgg["visionEnabled"].(int) + 1
			}
			if streams, ok := s["streams"].(map[string]any); ok && isRecent {
				if strings.TrimSpace(asString(streams["avatarStreamId"])) != "" {
					siAgg["avatarActive"] = siAgg["avatarActive"].(int) + 1
				}
			}
		}
	}

	sessionCounts := map[string]any{
		"total": len(sessions),
		"tiers": tierCounts,
		"human": humanAgg,
		"si":    siAgg,
	}

	return map[string]any{
		"participants": participantCounts,
		"sessions":     sessionCounts,
	}, nil
}

// loadParticipantsForContext retrieves participants for space context computation.
// Delegates to the spaceParticipants MemQL query function.
func (c *CognitionIntegration) loadParticipantsForContext(ctx context.Context, spaceId string) ([]map[string]any, error) {
	query := fmt.Sprintf(`querySpaceParticipants({spaceId: "%s"})`, spaceId)
	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		return nil, err
	}
	data, err := extractDataFromResult(result)
	if err != nil {
		return nil, err
	}
	var items []any
	switch v := data.(type) {
	case []any:
		items = v
	case map[string]any:
		items = []any{v}
	default:
		return nil, fmt.Errorf("unexpected participants data type: %T", data)
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (c *CognitionIntegration) loadSessionsForContext(ctx context.Context, spaceId string) ([]map[string]any, error) {
	query := fmt.Sprintf(`shape(
  concept==v1:cognition:session;payload.spaceId==%s,
  "sessionFull"
)`, escapeJSONString(spaceId))
	result, err := c.engine.Execute(ctx, query)
	if err != nil {
		return nil, err
	}
	data, err := extractDataFromResult(result)
	if err != nil {
		return nil, err
	}
	var items []any
	switch v := data.(type) {
	case []any:
		items = v
	case map[string]any:
		items = []any{v}
	default:
		return nil, fmt.Errorf("unexpected sessions data type: %T", data)
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func parseBestEffortTime(primary any, fallback any) time.Time {
	// primary and fallback are expected to be RFC3339 strings.
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(asString(primary))); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(asString(fallback))); err == nil {
		return t
	}
	return time.Time{}
}
