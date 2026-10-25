package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

type workReplyReviewStore interface {
	ReadWorkReplyReview(context.Context) (memql.WorkReplyReviewCheckpoint, error)
	SaveWorkReplyReview(context.Context, memql.WorkReplyReviewBinding, json.RawMessage) error
}

// This private protocol record is never inserted into a model prompt. The
// completed result and terminal envelope remain withheld, with their exact
// source checkpoint, until review succeeds under current owner instructions.
type pendingWorkReply struct {
	Version           int                   `json:"version"`
	Result            *TurnResult           `json:"result"`
	TerminalID        string                `json:"terminalId"`
	TerminalArguments string                `json:"terminalArguments"`
	Messages          []common.ChatMessage  `json:"messages"`
	OutcomeFormat     *pendingOutcomeFormat `json:"outcomeFormat,omitempty"`
}

func decodePendingWorkReply(raw json.RawMessage) (*pendingWorkReply, error) {
	var pending pendingWorkReply
	if json.Unmarshal(raw, &pending) != nil || pending.Version != 1 || pending.Result == nil || pending.Result.Paused || (strings.TrimSpace(pending.Result.FinalText) == "" && pending.OutcomeFormat == nil) || len(pending.Messages) == 0 || (pending.TerminalID != "" && !json.Valid([]byte(pending.TerminalArguments))) {
		return nil, fmt.Errorf("unreadable pending reply review; completed production cannot be replayed")
	}
	if f := pending.OutcomeFormat; f != nil && (f.Attempts < 0 || f.Attempts > 1 || f.Input.SessionID == "" || len(f.Input.Messages) == 0 || (f.Done && f.Attempts != 1)) {
		return nil, fmt.Errorf("unreadable outcome-format checkpoint; production cannot be replayed")
	}
	if _, _, err := recoveryState(pending.Messages); err != nil {
		return nil, err
	}
	return &pending, nil
}

func clearConsumedWorkContinuation(ctx context.Context) context.Context {
	if run, ok := common.RunFromContext(ctx); ok && run.Continuation != nil {
		run.Continuation = nil
		return common.ContextWithRun(ctx, run)
	}
	return ctx
}

// A later operational checkpoint may advance recovery state and append source
// receipts. Never replace it with the older draft's context. A helper checkpoint
// or compacted history with a different source sequence cannot authorize fresh
// production here: retain both records and refuse that transition.
func replySourceExtends(previous, latest []common.ChatMessage) (compatible, added bool) {
	strip := func(messages []common.ChatMessage) []common.ChatMessage {
		out := make([]common.ChatMessage, 0, len(messages))
		for i, message := range messages {
			if (i == 0 && message.Role == "system") || (message.Role == "system" && message.Name == workRecoveryMarker) {
				continue
			}
			out = append(out, message)
		}
		return out
	}
	a, b := strip(previous), strip(latest)
	if len(b) < len(a) || recoveryDigest(a) != recoveryDigest(b[:len(a)]) {
		return false, false
	}
	return true, len(b) > len(a)
}

func savePendingWorkReply(ctx context.Context, store workReplyReviewStore, binding memql.WorkReplyReviewBinding, pending *pendingWorkReply) error {
	var raw json.RawMessage
	if pending != nil {
		encoded, err := json.Marshal(pending)
		if err != nil {
			return err
		}
		if _, err := decodePendingWorkReply(encoded); err != nil {
			return err
		}
		raw = encoded
	}
	if err := store.SaveWorkReplyReview(ctx, binding, raw); err != nil {
		return fmt.Errorf("%w: saving reply review stage: %w", memql.ErrWorkCheckpointRequired, err)
	}
	return nil
}
