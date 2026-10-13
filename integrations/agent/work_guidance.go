package agent

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

type workGuidanceEngine interface {
	ReadWorkGuidance(context.Context) ([]memql.WorkGuidance, error)
	AcknowledgeWorkGuidance(context.Context, []string) error
}

type workGuidanceCursor struct {
	seen      map[string]bool
	reasoning []string
}

// Read at every model/tool boundary, rather than once when a long-lived turn
// starts. Runtime ownership is reconstructed on the executing replica.
func (r *Replier) receiveWorkGuidance(ctx context.Context, cursor *workGuidanceCursor, messages []common.ChatMessage, unstarted []common.ToolCall) ([]common.ChatMessage, bool, error) {
	if !isOwnedWorkExecution(ctx) {
		return messages, false, nil
	}
	engine, ok := r.engine.(workGuidanceEngine)
	if !ok {
		return messages, false, nil
	}
	rows, err := engine.ReadWorkGuidance(ctx)
	if err != nil {
		return nil, false, err
	}
	var fresh []memql.WorkGuidance
	if cursor.seen == nil {
		cursor.seen = map[string]bool{}
	}
	for _, row := range rows {
		if !cursor.seen[row.ID] {
			fresh = append(fresh, row)
		}
	}
	if len(fresh) == 0 {
		return messages, false, nil
	}
	// Every proposed call still needs a paired result. Completed siblings keep
	// their actual results; only calls not yet dispatched are cancelled.
	for _, call := range unstarted {
		messages = append(messages, common.ChatMessage{Role: "tool", ToolCallId: call.ID, Name: call.Name, Content: `{"status":"not_started","reason":"New human guidance arrived before this call started. Reconsider this action using that guidance; completed tool results remain authoritative."}`})
	}
	var human strings.Builder
	human.WriteString("Guidance from the person for this existing work:\n")
	for index, row := range fresh {
		if index > 0 {
			human.WriteString("\n\nNext human message:\n")
		}
		human.WriteString(row.Prompt)
		cursor.seen[row.ID] = true
		cursor.reasoning = append(cursor.reasoning, row.ID)
	}
	// Keep a newly received batch together as the newest human correction;
	// compaction must not summarize an earlier member before first reasoning.
	messages = append(messages, common.ChatMessage{Role: "user", Content: human.String()})
	return messages, true, nil
}

func (r *Replier) acknowledgeWorkGuidance(ctx context.Context, cursor *workGuidanceCursor) error {
	if len(cursor.reasoning) == 0 {
		return nil
	}
	engine, ok := r.engine.(workGuidanceEngine)
	if !ok {
		return nil
	}
	if err := engine.AcknowledgeWorkGuidance(ctx, cursor.reasoning); err != nil {
		return err
	}
	cursor.reasoning = nil
	return nil
}
