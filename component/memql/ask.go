package memql

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/id"
)

type AskTurn struct {
	VoiceInterrupted bool        `json:"voiceInterrupted,omitempty"`
	ID               string      `json:"id"`
	Prompt           string      `json:"prompt"`
	Answer           string      `json:"answer"`
	Context          string      `json:"context,omitempty"`
	State            string      `json:"state"`
	StartedAt        time.Time   `json:"startedAt"`
	EndedAt          *time.Time  `json:"endedAt,omitempty"`
	Activity         []WorkEvent `json:"activity"`
	Error            string      `json:"error,omitempty"`
	GoalID           string      `json:"goalId,omitempty"`
	RunID            string      `json:"runId,omitempty"`
}

type askTranscript struct {
	Turns []AskTurn `json:"turns"`
}

// RunAsk is shared by text and live voice. Only the server can append an
// assistant message. The lock spans replicas and rejects overlapping turns;
// a lost request is never automatically re-executed with its side effects.
func (e *MemQLEngine) RunAsk(ctx context.Context, conversationID, turnID, prompt, pageContext string, onText func(string), onEvent func(WorkEvent)) (answer string, err error) {
	if subject, ok := auth.SubjectFromContext(ctx); !ok || !auth.CapableFor(ctx, subject, "read", "app:ask") {
		return "", fmt.Errorf("Ask requires a signed-in person")
	}
	if len(prompt) == 0 || len(prompt) > 32000 || len(pageContext) > 24000 {
		return "", fmt.Errorf("Ask message or context exceeds its limit")
	}
	if err := id.ValidateShortId("v1:os:askConversation", conversationID); err != nil {
		return "", fmt.Errorf("invalid conversation id")
	}
	if turnID == "" || len(turnID) > 160 {
		return "", fmt.Errorf("invalid turn id")
	}
	// Verify ownership before reserving capacity, then re-read under the lock.
	if _, err = e.askRead(ctx, conversationID); err != nil {
		return "", err
	}
	release, err := e.lockAskConversation(ctx, conversationID)
	if err != nil {
		return "", err
	}
	defer release()
	row, err := e.askRead(ctx, conversationID)
	if err != nil {
		return "", err
	}
	var transcript askTranscript
	raw, _ := json.Marshal(row["transcript"])
	if err = json.Unmarshal(raw, &transcript); err != nil {
		return "", fmt.Errorf("conversation transcript is unreadable")
	}
	resumeIndex := -1
	for index, prior := range transcript.Turns {
		if prior.ID == turnID {
			if prior.State == "done" {
				if onText != nil {
					onText(prior.Answer)
				}
				return prior.Answer, nil
			}
			if prior.RunID != "" {
				resumeIndex = index
				break
			}
			return "", fmt.Errorf("this turn already started; inspect its activity before trying again")
		}
	}
	// Preserve interrupted turns as evidence rather than replaying operations.
	for i := range transcript.Turns {
		if transcript.Turns[i].State == "streaming" {
			transcript.Turns[i].State = "interrupted"
		}
	}
	title, _ := row["title"].(string)
	if len(transcript.Turns) == 0 {
		runes := []rune(strings.Join(strings.Fields(prompt), " "))
		title = string(runes[:min(80, len(runes))])
	}
	e.reconcileAskRuns(ctx, &transcript)
	history := askConversationMessages(transcript.Turns)
	if resumeIndex < 0 {
		transcript.Turns = append(transcript.Turns, AskTurn{ID: turnID, Prompt: prompt, Context: pageContext, State: "streaming", StartedAt: time.Now().UTC(), Activity: []WorkEvent{}})
		resumeIndex = len(transcript.Turns) - 1
	}
	turn := &transcript.Turns[resumeIndex]
	turn.State, turn.Error, turn.Answer = "streaming", "", ""
	if err = e.askSave(ctx, conversationID, title, transcript); err != nil {
		return "", err
	}
	timingHistory := e.askTimingHistory(ctx)
	if len(timingHistory) == 0 {
		timingHistory = transcript.Turns[:len(transcript.Turns)-1]
	}
	var mu sync.Mutex
	emit := func(event WorkEvent) error {
		mu.Lock()
		defer mu.Unlock()
		if event.ID == "" {
			event.ID = id.NewShortId()
		}
		if event.At.IsZero() {
			event.At = time.Now().UTC()
		}
		if event.Kind == "model" && event.Phase == "running" && event.Call != nil {
			event.ExpectedMS, event.EstimateSource = askExpectedFor(timingHistory, *event.Call)
		}
		turn.Activity = append(turn.Activity, event)
		if err := e.askSave(ctx, conversationID, title, transcript); err != nil {
			return err
		}
		if onEvent != nil {
			onEvent(event)
		}
		return nil
	}
	defer func() {
		ended := time.Now().UTC()
		turn.EndedAt = &ended
		turn.State = "done"
		if err != nil {
			turn.State = "error"
			if ctx.Err() != nil {
				turn.State = "interrupted"
			}
			turn.Error = err.Error()
		}
		// Persist cancellation too, but do not give any model/tool a detached context.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if saveErr := e.askSave(saveCtx, conversationID, title, transcript); saveErr != nil {
			err = fmt.Errorf("conversation could not be saved: %w", saveErr)
		}
	}()

	// Ask is a conversation adapter over the same intake as Nexus/API goals.
	// Context is persisted with the run before the planner/agent replica sees it.
	if turn.RunID == "" {
		call, callErr := parser.RenderCall("work.createGoal", map[string]any{
			"statement": prompt, "requestedVia": "ask",
			"input": map[string]any{"conversation": map[string]any{
				"id": conversationID, "turnId": turnID, "messages": history, "pageContext": pageContext,
			}},
		})
		if callErr != nil {
			return "", callErr
		}
		result, callErr := e.Execute(ctx, "builtin "+call)
		if callErr != nil {
			return "", callErr
		}
		rows := MaterializeRows(result.OutputPayload())
		if len(rows) != 1 {
			return "", fmt.Errorf("work intake returned no run")
		}
		turn.GoalID, _ = rows[0]["goalId"].(string)
		turn.RunID, _ = rows[0]["runId"].(string)
		if turn.GoalID == "" || turn.RunID == "" {
			return "", fmt.Errorf("work intake returned no run identity")
		}
		if err = e.askSave(ctx, conversationID, title, transcript); err != nil {
			return "", err
		}
	}
	if err = emit(WorkEvent{ID: "work:" + turn.RunID, Kind: "run", Phase: "running", Name: turn.RunID, Arguments: map[string]any{"goalId": turn.GoalID, "runId": turn.RunID}}); err != nil {
		return "", err
	}
	answer, err = e.followWorkRun(ctx, turn.RunID, func(text string) {
		mu.Lock()
		turn.Answer += text
		mu.Unlock()
		if onText != nil {
			onText(text)
		}
	}, emit)
	if err == nil {
		err = emit(WorkEvent{ID: "work:" + turn.RunID, Kind: "run", Phase: "completed", Name: turn.RunID, Arguments: map[string]any{"goalId": turn.GoalID, "runId": turn.RunID}})
	}
	// A disconnected viewer stops observing; the durable run retains its
	// receipts and is not recreated. Explicit cancellation uses cancelGoal.
	if turn.Answer == "" {
		turn.Answer = answer
	}
	return answer, err
}

func askExpected(turns []AskTurn, provider, model string) (int64, string) {
	return askExpectedFor(turns, airoute.CallObservation{Provider: provider, Model: model})
}

func askExpectedFor(turns []AskTurn, call airoute.CallObservation) (int64, string) {
	provider, model := call.Provider, call.Model
	var samples []int64
	for _, turn := range turns {
		for _, event := range turn.Activity {
			if event.Kind == "model" && event.Phase == "completed" && event.Provider == provider && event.Model == model && event.ElapsedMS > 0 {
				if call.Modality != "" && (event.Call == nil || event.Call.Modality != call.Modality) {
					continue
				}
				if call.ExecutionSurface != "" && (event.Call == nil || event.Call.ExecutionSurface != call.ExecutionSurface) {
					continue
				}
				samples = append(samples, event.ElapsedMS)
			}
		}
	}
	if len(samples) > 0 {
		samples = samples[max(0, len(samples)-20):]
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		return samples[len(samples)*3/4], "recent calls"
	}
	if strings.HasPrefix(provider, "fleet:") {
		return 60000, "initial estimate"
	}
	return 15000, "initial estimate"
}

// The same actor-scoped graph query runs on every replica. Timing evidence is
// private to the caller and survives a process restart; it never grants access.
func (e *MemQLEngine) askTimingHistory(ctx context.Context) []AskTurn {
	result, err := e.Execute(ctx, "query askRecentTimingHistory()")
	if err != nil {
		return nil
	}
	var turns []AskTurn
	rows := MaterializeRows(result.OutputPayload())
	for i := len(rows) - 1; i >= 0; i-- {
		raw, err := json.Marshal(rows[i]["transcript"])
		if err != nil {
			continue
		}
		var transcript askTranscript
		if json.Unmarshal(raw, &transcript) == nil {
			turns = append(turns, transcript.Turns...)
		}
	}
	return turns
}

func (e *MemQLEngine) askRead(ctx context.Context, conversationID string) (map[string]any, error) {
	call, err := parser.RenderCall("askConversationById", map[string]any{"conversationId": conversationID})
	if err != nil {
		return nil, err
	}
	result, err := e.Execute(ctx, call)
	if err != nil {
		return nil, err
	}
	rows := MaterializeRows(result.OutputPayload())
	if len(rows) != 1 {
		return nil, fmt.Errorf("conversation is unavailable")
	}
	return rows[0], nil
}

func (e *MemQLEngine) askSave(ctx context.Context, conversationID, title string, transcript askTranscript) error {
	raw, err := json.Marshal(transcript)
	if err != nil {
		return err
	}
	var document map[string]any
	if err = json.Unmarshal(raw, &document); err != nil {
		return err
	}
	call, err := parser.RenderCall("saveAskConversation", map[string]any{"id": conversationID, "title": title, "transcript": document})
	if err != nil {
		return err
	}
	_, err = e.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
	return err
}

func (e *MemQLEngine) lockAskConversation(ctx context.Context, conversationID string) (func(), error) {
	return e.lockAskConversationKind(ctx, conversationID, "turn")
}
func (e *MemQLEngine) lockAskConversationKind(ctx context.Context, conversationID, kind string) (func(), error) {
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("conversation storage is unavailable")
	}
	conn, err := db.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	_, bareID, err := id.ParseNodeId(conversationID)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte("ask:" + kind + ":" + bareID))
	key := int64(hash.Sum64())
	var held bool
	if err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&held); err != nil || !held {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = conn.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("MemQL is already working in this conversation")
	}
	return func() {
		release, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(release, "SELECT pg_advisory_unlock($1)", key); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}, nil
}
