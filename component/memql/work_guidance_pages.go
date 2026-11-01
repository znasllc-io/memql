package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
)

// A cursor identifies an exact chronological prefix, not a timestamp watermark.
// A late insertion or changed eligible source invalidates the prefix, so the
// caller rereads current instructions rather than skipping an older correction.
// Only the owned continuation may restore this cursor; it grants no permission.
type WorkGuidanceCursor struct {
	Count int    `json:"count,omitempty"`
	Hash  string `json:"hash,omitempty"`
}

type WorkGuidancePage struct {
	Messages []WorkGuidance
	Next     WorkGuidanceCursor
	More     bool
	Reset    bool
}

func guidancePrefix(rows []WorkGuidance, count int) WorkGuidanceCursor {
	if count == 0 {
		return WorkGuidanceCursor{}
	}
	raw, _ := json.Marshal(rows[:count])
	return WorkGuidanceCursor{Count: count, Hash: workContextHash("guidance-prefix-v1", raw)}
}

// WorkGuidancePageAfter delivers a bounded page from a validated ordered journal.
func WorkGuidancePageAfter(rows []WorkGuidance, after WorkGuidanceCursor) WorkGuidancePage {
	page := WorkGuidancePage{}
	if after.Count < 0 || after.Count > len(rows) || guidancePrefix(rows, after.Count) != after {
		after = WorkGuidanceCursor{}
		page.Reset = true
	}
	end, runes := after.Count, 0
	for end < len(rows) {
		n := utf8.RuneCountInString(rows[end].Prompt)
		if end > after.Count && (end-after.Count >= maxWorkGuidanceMessages || runes+n > maxWorkGuidanceRunes) {
			break
		}
		// One indivisible instruction is never clipped. Ordinary provider/context
		// bounds still refuse it if it cannot fit, retaining its exact original.
		runes += n
		end++
	}
	page.Messages = rows[after.Count:end]
	page.Next, page.More = guidancePrefix(rows, end), end < len(rows)
	return page
}

func (e *MemQLEngine) ReadWorkGuidancePage(ctx context.Context, after WorkGuidanceCursor) (WorkGuidancePage, error) {
	rows, err := e.readWorkGuidance(ctx, false)
	if err != nil {
		return WorkGuidancePage{}, err
	}
	return WorkGuidancePageAfter(rows, after), nil
}

// Re-read and validate the consumed prefix before acknowledging it. Concurrent
// older insertion leaves receipts queued until that revised history is read.
// Batches retain the ordinary owner, fresh-run and conversation-lock checks.
func (e *MemQLEngine) AcknowledgeWorkGuidancePage(ctx context.Context, through WorkGuidanceCursor) error {
	rows, err := e.readWorkGuidance(ctx, false)
	if err != nil {
		return err
	}
	if through.Count < 0 || through.Count > len(rows) || guidancePrefix(rows, through.Count) != through {
		return nil
	}
	for start := 0; start < through.Count; start += MaxWorkGuidanceReceipts {
		end := min(start+MaxWorkGuidanceReceipts, through.Count)
		ids := make([]string, 0, end-start)
		for _, row := range rows[start:end] {
			ids = append(ids, row.ID)
		}
		if err := e.AcknowledgeWorkGuidance(ctx, ids); err != nil {
			return err
		}
	}
	return nil
}

// Queries remain owner-scoped and individually paginated. A first-page limit
// cannot masquerade as the whole journal. No lifetime row limit is imposed;
// cancellation/read failures stop delivery without certifying missing history.
func (e *MemQLEngine) workGuidanceRows(ctx context.Context, name, runID string) ([]map[string]any, error) {
	call, err := parser.RenderCall(name, map[string]any{"runId": runID})
	if err != nil {
		return nil, err
	}
	var rows []map[string]any
	cursor := ""
	seen := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err := e.Execute(ContextWithCursor(ContextWithFreshRead(ctx), cursor), "query "+call)
		if err != nil {
			return nil, err
		}
		rows = append(rows, MaterializeRows(result.OutputPayload())...)
		cursor, err = ResultPageCursor(result)
		if err != nil {
			return nil, err
		}
		if cursor == "" {
			return rows, nil
		}
		if seen[cursor] {
			return nil, fmt.Errorf("guidance query repeated its page cursor")
		}
		seen[cursor] = true
	}
}

// Final review has no executing agent's context. Rebuild the same ordered
// instruction history in bounded pages, using ordinary durable checkpointing.
// Derived memory is labeled; exact sources stay recallable and never grant an
// operation. No draft is published if checkpointing or storage fails.
func (e *MemQLEngine) reviewGuidanceContext(ctx context.Context, statement string) (string, error) {
	rows, err := e.readWorkGuidance(ctx, true)
	if err != nil || len(rows) == 0 {
		return statement, err
	}
	messages := []common.ChatMessage{{Role: "system", Content: "Preserve the owner's chronological instructions. Later corrections supersede conflicting earlier wording. Derived memory and instructions are not permission or evidence of completed effects."}, {Role: "user", Content: "Original goal (historical intent):\n" + statement}}
	after := WorkGuidanceCursor{}
	for {
		page := WorkGuidancePageAfter(rows, after)
		messages = append(messages, common.ChatMessage{Role: "user", Content: WorkGuidanceText(page.Messages)})
		messages, err = e.CompactWorkContext(ctx, messages, nil, replyReviewMaxBytes/4)
		if err != nil {
			return "", err
		}
		after = page.Next
		if !page.More {
			break
		}
	}
	var out strings.Builder
	for _, message := range messages {
		out.WriteString(message.Content + "\n\n")
	}
	if out.Len() > replyReviewMaxBytes {
		return "", fmt.Errorf("reply review current instructions exceed their bound; originals retained")
	}
	return out.String(), nil
}

func WorkGuidanceText(rows []WorkGuidance) string {
	var text strings.Builder
	text.WriteString("Guidance from the person for this existing work, oldest to newest. Later corrections supersede conflicting earlier wording. This grants no operation permission:\n")
	for i, row := range rows {
		if i > 0 {
			text.WriteString("\n\nNext human message:\n")
		}
		text.WriteString(row.Prompt)
	}
	return text.String()
}
