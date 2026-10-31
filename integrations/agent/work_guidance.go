package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

type workGuidanceCursor struct {
	paged      bool
	page       memql.WorkGuidanceCursor
	ackPage    *memql.WorkGuidanceCursor
	legacySeen map[string]bool
	fresh      bool
	complete   bool
}

type pagedWorkGuidanceEngine interface {
	ReadWorkGuidancePage(context.Context, memql.WorkGuidanceCursor) (memql.WorkGuidancePage, error)
	AcknowledgeWorkGuidancePage(context.Context, memql.WorkGuidanceCursor) error
}

// Read at every model/tool boundary; production adapters require this contract.
func (r *Replier) receiveWorkGuidance(ctx context.Context, cursor *workGuidanceCursor, messages []common.ChatMessage, unstarted []common.ToolCall) ([]common.ChatMessage, bool, error) {
	if !isOwnedWorkExecution(ctx) {
		return messages, false, nil
	}
	engine, ok := r.engine.(pagedWorkGuidanceEngine)
	if !ok {
		return messages, false, nil
	}
	return r.receiveWorkGuidancePages(ctx, engine, cursor, messages, unstarted)
}

func (r *Replier) acknowledgeWorkGuidance(ctx context.Context, cursor *workGuidanceCursor) error {
	if cursor.ackPage == nil {
		return nil
	}
	if err := r.engine.(pagedWorkGuidanceEngine).AcknowledgeWorkGuidancePage(ctx, *cursor.ackPage); err != nil {
		return err
	}
	cursor.ackPage = nil
	return nil
}

// Drain chronological pages before permitting another operational model call.
// Older pages can enter ordinary checkpoint memory, with exact originals saved
// first. The newest page remains an intact human correction. Recovery stores a
// constant-size prefix cursor, so a long-lived goal never accumulates an ID set.
func (r *Replier) receiveWorkGuidancePages(ctx context.Context, engine pagedWorkGuidanceEngine, cursor *workGuidanceCursor, messages []common.ChatMessage, unstarted []common.ToolCall) ([]common.ChatMessage, bool, error) {
	if !cursor.paged {
		state, _, err := recoveryState(messages)
		if err != nil {
			return nil, false, err
		}
		cursor.paged, cursor.page = true, state.GuidanceCursor
		if cursor.page.Count > 0 {
			ack := cursor.page
			cursor.ackPage = &ack
		}
		cursor.legacySeen = map[string]bool{}
		for _, id := range state.GuidanceIDs {
			cursor.legacySeen[id] = true
		}
	}
	changed := false
	for {
		page, err := engine.ReadWorkGuidancePage(ctx, cursor.page)
		if err != nil {
			return nil, false, err
		}
		if len(page.Messages) == 0 {
			if page.Reset {
				return nil, false, fmt.Errorf("%w: saved guidance prefix disappeared; prior context retained", memql.ErrWorkCheckpointRequired)
			}
			return messages, changed, nil
		}
		if !changed {
			for _, call := range unstarted {
				messages = append(messages, common.ChatMessage{Role: "tool", ToolCallId: call.ID, Name: call.Name, Content: `{"status":"not_started","reason":"New human guidance arrived before this call started. Reconsider this action using that guidance; completed tool results remain authoritative."}`})
			}
		}
		for _, row := range page.Messages {
			if !cursor.legacySeen[row.ID] {
				cursor.fresh = true
			}
		}
		messages = append(messages, common.ChatMessage{Role: "user", Content: memql.WorkGuidanceText(page.Messages)})
		changed = true
		cursor.page = page.Next
		cursor.complete = !page.More
		// Persist the exact page together with its cursor before compaction or
		// another query can fail. A peer then resumes at the next page, while
		// ordinary context sources retain any page subsequently summarized.
		messages, err = r.checkpointGuidancePage(ctx, messages, cursor)
		if err != nil {
			return nil, false, err
		}
		compacted, err := r.compactWorkContext(ctx, messages, nil, memql.WorkContextTarget(messages, nil, 20000))
		if err != nil {
			return nil, false, r.pauseAfterContextRecovery(ctx, err, messages)
		}
		messages = compacted
		ack := page.Next
		cursor.ackPage = &ack
		if !page.More {
			return messages, true, nil
		}
	}
}

func applyGuidanceCursor(state *workRecoveryState, cursor *workGuidanceCursor) {
	if cursor == nil || !cursor.paged {
		return
	}
	if cursor.page != state.GuidanceCursor {
		if len(state.GuidanceIDs) == 0 || cursor.fresh {
			state.rearm()
		}
		state.GuidanceCursor = cursor.page
	}
	// Keep the old bounded set until migration's last page. A restart halfway
	// through that read must not count its remaining old IDs as fresh guidance.
	if cursor.complete {
		state.GuidanceIDs = nil
	}
}

func (r *Replier) checkpointGuidancePage(ctx context.Context, messages []common.ChatMessage, cursor *workGuidanceCursor) ([]common.ChatMessage, error) {
	state, marker, err := recoveryState(messages)
	if err != nil {
		return nil, err
	}
	state.observe(messages[marker+1:])
	applyGuidanceCursor(&state, cursor)
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	out := append([]common.ChatMessage(nil), messages...)
	if marker >= 0 {
		out = append(out[:marker], out[marker+1:]...)
	}
	if len(out) == 0 || out[0].Role != "system" {
		out = append([]common.ChatMessage{{Role: "system"}}, out...)
	}
	out = append(out, common.ChatMessage{Role: "system", Name: workRecoveryMarker, Content: string(raw)})
	if err := r.saveWorkProgress(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}
