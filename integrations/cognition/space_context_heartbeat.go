package cognition

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// startSpaceContextHeartbeat runs a periodic reconciliation of space context snapshots.
func (c *CognitionIntegration) startSpaceContextHeartbeat(ctx context.Context) {
	if c == nil {
		return
	}
	// Avoid double-start.
	if c.spaceContextHeartbeatCancel != nil {
		return
	}

	hctx, cancel := context.WithCancel(ctx)
	c.spaceContextHeartbeatCancel = cancel
	c.spaceContextHeartbeatDone = make(chan struct{})

	go func() {
		defer close(c.spaceContextHeartbeatDone)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-hctx.Done():
				return
			case <-ticker.C:
				_ = c.recomputeAllSpacesContext(hctx)
			}
		}
	}()
}

func (c *CognitionIntegration) stopSpaceContextHeartbeat() {
	if c == nil {
		return
	}
	if c.spaceContextHeartbeatCancel != nil {
		c.spaceContextHeartbeatCancel()
	}
	if c.spaceContextHeartbeatDone != nil {
		<-c.spaceContextHeartbeatDone
	}
	c.spaceContextHeartbeatCancel = nil
	c.spaceContextHeartbeatDone = nil
}

func (c *CognitionIntegration) recomputeAllSpacesContext(ctx context.Context) error {
	if c == nil || c.engine == nil {
		return nil
	}
	// Load all active spaces (best-effort).
	// memql#287 (sub-epic #286): migrated from a handwritten
	// `shape(concept==v1:cognition:space;payload.active==true,
	// {"id": node("id")})` runtime query to a DSL-defined query
	// that produces the equivalent shape via spaceIdOnly. The
	// downstream extractDataFromResult call returns the same
	// `[]any` of `{"id": "<id>"}` maps so the iteration below is
	// unchanged. Unblocks #250 (legacy parser deletion).
	result, err := c.engine.Execute(ctx, `queryActiveSpaceIds({})`)
	if err != nil {
		return err
	}
	data, err := extractDataFromResult(result)
	if err != nil {
		return err
	}
	var items []any
	switch v := data.(type) {
	case []any:
		items = v
	case map[string]any:
		items = []any{v}
	default:
		return fmt.Errorf("unexpected spaces data type: %T", data)
	}
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		spaceId := strings.TrimSpace(asString(m["id"]))
		if spaceId == "" {
			continue
		}
		_ = c.recomputeAndUpsertSpaceContext(ctx, spaceId)
	}
	return nil
}
