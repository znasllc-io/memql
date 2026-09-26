package memql

import (
	"context"
	"strings"
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
			history = append(history, map[string]any{"role": "assistant", "content": turn.Answer})
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
func (e *MemQLEngine) reconcileAskRuns(ctx context.Context, transcript *askTranscript) {
	for index := range transcript.Turns {
		turn := &transcript.Turns[index]
		if turn.RunID == "" || turn.State == "done" {
			continue
		}
		rows, err := e.workRows(ctx, "workRunForOwner", turn.RunID)
		if err != nil || len(rows) != 1 || rows[0]["status"] != "succeeded" {
			continue
		}
		answer, err := e.followWorkRun(ctx, turn.RunID, nil, nil)
		if err != nil {
			continue
		}
		turn.Answer, turn.State, turn.Error = answer, "done", ""
	}
}
