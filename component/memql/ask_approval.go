package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
)

const ApprovalDiscussionTemplate = "askApprovalAnalysis"

const maxAskApprovalLinks = 128

// ApprovalDiscussionMessage is a human-authored turn. The work integration
// binds its child identities before publishing the run's dispatch event.
type ApprovalDiscussionMessage struct {
	ApprovalID, ConversationID, RequestID, Prompt string
	Advisor                                       bool
}

func (e *MemQLEngine) approvalRows(ctx context.Context, approvalID string) (map[string]any, map[string]any, error) {
	if subject, ok := auth.SubjectFromContext(ctx); !ok || !auth.CapableFor(ctx, subject, "read", "app:ask") {
		return nil, nil, fmt.Errorf("sign in to discuss this request")
	}
	ctx = ContextWithFreshRead(ctx)
	call, err := parser.RenderCall("workApprovalForOwner", map[string]any{"approvalId": approvalID})
	if err != nil {
		return nil, nil, err
	}
	result, err := e.Execute(ctx, "query "+call)
	if err != nil {
		return nil, nil, err
	}
	rows := MaterializeRows(result.OutputPayload())
	if len(rows) != 1 {
		return nil, nil, fmt.Errorf("request is unavailable")
	}
	approval := rows[0]
	runs, err := e.workRows(ctx, "workRunForOwner", stringArg(approval, "runId"))
	if err != nil || len(runs) != 1 {
		return nil, nil, fmt.Errorf("request work is unavailable")
	}
	return approval, runs[0], nil
}

func approvalPending(approval map[string]any) bool {
	if stringArg(approval, "decision") != "" {
		return false
	}
	if at, valid := askTimestamp(approval["expiresAt"]); valid && !time.Now().Before(at) {
		return false
	}
	return true
}

func approvalDiscussionWaiting(approval, run map[string]any) bool {
	waiting, _ := run["waitingOn"].(map[string]any)
	return stringArg(approval, "decision") == "" && run["status"] == "waiting" && run["cancelRequested"] != true && waiting["kind"] == "approval" && BareShortId(stringArg(waiting, "subject")) == BareShortId(stringArg(approval, "id"))
}

func approvalDiscussionPending(approval, run map[string]any) bool {
	return approvalPending(approval) && approvalDiscussionWaiting(approval, run)
}

func approvalDiscussionGuidancePending(approval, run map[string]any) bool {
	if !approvalDiscussionWaiting(approval, run) {
		return false
	}
	if approvalPending(approval) {
		return true
	}
	// Expiry prevents granting access, but the same native scope question can
	// still take a correction. The work adapter and continuation receiver retain
	// their owner, admitted-step and exact-checkpoint checks. Advice remains
	// subject to the ordinary pending gate.
	subject, _ := approval["subject"].(map[string]any)
	return workPermissionGuidanceSubject(stringArg(approval, "kind"), subject, stringArg(approval, "artifactHash"))
}

func askValue(value any) ([]memorynodes.MemoryNode, error) {
	raw, err := json.Marshal(value)
	return []memorynodes.MemoryNode{{ID: "ask-approval-result", Type: memorynodes.NodeTypeObject, Payload: raw}}, err
}

// Opening a discussion invokes no model and never changes the approval. The
// work adapter reuses the original conversation or creates a stable private one.
func (e *MemQLEngine) askApprovalDiscussionBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	approval, run, err := e.approvalRows(ctx, stringArg(args, "approvalId"))
	if err != nil {
		return nil, err
	}
	conversation, err := e.askWorkConversationBuiltin(ctx, map[string]any{"runId": run["id"]}, 0)
	if err != nil {
		return nil, err
	}
	var row map[string]any
	if len(conversation) != 1 || json.Unmarshal(conversation[0].Payload, &row) != nil {
		return nil, fmt.Errorf("conversation is unavailable")
	}
	if err := e.linkAskApproval(ctx, stringArg(row, "id"), stringArg(approval, "id")); err != nil {
		return nil, err
	}
	pending := approvalDiscussionPending(approval, run)
	subject, _ := approval["subject"].(map[string]any)
	summary := stringArg(subject, "errorMessage")
	if summary == "" {
		summary = stringArg(subject, "summary")
	}
	status := stringArg(approval, "decision")
	if status == "" {
		status = "pending"
		if !approvalPending(approval) {
			status = "expired"
		}
	}
	call, _ := parser.RenderCall("workApprovalDiscussionState", map[string]any{"approvalId": approval["id"]})
	state, err := e.Execute(ctx, "builtin "+call)
	if err != nil {
		return nil, err
	}
	stateRows := MaterializeRows(state.OutputPayload())
	canSend := approvalDiscussionGuidancePending(approval, run) && len(stateRows) == 1 && stateRows[0]["canSendGuidance"] == true
	response := map[string]any{
		"conversationId": BareShortId(stringArg(row, "id")),
		"approval":       map[string]any{"id": BareShortId(stringArg(approval, "id")), "runId": BareShortId(stringArg(run, "id")), "kind": approval["kind"], "question": boundedMemoryText(stringArg(approval, "question"), 4000), "status": status, "summary": boundedMemoryText(summary, 2000)},
		"canAskAdvisor":  pending, "canSendGuidance": canSend,
	}
	if len(stateRows) == 1 {
		response["guidanceMode"] = stringArg(stateRows[0], "guidanceMode")
		response["savedGuidance"] = stringArg(stateRows[0], "savedGuidance")
		if guidance := stringArg(stateRows[0], "guidanceToResume"); guidance != "" {
			response["guidanceToResume"] = guidance
		}
	}
	return askValue(response)
}

func (e *MemQLEngine) askApprovalMessageBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	// A model cannot use this human admission surface to delegate itself.
	if run, ok := common.RunFromContext(ctx); ok && run.RunId != "" {
		return nil, fmt.Errorf("approval discussion needs a person's message")
	}
	call, err := parser.RenderCall("workOpenApprovalDiscussion", args)
	if err != nil {
		return nil, err
	}
	result, err := e.Execute(ctx, "builtin "+call)
	if err != nil {
		return nil, err
	}
	rows := MaterializeRows(result.OutputPayload())
	if len(rows) != 1 {
		return nil, fmt.Errorf("discussion could not be started")
	}
	return askValue(rows[0])
}

// ValidateApprovalDiscussionMessage binds admission to the original owned
// conversation. Neither a caller-supplied page context nor a guessed run id is
// authority to read another request. Replays may recover an already-saved turn.
func (e *MemQLEngine) ValidateApprovalDiscussionMessage(ctx context.Context, message ApprovalDiscussionMessage) (string, error) {
	if message.RequestID == "" || len(message.RequestID) > 120 || strings.TrimSpace(message.Prompt) == "" || utf8.RuneCountInString(message.Prompt) > 4000 {
		return "", fmt.Errorf("discussion needs a request id and a message of 1 to 4000 characters")
	}
	approval, run, err := e.approvalRows(ctx, message.ApprovalID)
	if err != nil {
		return "", err
	}
	conversation, err := e.askWorkConversationBuiltin(ctx, map[string]any{"runId": run["id"]}, 0)
	if err != nil {
		return "", err
	}
	if len(conversation) != 1 || BareShortId(conversation[0].ID) != BareShortId(message.ConversationID) {
		return "", fmt.Errorf("use this request's own conversation")
	}
	if !approvalDiscussionPending(approval, run) {
		row, readErr := e.askRead(ContextWithFreshRead(ctx), message.ConversationID)
		if readErr != nil {
			return "", readErr
		}
		transcript, err := approvalTranscript(row)
		if err != nil {
			return "", err
		}
		for _, turn := range transcript.Turns {
			if sameApprovalMessage(turn, message) {
				return BareShortId(stringArg(run, "id")), nil
			}
		}
		return "", fmt.Errorf("this request is no longer pending")
	}
	return BareShortId(stringArg(run, "id")), nil
}

func ApprovalDiscussionTurnID(message ApprovalDiscussionMessage) string {
	return workContextHash("approval-message", []byte(BareShortId(message.ApprovalID)+"\x00"+message.RequestID))
}

func sameApprovalMessage(turn AskTurn, message ApprovalDiscussionMessage) bool {
	return turn.ID == ApprovalDiscussionTurnID(message) && BareShortId(turn.ApprovalID) == BareShortId(message.ApprovalID) && turn.Prompt == message.Prompt && turn.Advisor == message.Advisor
}

func approvalTranscript(row map[string]any) (askTranscript, error) {
	var transcript askTranscript
	raw, err := json.Marshal(row["transcript"])
	if err != nil {
		return transcript, err
	}
	err = json.Unmarshal(raw, &transcript)
	return transcript, err
}

// A discussion can go straight from opening to sending guidance, without an
// analysis turn. Keep its durable request link so a later snapshot can recover
// that human decision, including when the request belongs to a delegated run.
func (e *MemQLEngine) linkAskApproval(ctx context.Context, conversationID, approvalID string) error {
	hold, err := e.holdAskConversation(ctx, conversationID, "write")
	if err != nil {
		return err
	}
	defer hold.release()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	row, err := e.askRead(ContextWithFreshRead(ctx), conversationID)
	if err != nil {
		return err
	}
	transcript, err := approvalTranscript(row)
	if err != nil {
		return err
	}
	id := BareShortId(approvalID)
	for _, linked := range transcript.ApprovalIDs {
		if BareShortId(linked) == id {
			return nil
		}
	}
	if len(transcript.ApprovalIDs) >= maxAskApprovalLinks {
		return fmt.Errorf("this conversation has reached its linked request limit")
	}
	transcript.ApprovalIDs = append(transcript.ApprovalIDs, id)
	if err := hold.verify(ctx); err != nil {
		return err
	}
	return e.askSave(ctx, conversationID, stringArg(row, "title"), transcript)
}

// AppendApprovalDiscussionTurn records a human message before the child run is
// reachable. Short fresh read/merge locks let another human message arrive while
// advice is running; no observer later replaces the whole saved transcript.
func (e *MemQLEngine) AppendApprovalDiscussionTurn(ctx context.Context, message ApprovalDiscussionMessage, goalID, runID string) error {
	if _, err := e.ValidateApprovalDiscussionMessage(ctx, message); err != nil {
		return err
	}
	hold, err := e.holdAskConversation(ctx, message.ConversationID, "write")
	if err != nil {
		return err
	}
	defer hold.release()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	row, err := e.askRead(ContextWithFreshRead(ctx), message.ConversationID)
	if err != nil {
		return err
	}
	transcript, err := approvalTranscript(row)
	if err != nil {
		return err
	}
	for _, turn := range transcript.Turns {
		if turn.ID != ApprovalDiscussionTurnID(message) {
			continue
		}
		if !sameApprovalMessage(turn, message) || BareShortId(turn.RunID) != BareShortId(runID) {
			return fmt.Errorf("this message id already belongs to another discussion")
		}
		return nil
	}
	transcript.Turns = append(transcript.Turns, AskTurn{ID: ApprovalDiscussionTurnID(message), ApprovalID: BareShortId(message.ApprovalID), Advisor: message.Advisor, Prompt: message.Prompt, State: "queued", StartedAt: time.Now().UTC(), GoalID: BareShortId(goalID), RunID: BareShortId(runID), Background: true, Activity: []WorkEvent{}})
	if err := hold.verify(ctx); err != nil {
		return err
	}
	return e.askSave(ctx, message.ConversationID, stringArg(row, "title"), transcript)
}

// Recover scope from durable owner-bound rows on the executing replica. No
// serialized argument can supply the model's approval, parent or conversation.
func (e *MemQLEngine) approvalAnalysisScope(ctx context.Context) (map[string]any, map[string]any, map[string]any, error) {
	rc, ok := common.RunFromContext(ctx)
	if !ok || rc.RunId == "" || rc.StepKey == "" {
		return nil, nil, nil, fmt.Errorf("approval analysis requires its owned child run")
	}
	runs, err := e.workRows(ContextWithFreshRead(ctx), "workRunForOwner", rc.RunId)
	if err != nil || len(runs) != 1 {
		return nil, nil, nil, fmt.Errorf("approval analysis run is unavailable")
	}
	run := runs[0]
	input, _ := run["input"].(map[string]any)
	if BareShortId(rc.OwnerUserId) != BareShortId(stringArg(run, "ownerUserId")) || BareShortId(rc.GoalId) != BareShortId(stringArg(run, "goalId")) {
		return nil, nil, nil, fmt.Errorf("approval analysis requires its recorded owner and budget")
	}
	if run["automationName"] != ApprovalDiscussionTemplate || run["triggeredBy"] != "approval-discussion" || run["status"] != "running" || run["cancelRequested"] == true {
		return nil, nil, nil, fmt.Errorf("this run cannot analyze a pending request")
	}
	approval, parent, err := e.approvalRows(ctx, stringArg(input, "approvalId"))
	if err != nil {
		return nil, nil, nil, err
	}
	if BareShortId(stringArg(run, "parentRunId")) != BareShortId(stringArg(parent, "id")) {
		return nil, nil, nil, fmt.Errorf("approval analysis lost its parent binding")
	}
	if !approvalDiscussionPending(approval, parent) {
		return nil, nil, nil, fmt.Errorf("the parent request is no longer pending")
	}
	row, err := e.askRead(ContextWithFreshRead(ctx), stringArg(input, "conversationId"))
	if err != nil {
		return nil, nil, nil, err
	}
	message := ApprovalDiscussionMessage{ApprovalID: stringArg(input, "approvalId"), ConversationID: stringArg(input, "conversationId"), RequestID: stringArg(input, "requestId"), Prompt: stringArg(input, "prompt"), Advisor: input["advisor"] == true}
	transcript, err := approvalTranscript(row)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, turn := range transcript.Turns {
		if sameApprovalMessage(turn, message) && BareShortId(turn.RunID) == BareShortId(rc.RunId) {
			return approval, parent, input, nil
		}
	}
	return nil, nil, nil, fmt.Errorf("approval analysis has no recorded human message")
}

func (e *MemQLEngine) askApprovalEvidenceBuiltin(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = ContextWithFreshRead(ctx)
	approval, parent, input, err := e.approvalAnalysisScope(ctx)
	if err != nil {
		return nil, err
	}
	subject, _ := approval["subject"].(map[string]any)
	// Exact named receipt only: no unbounded read of an owner's full history,
	// credentials, continuation, tool arguments or other conversation's input.
	evidence := map[string]any{"question": boundedMemoryText(stringArg(approval, "question"), 4000), "kind": approval["kind"], "runStatus": parent["status"], "error": boundedMemoryText(stringArg(parent, "errorMessage"), 4000)}
	if permission, ok := subject["computerPermission"].(map[string]any); ok {
		scope := map[string]any{}
		for _, key := range []string{"machineLabel", "directory", "command", "cwd", "kind", "action"} {
			if value := stringArg(permission, key); value != "" {
				scope[key] = boundedMemoryText(value, 2000)
			}
		}
		if timeout, ok := permission["timeoutSeconds"].(float64); ok && timeout > 0 {
			scope["timeoutSeconds"] = timeout
		}
		evidence["computerPermission"] = scope
	}
	evidence["reportedCause"] = map[string]any{"symptom": subject["symptom"], "message": boundedMemoryText(stringArg(subject, "errorMessage"), 4000)}
	goalCall, _ := parser.RenderCall("workGoalForOwner", map[string]any{"goalId": parent["goalId"]})
	goalResult, err := e.Execute(ContextWithFreshRead(ctx), "query "+goalCall)
	if err != nil {
		return nil, err
	}
	if goals := MaterializeRows(goalResult.OutputPayload()); len(goals) == 1 {
		evidence["originalRequest"] = boundedMemoryText(stringArg(goals[0], "statement"), 4000)
	}
	row, err := e.askRead(ContextWithFreshRead(ctx), stringArg(input, "conversationId"))
	if err != nil {
		return nil, err
	}
	transcript, err := approvalTranscript(row)
	if err != nil {
		return nil, err
	}
	turns := transcript.Turns
	discussion := []map[string]any{}
	for n := len(turns) - 1; n >= 0 && len(discussion) < 6; n-- {
		turn := turns[n]
		if BareShortId(turn.ApprovalID) != BareShortId(stringArg(approval, "id")) {
			continue
		}
		entry := map[string]any{"message": boundedMemoryText(turn.Prompt, 2000), "advisor": turn.Advisor}
		if turn.RunID != "" && turn.RunID != commonRunID(ctx) {
			answer, followErr := e.followWorkRun(context.WithValue(ctx, workFollowModeKey{}, followSnapshot), turn.RunID, nil, nil)
			if followErr == nil {
				entry["advisoryReply"] = boundedMemoryText(answer, 2000)
			}
		}
		discussion = append(discussion, entry)
	}
	for left, right := 0, len(discussion)-1; left < right; left, right = left+1, right-1 {
		discussion[left], discussion[right] = discussion[right], discussion[left]
	}
	evidence["recentDiscussion"] = discussion
	key := stringArg(approval, "stepKey")
	if key == "" {
		key = stringArg(subject, "stepKey")
	}
	if key != "" {
		call, _ := parser.RenderCall("workStepForOwnerRun", map[string]any{"runId": parent["id"], "stepKey": key})
		result, err := e.Execute(ContextWithFreshRead(ctx), "query "+call)
		if err != nil {
			return nil, err
		}
		rows := MaterializeRows(result.OutputPayload())
		if len(rows) == 1 {
			evidence["step"] = map[string]any{"key": key, "status": rows[0]["status"], "error": boundedMemoryText(stringArg(rows[0], "errorMessage"), 4000), "symptom": rows[0]["symptom"]}
		}
	}
	return askValue(evidence)
}

func commonRunID(ctx context.Context) string {
	rc, _ := common.RunFromContext(ctx)
	return BareShortId(rc.RunId)
}

func (e *MemQLEngine) askRunApprovalMessageBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	_, _, input, err := e.approvalAnalysisScope(ctx)
	if err != nil {
		return nil, err
	}
	if e.modelSeam == nil || e.modelSeam.ceilings == nil || e.modelSeam.journal == nil {
		return nil, fmt.Errorf("approval analysis needs its model budget and journal")
	}
	if len(args) != 0 {
		return nil, fmt.Errorf("approval analysis takes its prompt from the recorded human choice")
	}
	template := "askApprovalReply"
	if input["advisor"] == true {
		template = "askApprovalSecondOpinion"
	}
	evidence, err := e.askApprovalEvidenceBuiltin(ctx, nil, 0)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// InvokeAI is the work model seam: admission, deadline, accounting and
	// durable replay apply to this single call. No execution tools are offered.
	reply, err := e.InvokeAI(ctx, template, map[string]any{"question": input["prompt"], "evidence": string(evidence[0].Payload)})
	if err != nil {
		return nil, err
	}
	text, ok := reply.(string)
	if !ok || strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > 16000 {
		return nil, fmt.Errorf("approval analysis returned no bounded text reply")
	}
	return askValue(map[string]any{"reply": text})
}
