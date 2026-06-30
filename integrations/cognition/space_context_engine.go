package cognition

import (
	"context"
	"fmt"
	"strings"
	"time"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
)

const (
	spaceContextIdPrefix = "spacectx-"
)

func spaceContextRecordId(partitionId string) string {
	sid := strings.TrimSpace(partitionId)
	if sid == "" {
		return ""
	}
	return spaceContextIdPrefix + sid
}

// handleParticipantChanged updates the space context snapshot whenever participants change.
func (c *CognitionIntegration) handleParticipantChanged(event events.Event) {
	ctx := contextWithSystemActor(context.Background())
	partitionId, _ := event.Payload["partitionId"].(string)
	if strings.TrimSpace(partitionId) == "" {
		if nested, ok := event.Payload["payload"].(map[string]any); ok {
			partitionId, _ = nested["partitionId"].(string)
		}
	}
	if strings.TrimSpace(partitionId) == "" {
		return
	}
	c.invalidatePromptCachesForSpace(partitionId)
	_ = c.recomputeAndUpsertSpaceContext(ctx, partitionId)
}

// handleSessionChanged updates the space context snapshot whenever sessions change.
func (c *CognitionIntegration) handleSessionChanged(event events.Event) {
	ctx := contextWithSystemActor(context.Background())
	partitionId, _ := event.Payload["partitionId"].(string)
	if strings.TrimSpace(partitionId) == "" {
		if nested, ok := event.Payload["payload"].(map[string]any); ok {
			partitionId, _ = nested["partitionId"].(string)
		}
	}
	if strings.TrimSpace(partitionId) == "" {
		return
	}
	c.invalidatePromptCachesForSpace(partitionId)
	_ = c.recomputeAndUpsertSpaceContext(ctx, partitionId)
}

// handleSpaceCreated bootstraps a space context record after a space is created.
func (c *CognitionIntegration) handleSpaceCreated(event events.Event) {
	ctx := contextWithSystemActor(context.Background())
	partitionId, _ := event.Payload["nodeId"].(string)
	if strings.TrimSpace(partitionId) == "" {
		partitionId, _ = event.Payload["id"].(string)
	}
	if strings.TrimSpace(partitionId) == "" {
		if nested, ok := event.Payload["payload"].(map[string]any); ok {
			partitionId, _ = nested["id"].(string)
		}
	}
	if strings.TrimSpace(partitionId) == "" {
		return
	}
	c.invalidatePromptCachesForSpace(partitionId)
	_ = c.recomputeAndUpsertSpaceContext(ctx, partitionId)
}

func (c *CognitionIntegration) recomputeAndUpsertSpaceContext(ctx context.Context, partitionId string) error {
	if c == nil || c.engine == nil {
		return fmt.Errorf("engine not configured")
	}
	partitionId = strings.TrimSpace(partitionId)
	if partitionId == "" {
		return fmt.Errorf("partitionId is empty")
	}

	snap, err := c.computeSpaceContextSnapshot(ctx, partitionId)
	if err != nil {
		return err
	}

	// Load latest snapshot for change detection (compare only snapshot object).
	prev, _ := c.getLatestSpaceContextSnapshot(ctx, partitionId)
	if prev != nil {
		prevJSON := mustJSON(prev)
		nextJSON := mustJSON(snap)
		if prevJSON == nextJSON {
			return nil // no change
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	payload := map[string]any{
		"partitionId":   partitionId,
		"snapshot":      snap,
		"computedAt":    now,
		"lastUpdatedAt": now,
	}
	id := spaceContextRecordId(partitionId)
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
		c.spaceContextCache[partitionId] = cachedSpaceContext{
			expiresAt: time.Now().Add(defaultSpaceContextCacheTTL),
			value: map[string]any{
				"partitionId": partitionId,
				"snapshot":    snap,
				"computedAt":  now,
				"lastUpdated": now,
			},
		}
		c.promptCacheMu.Unlock()
	}
	return err
}

func (c *CognitionIntegration) getLatestSpaceContextSnapshot(ctx context.Context, partitionId string) (map[string]any, error) {
	if c == nil || c.engine == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	partitionId = strings.TrimSpace(partitionId)
	if partitionId == "" {
		return nil, fmt.Errorf("partitionId is empty")
	}
	// memql#290 (sub-epic #286, final child): migrated from a
	// handwritten `shape(paginate(sort(filter, "createdAt",
	// "desc"), 1), {"snapshot": node("payload.snapshot")})`
	// runtime query to the DSL-defined queryLatestSpaceContextSnapshot,
	// which uses the snapshot-only spaceContextSnapshotOnly shape
	// + the sort/paginate directives wired in memql#294. The
	// projection minimises wire payload for the deterministic-
	// comparison path. Downstream extractDataFromResult sees the
	// same `{"snapshot": <map>}` shape so the type-switch below is
	// unchanged. Closes sub-epic #286; unblocks #250.
	query := fmt.Sprintf(`query queryLatestSpaceContextSnapshot(partitionId: %s)`, escapeJSONString(partitionId))
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

func (c *CognitionIntegration) computeSpaceContextSnapshot(ctx context.Context, partitionId string) (map[string]any, error) {
	participants, err := c.loadParticipantsForContext(ctx, partitionId)
	if err != nil {
		return nil, err
	}
	sessions, err := c.loadSessionsForContext(ctx, partitionId)
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
	aiAgg := map[string]any{
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
				aiAgg["voiceEnabled"] = aiAgg["voiceEnabled"].(int) + 1
			}
			if b, _ := ao["avatarEnabled"].(bool); b {
				aiAgg["avatarEnabled"] = aiAgg["avatarEnabled"].(int) + 1
			}
			if b, _ := ao["visionEnabled"].(bool); b {
				aiAgg["visionEnabled"] = aiAgg["visionEnabled"].(int) + 1
			}
			if streams, ok := s["streams"].(map[string]any); ok && isRecent {
				if strings.TrimSpace(asString(streams["avatarStreamId"])) != "" {
					aiAgg["avatarActive"] = aiAgg["avatarActive"].(int) + 1
				}
			}
		}
	}

	sessionCounts := map[string]any{
		"total": len(sessions),
		"tiers": tierCounts,
		"human": humanAgg,
		"si":    aiAgg,
	}

	return map[string]any{
		"participants": participantCounts,
		"sessions":     sessionCounts,
	}, nil
}

// loadParticipantsForContext retrieves participants for space context computation.
// Delegates to the spaceParticipants MemQL query function.
func (c *CognitionIntegration) loadParticipantsForContext(ctx context.Context, partitionId string) ([]map[string]any, error) {
	// escapeJSONString (json.Marshal) supplies the quotes, so no literal
	// quote remains in the format string (go/unsafe-quoting).
	query := fmt.Sprintf(`query spaceParticipants(partitionId: %s)`, escapeJSONString(partitionId))
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

func (c *CognitionIntegration) loadSessionsForContext(ctx context.Context, partitionId string) ([]map[string]any, error) {
	query := fmt.Sprintf(`shape(
  concept==v1:cognition:session;payload.partitionId==%s,
  "sessionFull"
)`, escapeJSONString(partitionId))
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
