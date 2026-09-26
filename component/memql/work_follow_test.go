package memql

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestWorkObserverSurvivesReplicaHopAndAutomaticRecovery(t *testing.T) {
	polls := 0
	read := func(_ context.Context, name, run string) ([]map[string]any, error) {
		require.Equal(t, "remote-run", run)
		if name == "workRunForOwner" {
			polls++
			if polls == 1 {
				return []map[string]any{{"status": "waiting", "waitingOn": map[string]any{"kind": "retry"}}}, nil
			}
			return []map[string]any{{"status": "succeeded"}}, nil
		}
		text := "Hello"
		if polls > 1 {
			text = "Hello again"
		}
		raw, _ := json.Marshal(WorkEvent{ID: "response", Kind: "response", Text: text, At: time.Now()})
		var event map[string]any
		_ = json.Unmarshal(raw, &event)
		return []map[string]any{{"id": "observation", "data": map[string]any{"execution": event}}}, nil
	}
	var text string
	answer, err := followWorkRun(context.Background(), "remote-run", read, func(delta string) { text += delta }, nil, time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, "Hello again", answer)
	require.Equal(t, answer, text, "cumulative snapshots must not duplicate the prefix")
	require.Equal(t, 2, polls)
}

func TestWorkObserverDisconnectDoesNotSubmitOrReplay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	read := func(_ context.Context, name, _ string) ([]map[string]any, error) {
		if name == "workRunForOwner" {
			return []map[string]any{{"status": "running"}}, nil
		}
		return nil, nil
	}
	_, err := followWorkRun(ctx, "run", read, nil, nil, time.Millisecond)
	require.True(t, errors.Is(err, context.Canceled))
}

func TestAskHistoryKeepsRealMessagesBeyondOldCutoff(t *testing.T) {
	turns := []AskTurn{{Prompt: "The project is CNAS", Answer: "Recorded", State: "done"}, {Prompt: "Voice message", Error: "context canceled", State: "error"}}
	for range 40 {
		turns = append(turns, AskTurn{Prompt: "Continue", Answer: "Okay", State: "done"})
	}
	messages := askConversationMessages(turns)
	require.Contains(t, messages[0]["content"], "CNAS")
	require.NotContains(t, messages[2]["content"], "Voice message")
	require.Len(t, messages, 82)
}
