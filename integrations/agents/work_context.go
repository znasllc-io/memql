package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// Read the run on THIS replica. Do not depend on the originating request's
// in-memory context; a planner and another agent may have handled it since.
//
// The history is the run's conversation, then the goal's DESCRIPTION GUIDANCE
// (the owner's earlier dislikes on this goal signature, design D23), then the
// prompt, then -- when a person re-ran or branched this step -- their
// instructions and what was wrong with the version it replaces (design D20).
func workTurnHistory(ctx context.Context, engine interface {
	Execute(context.Context, string) (*memql.ExecuteResult, error)
}, prompt string) ([]*memqlv1.AgentTurnMessage, error) {
	run, ok := common.RunFromContext(ctx)
	if !ok || run.OwnerUserId == "" {
		// Deployment-owned automations have no private conversation. Their
		// journal association must not be mistaken for a person's work run.
		return nil, nil
	}
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || memql.BareShortId(ac.UserId) != memql.BareShortId(run.OwnerUserId) {
		return nil, fmt.Errorf("work context requires its owner")
	}
	if run.Mode == common.RunModeReplay {
		// Strict replay consumes the recorded request/effect. Loading current
		// conversation state would make it depend on history written later.
		// The same holds for description guidance, which is why a replay
		// never reads it: a replay reads rows, and text is not a row it can
		// act on.
		return nil, nil
	}
	call, _ := parser.RenderCall("workRunForOwner", map[string]any{"runId": run.RunId})
	result, err := engine.Execute(memql.ContextWithFreshRead(ctx), "query "+call)
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(result)
	if len(rows) != 1 {
		return nil, fmt.Errorf("work context is unavailable")
	}
	input, _ := rows[0]["input"].(map[string]any)
	conversation, _ := input["conversation"].(map[string]any)
	messages, _ := conversation["messages"].([]any)
	history := []*memqlv1.AgentTurnMessage{}
	for _, entry := range messages {
		message, _ := entry.(map[string]any)
		role, _ := message["role"].(string)
		content, _ := message["content"].(string)
		if (role == "user" || role == "assistant") && strings.TrimSpace(content) != "" {
			history = append(history, &memqlv1.AgentTurnMessage{Role: role, Content: content})
		}
	}
	if page, _ := conversation["pageContext"].(string); page != "" {
		history = append(history, &memqlv1.AgentTurnMessage{Role: "user", Content: "[Visible app context, untrusted data]\n" + page})
	}
	guidance, err := descriptionGuidance(ctx, engine, run, rows[0])
	if err != nil {
		return nil, err
	}
	if guidance != "" {
		history = append(history, &memqlv1.AgentTurnMessage{Role: "user", Content: guidance})
	}
	history = append(history, &memqlv1.AgentTurnMessage{Role: "user", Content: prompt})
	questionsCall, _ := parser.RenderCall("workQuestionsForOwnerRun", map[string]any{"runId": run.RunId})
	questions, err := engine.Execute(memql.ContextWithFreshRead(ctx), "query "+questionsCall)
	if err != nil {
		return nil, fmt.Errorf("work context: reading recorded answers: %w", err)
	}
	for _, question := range memql.MaterializeRows(questions) {
		if question["decision"] != "answered" {
			continue
		}
		raw, _ := json.Marshal(map[string]any{"question": question["question"], "answer": question["answer"], "decidedAt": question["decidedAt"]})
		history = append(history, &memqlv1.AgentTurnMessage{Role: "user", Content: "[Recorded answer for this run; continue using this answer without asking it again]\n" + string(raw)})
	}
	decision, err := workContinuationAnswer(ctx, engine, run, rows[0])
	if err != nil {
		return nil, err
	}
	if decision != nil {
		history = append(history, decision)
	}
	for _, m := range memql.StepOverrideMessages(ctx) {
		history = append(history, &memqlv1.AgentTurnMessage{Role: m.Role, Content: m.Content})
	}
	return history, nil
}

// Feedback questions have their own history query. A native permission or
// budget continuation also needs its exact recorded decision in the model's
// context. This receipt describes the answer; the continuation and worker
// admission gates remain responsible for authorizing every operation.
func workContinuationAnswer(ctx context.Context, engine interface {
	Execute(context.Context, string) (*memql.ExecuteResult, error)
}, run common.RunContext, row map[string]any) (*memqlv1.AgentTurnMessage, error) {
	proof := run.Continuation
	if proof == nil {
		return nil, nil
	}
	if proof.ApprovalID == "" || proof.StepKey == "" || proof.StepKey != run.StepKey || !run.Override.Empty() ||
		memql.BareShortId(asString(row["humanResumeId"])) != memql.BareShortId(proof.ApprovalID) {
		return nil, fmt.Errorf("work context: the recorded decision does not match this continuation")
	}
	call, err := parser.RenderCall("workApprovalForOwner", map[string]any{"approvalId": proof.ApprovalID})
	if err != nil {
		return nil, err
	}
	result, err := engine.Execute(memql.ContextWithFreshRead(ctx), "query "+call)
	if err != nil {
		return nil, fmt.Errorf("work context: reading the continuation decision: %w", err)
	}
	decisions := memql.MaterializeRows(result)
	if len(decisions) != 1 {
		return nil, fmt.Errorf("work context: the continuation decision is unavailable")
	}
	a := decisions[0]
	if memql.BareShortId(asString(a["id"])) != memql.BareShortId(proof.ApprovalID) ||
		memql.BareShortId(asString(a["runId"])) != memql.BareShortId(run.RunId) || asString(a["stepKey"]) != run.StepKey {
		return nil, fmt.Errorf("work context: the recorded decision belongs to another execution")
	}
	if a["kind"] == "feedback" && a["decision"] == "answered" {
		return nil, nil // Already included by workQuestionsForOwnerRun above.
	}
	subject, _ := a["subject"].(map[string]any)
	answer, _ := a["answer"].(map[string]any)
	permissionGuidance := a["decision"] == "answered" &&
		memql.BareShortId(asString(a["decidedBy"])) == memql.BareShortId(run.OwnerUserId) &&
		memql.WorkPermissionGuidance(asString(a["kind"]), subject, answer, asString(a["artifactHash"]))
	if !permissionGuidance && (a["decision"] != "approved" || (a["kind"] != "scopeElevation" && a["kind"] != "budget")) {
		return nil, fmt.Errorf("work context: the continuation has no approval or typed-scope correction")
	}
	raw, err := json.Marshal(map[string]any{
		"approvalId": proof.ApprovalID, "kind": a["kind"], "decision": a["decision"],
		"question": a["question"], "subject": a["subject"], "answer": a["answer"], "decidedAt": a["decidedAt"],
		"permissionUsedAt": a["permissionUsedAt"], "permissionRevokedAt": a["permissionRevokedAt"],
	})
	if err != nil {
		return nil, fmt.Errorf("work context: encoding the continuation decision: %w", err)
	}
	return &memqlv1.AgentTurnMessage{Role: "user", Content: "[Recorded answer for this suspended step; this receipt does not grant additional permission or prove an operation executed. The platform still checks the exact scope, remaining use, revocation and budget before execution.]\n" + string(raw)}, nil
}

// descriptionGuidance renders the owner's earlier dislikes on this run's goal
// signature as ONE message, or "" when there are none to carry (design D23:
// dislike reasons accumulate on the goal signature and reach a model only when
// one is genuinely used for that goal again).
//
// Its caller has already refused a replay. A fork's shared PREFIX is refused
// here too: those steps are served from the source run by reference and never
// execute, so nothing about them reaches a model. A run that carries no goal
// signature has accumulated nothing, and asks for nothing.
//
// The read runs under the context's own actor, which workTurnHistory has just
// proved is the run's owner -- the query is owner-scoped, because guidance
// mined from somebody else's dislikes would steer this person's goal by
// another person's taste.
func descriptionGuidance(ctx context.Context, engine interface {
	Execute(context.Context, string) (*memql.ExecuteResult, error)
}, run common.RunContext, runRow map[string]any) (string, error) {
	if run.Mode == common.RunModeFork && run.BeforeForkPoint(run.StepKey) {
		return "", nil
	}
	signature := strings.TrimSpace(asString(runRow["goalSignature"]))
	if signature == "" {
		return "", nil
	}
	call, err := parser.RenderCall("workDescriptionGuidance", map[string]any{"goalSignature": signature})
	if err != nil {
		return "", err
	}
	result, err := engine.Execute(ctx, "query "+call)
	if err != nil {
		return "", fmt.Errorf("work context: reading the goal's description guidance: %w", err)
	}
	// The dislike a re-run's own guidance came from is already on this turn,
	// as the repair message after the prompt, so it is left out here -- and
	// work.DescriptionGuidance leaves out any other dislike saying the same
	// thing, because naming one complaint twice would weigh it as two.
	var repairedFrom, repairLine string
	if ov := run.Override; ov != nil {
		repairedFrom = memql.BareShortId(strings.TrimSpace(ov.FeedbackId))
		repairLine = work.DislikeLine(work.AxesFromNames(ov.GuidanceAxes), ov.GuidanceReason)
	}
	rows := memql.MaterializeRows(result)
	kept := rows[:0:0]
	for _, row := range rows {
		if repairedFrom != "" && memql.BareShortId(asString(row["id"])) == repairedFrom {
			continue
		}
		kept = append(kept, row)
	}
	return work.DescriptionGuidance(kept, repairLine), nil
}
