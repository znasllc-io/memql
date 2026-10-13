package work

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

type approvalDiscussionEngine interface {
	ValidateApprovalDiscussionMessage(context.Context, memql.ApprovalDiscussionMessage) (string, error)
	AppendApprovalDiscussionTurn(context.Context, memql.ApprovalDiscussionMessage, string, string) error
}

func (i *Integration) handleApprovalDiscussionState(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := requirePrincipal(ctx); err != nil {
		return nil, err
	}
	approval, err := one(i.store().query(memql.ContextWithFreshRead(ctx), "query "+call("workApprovalForOwner", map[string]any{"approvalId": argString(args, "approvalId")})))
	if err != nil {
		return nil, err
	}
	if approval == nil {
		return nil, fmt.Errorf("request is unavailable")
	}
	return i.resultNode(map[string]any{"canSendGuidance": i.ApprovalGuidanceAvailable(ctx, approval), "guidanceToResume": i.RecoverableApprovalGuidance(ctx, approval)}), nil
}

// Admission is a human action, separate from the read-only model capability.
// It opens a fixed, bounded child and never writes the parent's decision, work
// status, grants or budget. The existing dispatcher owns execution and recovery.
func (i *Integration) handleOpenApprovalDiscussion(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if ac, err := requirePrincipal(ctx); err != nil {
		return nil, err
	} else if ac.Synthetic || ac.Unranked {
		return nil, fmt.Errorf("approval discussion needs a signed-in person")
	}
	if run, ok := common.RunFromContext(ctx); ok && run.RunId != "" {
		return nil, fmt.Errorf("approval discussion needs a person's message")
	}
	engine, ok := i.engine.(approvalDiscussionEngine)
	if !ok {
		return nil, fmt.Errorf("approval discussion is unavailable on this node")
	}
	message := memql.ApprovalDiscussionMessage{ApprovalID: memql.BareShortId(argString(args, "approvalId")), ConversationID: memql.BareShortId(argString(args, "conversationId")), RequestID: argString(args, "requestId"), Prompt: argString(args, "prompt"), Advisor: args["advisor"] == true}
	parent, err := engine.ValidateApprovalDiscussionMessage(ctx, message)
	if err != nil {
		return nil, err
	}
	input := map[string]any{"approvalId": message.ApprovalID, "conversationId": message.ConversationID, "requestId": message.RequestID, "prompt": message.Prompt, "advisor": message.Advisor}
	receipt, err := i.OpenAnalysisGoal(ctx, "approval-message-"+memql.ApprovalDiscussionTurnID(message), DirectGoal{
		OwnerUserId: callerUserId(ctx), ParentRunID: parent, Statement: message.Prompt,
		AutomationName: memql.ApprovalDiscussionTemplate, Input: input, RequestedVia: "ask", TriggeredBy: "approval-discussion",
		Ceilings: map[string]any{"wallClockMs": 120000, "maxModelCalls": 1, "maxRetries": 1, "tokenBudget": 16000},
		BeforeRun: func(c context.Context, goalID, runID string) error {
			return engine.AppendApprovalDiscussionTurn(c, message, goalID, runID)
		},
	})
	if err != nil {
		return nil, err
	}
	return i.resultNode(map[string]any{"conversationId": message.ConversationID, "turnId": memql.ApprovalDiscussionTurnID(message), "runId": receipt.RunID, "status": "queued"}), nil
}
