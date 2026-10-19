package memql

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// A failed transcription is execution evidence, not a user's utterance.
// Keep genuine earlier messages and action receipts; never invent content for
// an unheard voice fragment. The shared agent manages its context budget.
func askConversationMessages(turns []AskTurn) []map[string]any {
	history := []map[string]any{}
	for _, turn := range turns {
		if strings.TrimSpace(turn.Prompt) == "" || (turn.Prompt == "Voice message" && turn.State == "error") {
			continue
		}
		history = append(history, map[string]any{"role": "user", "content": turn.Prompt})
		if turn.Answer != "" {
			answer := turn.Answer
			if turn.ApprovalID != "" {
				answer = "[Read-only approval discussion; this is advice, not a human decision or an executed action.]\n" + answer
			}
			history = append(history, map[string]any{"role": "assistant", "content": answer})
		}
		if turn.VoiceInterrupted {
			history = append(history, map[string]any{"role": "user", "content": "[Audio delivery of the preceding reply was interrupted. The generated text may not have been heard in full.]"})
		}
		evidence := []string{}
		for _, event := range turn.Activity {
			if event.Kind == "action" && event.Phase != "running" {
				evidence = append(evidence, event.Name+": "+event.Phase)
			}
		}
		if len(evidence) > 0 || turn.State != "done" {
			history = append(history, map[string]any{"role": "user", "content": "[Recorded execution status: " + turn.State + ". " + strings.Join(evidence, "; ") + ". Inspect the existing run before repeating an operation. Run: " + turn.RunID + "]"})
		}
	}
	return history
}

// A voice interruption stops delivery while the shared run keeps its receipts.
// Bring completed results back into history before answering the next turn.
// RunAsk saves this transcript, so routed turns keep what routing recorded:
// their projection over the runs is the snapshot's to show.
func (e *MemQLEngine) reconcileAskRuns(ctx context.Context, transcript *askTranscript) {
	_ = e.refreshAskTranscript(ctx, transcript, false)
}

// Persist actual receipts beside conversational prose before the planner hop.
// Only the saved, owner-authorized conversation can select the prior runs.
func (e *MemQLEngine) askConversationInput(ctx context.Context, conversationID, turnID, pageContext string, history []map[string]any) map[string]any {
	conversation := map[string]any{"id": conversationID, "turnId": turnID, "messages": history, "pageContext": pageContext, "capturedAt": time.Now().UTC().Format(time.RFC3339Nano)}
	if len(history) == 0 {
		return conversation
	}
	sources, err := e.workSourceContextBuiltin(ctx, map[string]any{"conversationId": conversationID, "turnId": turnID}, 0)
	var evidence map[string]any
	if err == nil && len(sources) == 1 {
		err = json.Unmarshal(sources[0].Payload, &evidence)
	}
	if err != nil || evidence == nil {
		conversation["sourceContext"] = map[string]any{"limitations": []string{"Earlier source receipts could not be read. Prior answer text alone is not verification."}}
	} else {
		// Viewer context has its own fresh read on each receiving replica.
		delete(evidence, "currentProfile")
		conversation["sourceContext"] = evidence
	}
	return conversation
}
