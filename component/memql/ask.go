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
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/id"
)

// AskEvent is public execution evidence, never a model's private reasoning.
// Unknown token usage and cost remain absent, not misleading zeroes.
type AskEvent struct {
	ID             string                   `json:"id"`
	Kind           string                   `json:"kind"`
	Phase          string                   `json:"phase"`
	At             time.Time                `json:"at"`
	Provider       string                   `json:"provider,omitempty"`
	Model          string                   `json:"model,omitempty"`
	Name           string                   `json:"name,omitempty"`
	App            string                   `json:"app,omitempty"`
	Navigate       bool                     `json:"navigate,omitempty"`
	Arguments      map[string]any           `json:"arguments,omitempty"`
	ElapsedMS      int64                    `json:"elapsedMs,omitempty"`
	ExpectedMS     int64                    `json:"expectedMs,omitempty"`
	EstimateSource string                   `json:"estimateSource,omitempty"`
	Error          string                   `json:"error,omitempty"`
	Call           *airoute.CallObservation `json:"call,omitempty"`
}

type AskTurn struct {
	ID        string     `json:"id"`
	Prompt    string     `json:"prompt"`
	Answer    string     `json:"answer"`
	Context   string     `json:"context,omitempty"`
	State     string     `json:"state"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	Activity  []AskEvent `json:"activity"`
	Error     string     `json:"error,omitempty"`
}

type askTranscript struct {
	Turns []AskTurn `json:"turns"`
}
type askEventKey struct{}
type askEmitter func(AskEvent) error

func emitAsk(ctx context.Context, event AskEvent) error {
	if emit, ok := ctx.Value(askEventKey{}).(askEmitter); ok {
		return emit(event)
	}
	return nil
}

// RunAsk is shared by text and live voice. Only the server can append an
// assistant message. The lock spans replicas and rejects overlapping turns;
// a lost request is never automatically re-executed with its side effects.
func (e *MemQLEngine) RunAsk(ctx context.Context, conversationID, turnID, prompt, pageContext string, onText func(string), onEvent func(AskEvent)) (answer string, err error) {
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
	for _, prior := range transcript.Turns {
		if prior.ID == turnID {
			if prior.State == "done" {
				if onText != nil {
					onText(prior.Answer)
				}
				return prior.Answer, nil
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
	history := make([]map[string]any, 0, 50)
	for _, turn := range transcript.Turns[max(0, len(transcript.Turns)-24):] {
		history = append(history, map[string]any{"role": "user", "content": turn.Prompt})
		if turn.Answer != "" {
			history = append(history, map[string]any{"role": "assistant", "content": turn.Answer})
		}
		evidence := []string{}
		for _, event := range turn.Activity {
			if event.Kind == "action" && event.Phase != "running" {
				evidence = append(evidence, event.Name+": "+event.Phase)
			}
		}
		if len(evidence) > 0 || turn.State != "done" {
			history = append(history, map[string]any{"role": "user", "content": "[Recorded execution status for the preceding turn: " + turn.State + ". " + strings.Join(evidence, "; ") + ". Inspect current state before repeating an operation.]"})
		}
	}
	history = append(history, map[string]any{"role": "user", "content": prompt})
	transcript.Turns = append(transcript.Turns, AskTurn{ID: turnID, Prompt: prompt, Context: pageContext, State: "streaming", StartedAt: time.Now().UTC(), Activity: []AskEvent{}})
	turn := &transcript.Turns[len(transcript.Turns)-1]
	if err = e.askSave(ctx, conversationID, title, transcript); err != nil {
		return "", err
	}
	timingHistory := e.askTimingHistory(ctx)
	if len(timingHistory) == 0 {
		timingHistory = transcript.Turns[:len(transcript.Turns)-1]
	}
	var mu sync.Mutex
	emit := askEmitter(func(event AskEvent) error {
		mu.Lock()
		defer mu.Unlock()
		if event.ID == "" {
			event.ID = id.NewShortId()
		}
		event.At = time.Now().UTC()
		turn.Activity = append(turn.Activity, event)
		if saveErr := e.askSave(ctx, conversationID, title, transcript); saveErr != nil {
			return saveErr
		}
		if onEvent != nil {
			onEvent(event)
		}
		return nil
	})
	ctx = context.WithValue(WithActingAgentRole(ctx, "assistant"), askEventKey{}, emit)
	defer func() {
		ended := time.Now().UTC()
		turn.EndedAt = &ended
		turn.State = "done"
		if err != nil {
			turn.State = "error"
			turn.Error = err.Error()
		}
		// Persist cancellation too, but do not give any model/tool a detached context.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if saveErr := e.askSave(saveCtx, conversationID, title, transcript); saveErr != nil {
			err = fmt.Errorf("conversation could not be saved: %w", saveErr)
		}
	}()

	ctx, cancelCall := context.WithCancelCause(ctx)
	defer cancelCall(nil)
	ctx = airoute.WithObserver(ctx, func(call airoute.CallObservation) {
		event := AskEvent{ID: call.ID, Kind: "model", Phase: call.Phase, Provider: call.Provider, Model: call.Model, ElapsedMS: int64(call.ElapsedMS), Error: call.Error, Call: &call}
		if call.Phase == "running" {
			event.ExpectedMS, event.EstimateSource = askExpectedFor(timingHistory, call)
		}
		if saveErr := emit(event); saveErr != nil {
			cancelCall(fmt.Errorf("could not record model activity: %w", saveErr))
		}
	})
	answer, err = e.InvokeAIChatWithFilteredToolsOpts(ctx, "askMemql", map[string]any{"context": pageContext, "history": history}, []string{"os.askDiscover", "os.askExecuteCapability"}, &ToolLoopOptions{
		Sequential:    true,
		AllowTextOnly: true,
		RetryPolicy:   &ToolRetryPolicy{MaxAttempts: 1, PerCallTimeout: 90 * time.Second},
		ContextBudget: &ContextBudget{MaxTokens: 24000},
		OnText: func(text string) {
			mu.Lock()
			turn.Answer += text
			mu.Unlock()
			if onText != nil {
				onText(text)
			}
		},
	})
	if cause := context.Cause(ctx); cause != nil {
		err = cause
	}
	if turn.Answer == "" {
		turn.Answer = answer
		if onText != nil && answer != "" {
			onText(answer)
		}
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

func (e *MemQLEngine) askAllowed(ctx context.Context, fn *Function) bool {
	if fn == nil || !fn.Enabled || fn.ServerOnly || strings.HasPrefix(fn.Name, "ask") || strings.HasSuffix(fn.Name, "AskConversation") {
		return false
	}
	if fn.FunctionKind != "query" && fn.FunctionKind != "mutation" && fn.FunctionKind != "logic" && fn.FunctionKind != "builtin" {
		return false
	}
	if fn.ArgsSchema != nil {
		for _, field := range fn.ArgsSchema.Fields {
			if field.Secret {
				return false
			}
		}
	}
	if e.refuseBelowRequiredRank(ctx, fn, fn.Name) != nil || e.refuseBelowRequiredCapability(ctx, fn, fn.Name) != nil {
		return false
	}
	if app := askApp(fn); app != "" {
		subject, ok := auth.SubjectFromContext(ctx)
		if !ok || !auth.CapableFor(ctx, subject, "read", "app:"+app) {
			return false
		}
	}
	return true
}

func askApp(fn *Function) string {
	ns := ConstructNamespaceForOrigin(fn.Origin)
	switch ns {
	case "identity":
		if strings.HasPrefix(fn.Name, "my") || strings.HasPrefix(fn.Name, "own") {
			return "identity"
		}
		return "users"
	case "groups", "access":
		return "users"
	case "worker", "providers", "policies", "rules", "router":
		return "fleet"
	case "library":
		return "files"
	case "accounts":
		return "accounts"
	case "campaigns":
		return "campaigns"
	case "platform":
		return "deployables"
	case "agents", "node", "cluster", "automations":
		return "cluster"
	case "work", "goals":
		return "nexus"
	case "training", "knowledge":
		return "training"
	}
	return ""
}

func (e *MemQLEngine) askCapabilitiesBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, ok := auth.SubjectFromContext(ctx); !ok {
		return nil, fmt.Errorf("sign in to discover capabilities")
	}
	search := strings.ToLower(strings.TrimSpace(stringArg(args, "search")))
	if search == "" || len(search) > 300 {
		return nil, fmt.Errorf("provide a short capability search")
	}
	var matches []map[string]any
	for _, fn := range e.functions.List() {
		if !e.askAllowed(ctx, fn) {
			continue
		}
		name := QualifyConstruct(ConstructNamespaceForOrigin(fn.Origin), fn.Name)
		haystack := strings.ToLower(name + " " + fn.Description + " " + fn.DocComment + " " + fn.BoundConcept)
		found := true
		for _, word := range strings.Fields(search) {
			if !strings.Contains(haystack, word) {
				found = false
				break
			}
		}
		if !found {
			continue
		}
		fields := []map[string]any{}
		if fn.ArgsSchema != nil {
			for _, field := range fn.ArgsSchema.Fields {
				fields = append(fields, map[string]any{"name": field.Name, "type": field.Type, "optional": field.Optional, "description": field.Description, "enum": field.Enum})
			}
		}
		matches = append(matches, map[string]any{"name": name, "kind": fn.FunctionKind, "description": fn.Description, "arguments": fields, "app": askApp(fn)})
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i]["name"].(string) < matches[j]["name"].(string) })
	if len(matches) > 30 {
		matches = matches[:30]
	}
	raw, _ := json.Marshal(map[string]any{"capabilities": matches})
	return []memorynodes.MemoryNode{{ID: "capabilities", Payload: raw}}, nil
}

func (e *MemQLEngine) askExecuteBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	name := stringArg(args, "name")
	fn, err := e.functions.Get(name)
	if err != nil || !e.askAllowed(ctx, fn) {
		return nil, fmt.Errorf("capability is unavailable for this person")
	}
	arguments, ok := args["arguments"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("arguments must be an object")
	}
	call, err := parser.RenderCall(QualifyConstruct(ConstructNamespaceForOrigin(fn.Origin), fn.Name), arguments)
	if err != nil {
		return nil, err
	}
	// Runtime calls accept qualified names; `use` belongs to declarations,
	// not executable expressions. Keep the namespace to avoid ambiguous names.
	call = fn.FunctionKind + " " + call
	event := AskEvent{ID: id.NewShortId(), Kind: "action", Phase: "running", Name: name, App: askApp(fn), Navigate: askApp(fn) != ""}
	// Only resource identifiers are navigation hints. Never mirror arbitrary
	// content or credentials into desktop events.
	event.Arguments = map[string]any{}
	for key, value := range arguments {
		if strings.HasSuffix(key, "Id") {
			if v, ok := value.(string); ok {
				event.Arguments[key] = BareShortId(v)
			}
		}
	}
	if err := emitAsk(ctx, event); err != nil {
		return nil, fmt.Errorf("could not record action before execution: %w", err)
	}
	result, err := e.Execute(ctx, call)
	event.Phase = "completed"
	if err != nil {
		event.Phase = "failed"
		event.Error = err.Error()
	}
	if recordErr := emitAsk(ctx, event); recordErr != nil {
		return nil, fmt.Errorf("action result could not be recorded; check current state before retrying: %w", recordErr)
	}
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(result.OutputPayload())
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "result", Payload: raw}}, nil
}
