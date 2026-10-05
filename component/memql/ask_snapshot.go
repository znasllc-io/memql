package memql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// Merge one observer's receipt under a short database lock. Holding a copy of
// the whole transcript while another request runs must never erase that turn.
func (e *MemQLEngine) askSaveTurn(ctx context.Context, conversationID string, turn AskTurn) error {
	release, err := e.lockAskConversationKind(ctx, conversationID, "write")
	if err != nil {
		return err
	}
	defer release()
	ctx = ContextWithFreshRead(ctx)
	row, err := e.askRead(ctx, conversationID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(row["transcript"])
	if err != nil {
		return err
	}
	var transcript askTranscript
	if err := json.Unmarshal(raw, &transcript); err != nil {
		return err
	}
	for n := range transcript.Turns {
		if transcript.Turns[n].ID == turn.ID {
			transcript.Turns[n] = turn
			title, _ := row["title"].(string)
			return e.askSave(ctx, conversationID, title, transcript)
		}
	}
	return fmt.Errorf("the accepted conversation turn is unavailable")
}

// A read projection over durable runs. It does not start work or mutate a
// transcript: reconnecting on any replica reconstructs progress and results.
func (e *MemQLEngine) askConversationSnapshotBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = ContextWithFreshRead(ctx)
	row, err := e.askRead(ctx, stringArg(args, "conversationId"))
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(row["transcript"])
	if err != nil {
		return nil, err
	}
	var transcript askTranscript
	if err := json.Unmarshal(raw, &transcript); err != nil {
		return nil, err
	}
	if err := e.refreshAskRuns(ctx, &transcript); err != nil {
		return nil, err
	}
	row["transcript"] = transcript
	raw, err = json.Marshal(row)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: fmt.Sprint(row["id"]), Payload: raw}}, nil
}

func (e *MemQLEngine) refreshAskRuns(ctx context.Context, transcript *askTranscript) error {
	var answers []AskTurn
	seen := map[string]bool{}
	for _, turn := range transcript.Turns {
		seen[turn.ID] = true
	}
	for n := range transcript.Turns {
		turn := &transcript.Turns[n]
		if turn.RunID == "" {
			continue
		}
		runs, err := e.workRows(ctx, "workRunForOwner", turn.RunID)
		if err != nil {
			return err
		}
		if len(runs) != 1 {
			return fmt.Errorf("conversation work is unavailable")
		}
		run := runs[0]
		waiting, _ := run["waitingOn"].(map[string]any)
		questions, err := e.workRows(ctx, "workQuestionsForOwnerRun", turn.RunID)
		if err != nil {
			return err
		}
		turn.Question = nil
		for _, row := range questions {
			question, answer := askFeedbackProjection(row)
			if question != nil && run["status"] == "waiting" && BareShortId(fmt.Sprint(waiting["subject"])) == question.ID {
				turn.Question = question
			}
			if answer != nil && !seen[answer.ID] {
				answers = append(answers, *answer)
				seen[answer.ID] = true
			}
		}
		// The journal is authoritative even for a saved completed turn: an
		// earlier observer may have persisted drafts from a superseded attempt.
		outcome, _ := run["classification"].(map[string]any)
		if ack, _ := outcome["acknowledgement"].(string); ack != "" && (turn.State == "queued" || turn.State == "waiting" || turn.Acknowledgement != "") {
			turn.Acknowledgement = ack
		}
		if title, _ := outcome["workTitle"].(string); title != "" {
			turn.WorkTitle = title
		}
		if workload, _ := outcome["workload"].(string); workload != "" {
			turn.Workload = workload
		}
		answer, err := e.followWorkRun(context.WithValue(ctx, workFollowModeKey{}, followSnapshot), turn.RunID, nil, func(event WorkEvent) error {
			for index, prior := range turn.Activity {
				if prior.ID == event.ID {
					turn.Activity[index] = event
					return nil
				}
			}
			turn.Activity = append(turn.Activity, event)
			return nil
		})
		var pending *workPending
		switch {
		case errors.As(err, &pending):
			turn.Answer = turn.Acknowledgement
			turn.State, turn.Error, turn.EndedAt = "queued", "", nil
			if pending.Waiting {
				turn.State = "waiting"
			}
		case err != nil:
			if run["status"] != "failed" && run["status"] != "cancelled" && run["status"] != "abandoned" {
				return err
			}
			turn.State, turn.Error = "error", err.Error()
		default:
			turn.State, turn.Error = "done", ""
		}
		if turn.State != "waiting" {
			turn.Question = nil
		}
		if answer != "" && turn.State == "done" {
			turn.Answer = answer
		}
		if turn.State == "done" || turn.State == "error" {
			if ended, valid := askTimestamp(run["finishedAt"]); valid {
				turn.EndedAt = &ended
			}
		}
	}
	transcript.Turns = append(transcript.Turns, answers...)
	return nil
}
