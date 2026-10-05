package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
)

// Only a typed information request becomes an Ask question. Failure recovery
// and permission approvals keep their own action contracts.
func askFeedbackProjection(row map[string]any) (*AskQuestion, *AskTurn) {
	subject, _ := row["subject"].(map[string]any)
	kind, _ := subject["kind"].(string)
	if row["kind"] != "feedback" || (kind != "text" && kind != "choice" && kind != "multi") {
		return nil, nil
	}
	raw, _ := json.Marshal(row["options"])
	options := []map[string]any{}
	_ = json.Unmarshal(raw, &options)
	text, _ := row["question"].(string)
	id := BareShortId(fmt.Sprint(row["id"]))
	decision, _ := row["decision"].(string)
	if decision == "" {
		if until, ok := askTimestamp(row["expiresAt"]); ok && !time.Now().Before(until) {
			return nil, nil
		}
		return &AskQuestion{ID: id, Text: text, Kind: kind, Options: options}, nil
	}
	if decision != "answered" {
		return nil, nil
	}
	answer, _ := row["answer"].(map[string]any)
	reply, _ := answer["text"].(string)
	if strings.TrimSpace(reply) == "" {
		selected := map[string]bool{}
		if value, ok := answer["value"].(string); ok {
			selected[value] = true
		}
		if values, ok := answer["values"].([]any); ok {
			for _, value := range values {
				selected[fmt.Sprint(value)] = true
			}
		}
		labels := []string{}
		for _, option := range options {
			if selected[fmt.Sprint(option["value"])] {
				labels = append(labels, fmt.Sprint(option["label"]))
			}
		}
		reply = strings.Join(labels, ", ")
	}
	if strings.TrimSpace(reply) == "" {
		return nil, nil
	}
	at, ok := askTimestamp(row["decidedAt"])
	if !ok {
		return nil, nil
	}
	return nil, &AskTurn{ID: "feedback-" + id, Prompt: "In reply to “" + text + "”:\n" + reply, State: "done", StartedAt: at, EndedAt: &at, AnswerOnly: true, Activity: []WorkEvent{}}
}

// Open the originating conversation or create one deterministic conversation
// for Nexus-originated work. This never starts another goal or replays a run.
func (e *MemQLEngine) askWorkConversationBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	subject, ok := auth.SubjectFromContext(ctx)
	if !ok || !auth.CapableFor(ctx, subject, "read", "app:ask") {
		return nil, fmt.Errorf("sign in to open Ask")
	}
	ctx = ContextWithFreshRead(ctx)
	runID := stringArg(args, "runId")
	runs, err := e.workRows(ctx, "workRunForOwner", runID)
	if err != nil || len(runs) != 1 {
		return nil, fmt.Errorf("work is unavailable")
	}
	run := runs[0]
	input, _ := run["input"].(map[string]any)
	conversation, _ := input["conversation"].(map[string]any)
	if id, _ := conversation["id"].(string); id != "" {
		row, err := e.askRead(ctx, id)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(row)
		return []memorynodes.MemoryNode{{ID: fmt.Sprint(row["id"]), Payload: raw}}, err
	}
	call, _ := parser.RenderCall("workGoalForOwner", map[string]any{"goalId": run["goalId"]})
	result, err := e.Execute(ctx, "query "+call)
	if err != nil {
		return nil, err
	}
	goals := MaterializeRows(result.OutputPayload())
	if len(goals) != 1 {
		return nil, fmt.Errorf("goal is unavailable")
	}
	statement, _ := goals[0]["statement"].(string)
	title := boundedMemoryText(statement, 100)
	call, _ = parser.RenderCall("createAskConversation", map[string]any{"requestId": "work-" + BareShortId(runID), "title": title})
	result, err = e.Execute(ctx, "mutation "+call)
	if err != nil {
		return nil, err
	}
	rows := MaterializeRows(result.OutputPayload())
	if len(rows) != 1 {
		return nil, fmt.Errorf("conversation could not be opened")
	}
	id := fmt.Sprint(rows[0]["id"])
	unlock, err := e.lockAskConversationKind(ctx, id, "write")
	if err != nil {
		return nil, err
	}
	defer unlock()
	row, err := e.askRead(ContextWithFreshRead(ctx), id)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(row["transcript"])
	var transcript askTranscript
	if err = json.Unmarshal(raw, &transcript); err != nil {
		return nil, err
	}
	if len(transcript.Turns) == 0 {
		at, ok := askTimestamp(run["startedAt"])
		if !ok {
			at = time.Now().UTC()
		}
		transcript.Turns = []AskTurn{{ID: "work-" + BareShortId(runID), Prompt: statement, RunID: BareShortId(runID), GoalID: BareShortId(fmt.Sprint(run["goalId"])), State: "queued", StartedAt: at, Background: true, Activity: []WorkEvent{}}}
		if err = e.askSave(ctx, id, title, transcript); err != nil {
			return nil, err
		}
	}
	raw, err = json.Marshal(map[string]any{"id": BareShortId(id), "title": title})
	return []memorynodes.MemoryNode{{ID: id, Payload: raw}}, err
}
