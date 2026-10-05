package memql

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestWorkObserverReportsTerminalRunEvidenceFromAnotherReplica(t *testing.T) {
	for _, tc := range []struct{ status, phase string }{{"failed", "failed"}, {"abandoned", "failed"}, {"cancelled", "cancelled"}, {"waiting", "waiting"}} {
		t.Run(tc.status, func(t *testing.T) {
			read := func(_ context.Context, query, _ string) ([]map[string]any, error) {
				if query == "workRunForOwner" {
					return []map[string]any{{"status": tc.status, "errorMessage": "model deadline exceeded", "waitingOn": map[string]any{"kind": "budget"}}}, nil
				}
				return nil, nil
			}
			var events []WorkEvent
			_, err := followWorkRun(context.Background(), "remote-run", read, nil, func(e WorkEvent) error { events = append(events, e); return nil }, time.Millisecond)
			require.Error(t, err)
			require.Len(t, events, 1)
			require.Equal(t, "work:remote-run", events[0].ID)
			require.Equal(t, tc.phase, events[0].Phase)
			require.NotEmpty(t, events[0].Error)
		})
	}
}

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
		text, phase := "An unfinished draft", "streaming"
		if polls > 1 {
			text, phase = "Hello again", "completed"
		}
		raw, _ := json.Marshal(WorkEvent{ID: "response", Kind: "response", Phase: phase, Text: text, At: time.Now()})
		var event map[string]any
		_ = json.Unmarshal(raw, &event)
		return []map[string]any{{"id": "observation", "data": map[string]any{"execution": event}}}, nil
	}
	var text string
	answer, err := followWorkRun(context.Background(), "remote-run", read, func(delta string) {
		require.Equal(t, 2, polls, "uncommitted drafts must not be delivered")
		text += delta
	}, nil, time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, "Hello again", answer)
	require.Equal(t, answer, text, "the final response replaces the draft, even without a shared prefix")
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

func TestWorkObserverDeliversPersistedBuiltinFileReceipt(t *testing.T) {
	// The journal crossed a replica boundary, so this is decoded JSON, not a
	// map[string]MemoryNode. "done" is the step concept's terminal spelling.
	steps := []map[string]any{{"status": "done", "result": map[string]any{"value": map[string]any{
		"receipt": map[string]any{"id": "receipt", "concept": "v1:compose:result", "payload": map[string]any{
			"outputFileId": "v1:library:file:birds", "name": "birds.md", "sha256": "verified-hash",
		}},
	}}}}
	read := func(_ context.Context, name, _ string) ([]map[string]any, error) {
		switch name {
		case "workRunForOwner":
			return []map[string]any{{"status": "succeeded"}}, nil
		case "workStepsForOwnerRun":
			return steps, nil
		}
		return nil, nil
	}
	var events []WorkEvent
	answer, err := followWorkRun(context.Background(), "run", read, nil, func(event WorkEvent) error {
		events = append(events, event)
		return nil
	}, time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, "Created in Files: birds.md.", answer)
	require.Len(t, events, 1)
	require.Equal(t, "artifact", events[0].Kind)
	require.Equal(t, "birds", events[0].Arguments["fileId"])
	steps[0]["status"] = "failed"
	require.Empty(t, workResultFiles(steps), "a failed step is not a delivery receipt")
}

func TestSlowClassificationReleasesTheComposerWithoutResubmitting(t *testing.T) {
	read := func(_ context.Context, name, _ string) ([]map[string]any, error) {
		if name == "workRunForOwner" {
			return []map[string]any{{"status": "compiling", "startedAt": time.Now().Add(-20 * time.Second).Format(time.RFC3339Nano)}}, nil
		}
		return nil, nil
	}
	ctx := context.WithValue(context.Background(), workFollowModeKey{}, followBackground)
	_, err := followWorkRun(ctx, "run", read, nil, nil, time.Millisecond)
	var pending *workPending
	require.ErrorAs(t, err, &pending)
	require.False(t, pending.Waiting)
}

func TestCapabilityDiscoveryRanksRelevantPartialMatches(t *testing.T) {
	require.Greater(t, workCapabilityScore("memory search conversations", "work.workSearchConversations", "work.worksearchconversations private saved messages"), 0)
	require.Greater(t, workCapabilityScore("workSearchConversations", "work.workSearchConversations", "work.worksearchconversations"), workCapabilityScore("workSearchConversations", "other", "mentions worksearchconversations"))
	require.Zero(t, workCapabilityScore("invoices", "work.workSearchConversations", "saved messages"))
}

func TestWorkObserverDoesNotDeliverDraftsWhilePendingOrFailed(t *testing.T) {
	for _, status := range []string{"running", "waiting", "failed", "abandoned", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			read := func(_ context.Context, name, _ string) ([]map[string]any, error) {
				if name == "workRunForOwner" {
					return []map[string]any{{"status": status, "waitingOn": map[string]any{"kind": "feedback"}}}, nil
				}
				return []map[string]any{
					responseObservation("draft", "streaming", "I don't know yet.", time.Now()),
					responseObservation("earlier-step", "completed", "An intermediate result.", time.Now()),
				}, nil
			}
			var delivered string
			ctx := context.WithValue(context.Background(), workFollowModeKey{}, followSnapshot)
			answer, err := followWorkRun(ctx, "run", read, func(text string) { delivered += text }, nil, time.Millisecond)
			require.Error(t, err)
			require.Empty(t, answer)
			require.Empty(t, delivered, "a completed model call alone does not complete the work")
		})
	}
}

func responseObservation(id, phase, text string, at time.Time) map[string]any {
	return map[string]any{"id": id, "data": map[string]any{"execution": map[string]any{
		"id": id, "kind": "response", "phase": phase, "text": text, "at": at.Format(time.RFC3339Nano),
	}}}
}

func responseStep(seq int, status, text string) map[string]any {
	return map[string]any{"seq": seq, "status": status, "result": map[string]any{"value": map[string]any{
		"receipt": map[string]any{"id": "receipt", "payload": map[string]any{"reply": text}},
	}}}
}

func TestWorkObserverUsesSuccessfulJournalResultAfterHumanResume(t *testing.T) {
	now := time.Now().UTC()
	const acknowledged = "Recorded: the synthetic project launches in November."
	read := func(_ context.Context, name, _ string) ([]map[string]any, error) {
		switch name {
		case "workRunForOwner":
			return []map[string]any{{"status": "succeeded"}}, nil
		case "workStepsForOwnerRun":
			// Unordered query, failed later attempt, and an earlier completed
			// research step. Only the final successful step is the answer.
			return []map[string]any{responseStep(2, "done", acknowledged), responseStep(3, "failed", "Failed attempt"), responseStep(1, "done", "Research draft")}, nil
		}
		return []map[string]any{
			responseObservation("resumed", "completed", acknowledged, now),
			responseObservation("old-attempt", "completed", "An obsolete answer.", now.Add(time.Minute)),
			responseObservation("before-question", "streaming", "I don't know the launch month.", now.Add(2*time.Minute)),
		}, nil
	}
	var delivered []string
	answer, err := followWorkRun(context.Background(), "run", read, func(text string) { delivered = append(delivered, text) }, nil, time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, acknowledged, answer)
	require.Equal(t, []string{acknowledged}, delivered)
}

func TestWorkObserverCompletedResponseReplacesEarlierAttemptsWithoutJournalReply(t *testing.T) {
	now := time.Now().UTC()
	read := func(_ context.Context, name, _ string) ([]map[string]any, error) {
		if name == "workRunForOwner" {
			return []map[string]any{{"status": "succeeded"}}, nil
		}
		if name == "workStepsForOwnerRun" {
			return nil, nil
		}
		return []map[string]any{
			responseObservation("earlier", "completed", "Earlier attempt.", now),
			responseObservation("latest", "completed", "Final answer.", now.Add(time.Second)),
			responseObservation("draft", "streaming", "Unfinished.", now.Add(2*time.Second)),
		}, nil
	}
	answer, err := followWorkRun(context.Background(), "run", read, nil, nil, time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, "Final answer.", answer)
}
