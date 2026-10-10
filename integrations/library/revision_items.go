package library

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/work"
)

type revisionItem struct {
	ID         string                `json:"id"`
	Edits      []revisionReplacement `json:"edits"`
	CommentIDs []string              `json:"commentIds"`
}

func revisionEdits(value any) ([]revisionReplacement, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var edits []revisionReplacement
	if err = json.Unmarshal(raw, &edits); err != nil {
		return nil, fmt.Errorf("the proposed changes are incomplete")
	}
	return edits, nil
}

// Edits connected by feedback are one decision: accepting the destination of
// a move without its removal would silently turn the move into a copy.
func revisionItems(proposal map[string]any) ([]revisionItem, error) {
	edits, err := revisionEdits(proposal["edits"])
	if err != nil {
		return nil, err
	}
	groups := make([]revisionItem, 0, len(edits))
	for _, edit := range edits {
		if edit.Before == edit.After {
			continue
		}
		group := revisionItem{Edits: []revisionReplacement{edit}, CommentIDs: append([]string(nil), edit.CommentIDs...)}
		for n := 0; n < len(groups); {
			overlap := false
			for _, id := range groups[n].CommentIDs {
				for _, other := range group.CommentIDs {
					if id == other {
						overlap = true
					}
				}
			}
			if !overlap {
				n++
				continue
			}
			group.Edits = append(groups[n].Edits, group.Edits...)
			for _, id := range groups[n].CommentIDs {
				found := false
				for _, other := range group.CommentIDs {
					if id == other {
						found = true
					}
				}
				if !found {
					group.CommentIDs = append(group.CommentIDs, id)
				}
			}
			groups = append(groups[:n], groups[n+1:]...)
		}
		groups = append(groups, group)
	}
	for n := range groups {
		groups[n].ID = "item-" + workstate.ArtifactHash(map[string]any{"edits": groups[n].Edits})
	}
	return groups, nil
}

func selectedRevisionProposal(proposal, answer map[string]any) (map[string]any, error) {
	if answer["proposalHash"] != workstate.ArtifactHash(proposal) {
		return nil, fmt.Errorf("review the current proposal before applying accepted changes")
	}
	raw, _ := json.Marshal(answer["acceptedItemIds"])
	var selected []string
	if json.Unmarshal(raw, &selected) != nil || len(selected) == 0 {
		return nil, fmt.Errorf("accept at least one proposed change")
	}
	items, err := revisionItems(proposal)
	if err != nil {
		return nil, err
	}
	known := map[string]revisionItem{}
	for _, item := range items {
		known[item.ID] = item
	}
	requested := map[string]bool{}
	result := revisionAnswer{Summary: asString(proposal["summary"])}
	for _, id := range selected {
		item, ok := known[id]
		if !ok || requested[id] {
			return nil, fmt.Errorf("the accepted changes do not match this proposal")
		}
		requested[id] = true
		result.Edits = append(result.Edits, item.Edits...)
	}
	return buildRevisionProposal(proposal, result)
}

func amendmentCapture(captured, amendment map[string]any) map[string]any {
	target, _ := revisionEdits(amendment["edits"])
	ids := map[string]bool{}
	for _, edit := range target {
		for _, id := range edit.CommentIDs {
			ids[id] = true
		}
	}
	raw, _ := json.Marshal(captured["comments"])
	var comments []map[string]any
	_ = json.Unmarshal(raw, &comments)
	filtered := make([]any, 0, len(comments))
	for _, comment := range comments {
		if ids[asString(comment["id"])] {
			filtered = append(filtered, comment)
		}
	}
	out := map[string]any{}
	for k, v := range captured {
		out[k] = v
	}
	out["comments"] = filtered
	out["instruction"] = amendment["instruction"]
	return out
}

func buildAmendedRevisionProposal(captured map[string]any, response any) (map[string]any, error) {
	amendment := revisionMap(captured["amendment"])
	if len(amendment) == 0 {
		return buildRevisionProposal(captured, response)
	}
	changed, err := buildRevisionProposal(amendmentCapture(captured, amendment), response)
	if err != nil {
		return nil, err
	}
	edits, err := revisionEdits(changed["edits"])
	if err != nil {
		return nil, err
	}
	retained, err := revisionEdits(captured["retainedEdits"])
	if err != nil {
		return nil, err
	}
	return buildRevisionProposal(captured, revisionAnswer{Summary: asString(changed["summary"]), Edits: append(retained, edits...)})
}

// Changing a reason or summary is not a different proposed document. Compare
// amendments with the prior proposal, not only with the still-unmodified file.
func revisionRequestChanged(captured, proposal map[string]any) (bool, error) {
	amendment := revisionMap(captured["amendment"])
	if len(amendment) == 0 {
		return proposal["revisedContent"] != captured["content"], nil
	}
	previous, err := revisionEdits(amendment["edits"])
	if err != nil {
		return false, err
	}
	retained, err := revisionEdits(captured["retainedEdits"])
	if err != nil {
		return false, err
	}
	baseline, err := buildRevisionProposal(captured, revisionAnswer{Summary: "Previous proposed document", Edits: append(retained, previous...)})
	if err != nil {
		return false, err
	}
	return proposal["revisedContent"] != baseline["revisedContent"], nil
}

func (i *Integration) validateCurrentRevision(ctx context.Context, proposal map[string]any) error {
	removed, err := i.removedRevisionComments(ctx, proposal)
	if err != nil {
		return err
	}
	if len(removed) > 0 {
		return fmt.Errorf("feedback in this proposal was deleted; propose changes again with the remaining requests")
	}

	successor, err := i.revisionRow(ctx, "workDocumentRevisionAmendment", map[string]any{"requestId": proposal["requestId"]})
	if err != nil {
		return err
	}
	if successor != nil {
		return fmt.Errorf("this proposal was replaced by a revised item; review the latest proposal")
	}
	return nil
}

func (i *Integration) handleModifyRevisionItem(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := revisionWriter(ctx); err != nil {
		return nil, err
	}
	if run, ok := common.RunFromContext(ctx); ok && run.RunId != "" {
		return nil, fmt.Errorf("a person must request a different proposed change")
	}
	if i.reviewGoals == nil {
		return nil, fmt.Errorf("document review jobs are unavailable on this node")
	}
	newID := asString(args["newRequestId"])
	receipt, err := revisionIdentity(ctx, newID)
	if err != nil {
		return nil, err
	}
	instruction := strings.TrimSpace(asString(args["instruction"]))
	if instruction == "" || len(instruction) > 8000 {
		return nil, fmt.Errorf("describe the different change in 1 to 8000 bytes")
	}
	releaseApproval, err := i.reviewGoals.LockPlanReview(ctx, asString(args["approvalId"]))
	if err != nil {
		return nil, err
	}
	defer releaseApproval()
	ctx = memql.ContextWithFreshRead(ctx)
	ids, captured, run, approval, err := i.revisionRequest(ctx, asString(args["requestId"]))
	if err != nil {
		return nil, err
	}
	if ids.ApprovalID != asString(args["approvalId"]) || approval == nil {
		return nil, fmt.Errorf("review the current proposal before modifying an item")
	}
	doc, release, err := i.lockReviewDocument(ctx, asString(captured["artifactId"]))
	if err != nil {
		return nil, err
	}
	defer release()
	existing, err := i.revisionRow(ctx, "workGoalForOwner", map[string]any{"goalId": receipt.GoalID})
	if err != nil {
		return nil, err
	}
	next := revisionMap(revisionMap(existing["input"])["proposal"])
	if existing != nil {
		amendment := revisionMap(next["amendment"])
		if amendment["requestId"] != args["requestId"] || amendment["approvalId"] != args["approvalId"] || amendment["itemId"] != args["itemId"] || amendment["instruction"] != instruction {
			return nil, fmt.Errorf("this request identifier was already used for another modification")
		}
	} else {
		if approval["decision"] != nil && approval["decision"] != "" || run["status"] != "waiting" {
			return nil, fmt.Errorf("only an undecided proposal can be modified")
		}
		successor, err := i.revisionRow(ctx, "workDocumentRevisionAmendment", map[string]any{"requestId": captured["requestId"]})
		if err != nil {
			return nil, err
		}
		if successor != nil {
			_, _, latest, _, err := i.revisionRequest(ctx, asString(successor["requestId"]))
			if err != nil {
				return nil, err
			}
			if latest["status"] != "failed" && latest["status"] != "cancelled" && latest["status"] != "abandoned" && latest["cancelRequested"] != true {
				return nil, fmt.Errorf("another modification is already preparing; review that proposal first")
			}
		}
		version, _ := intArg(captured["version"])
		content, readErr := i.revisionContent(ctx, doc)
		if readErr != nil {
			return nil, readErr
		}
		if doc.version != version || doc.revision != captured["revision"] || content != captured["content"] {
			return nil, fmt.Errorf("the document changed; request a new proposal against its current version")
		}
		items, err := revisionItems(revisionMap(approval["subject"]))
		if err != nil {
			return nil, err
		}
		var target *revisionItem
		var retained []revisionReplacement
		for n := range items {
			if items[n].ID == args["itemId"] {
				target = &items[n]
			} else {
				retained = append(retained, items[n].Edits...)
			}
		}
		if target == nil {
			return nil, fmt.Errorf("the selected change is not part of this proposal")
		}
		parent := revisionMap(approval["subject"])
		if err = i.validatePreparedRevisionImages(ctx, parent); err != nil {
			return nil, err
		}
		next = map[string]any{}
		for k, v := range captured {
			next[k] = v
		}
		next["requestId"] = newID
		// Carry immutable image receipts from the reviewed proposal. A prose
		// amendment is not permission to regenerate or silently unpin its images.
		next["inheritedPreparedImages"] = parent["preparedImages"]
		next["retainedEdits"] = retained
		next["amendment"] = map[string]any{"requestId": args["requestId"], "approvalId": args["approvalId"], "itemId": args["itemId"], "instruction": instruction, "edits": target.Edits}
	}
	// Hash the same JSON representation that the durable goal stores. Typed
	// edit structs encode fields in declaration order; decoded maps sort keys.
	encoded, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(encoded, &next); err != nil {
		return nil, err
	}
	ac, _ := auth.AccessFromContext(ctx)
	opened, err := i.reviewGoals.OpenAnalysisGoal(ctx, "library-revision:"+newID, work.DirectGoal{OwnerUserId: ac.UserId, Statement: "Revise a proposed change in " + asString(next["name"]), AutomationName: documentRevisionTemplate, RequestedVia: "library", TriggeredBy: "document-review", Ceilings: map[string]any{"maxModelCalls": 16}, Input: map[string]any{"requestId": newID, "proposal": next}})
	if err != nil {
		return nil, err
	}
	return reviewResult(map[string]any{"requestId": newID, "goalId": opened.GoalID, "runId": opened.RunID})
}
