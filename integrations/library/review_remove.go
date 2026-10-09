package library

import (
	"context"
	"fmt"
	"sort"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/library/reviewstore"
)

// Deletion preserves the immutable receipt, so a delayed submission cannot
// resurrect it. The document lock serializes it with capture and application;
// the approval lock has the same order as item modification and human approval.
func (i *Integration) handleRemoveDocumentAnnotation(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	subject, ok := auth.SubjectFromContext(ctx)
	access, _ := auth.AccessFromContext(ctx)
	if !ok || access == nil || access.UserId == "" || !auth.CapableFor(ctx, subject, auth.VerbUpdate, auth.ResourceData) {
		return nil, fmt.Errorf("permission denied to remove document feedback")
	}
	ctx = memql.ContextWithFreshRead(ctx)
	artifact, comment := asString(args["artifactId"]), memql.BareShortId(asString(args["commentId"]))
	if comment == "" {
		return nil, fmt.Errorf("choose saved feedback or a note to delete")
	}
	doc, err := i.reviewDocument(ctx, artifact)
	if err != nil {
		return nil, err
	}
	requests, err := i.revisionRows(ctx, "workActiveDocumentRevisionRequests", map[string]any{"artifactId": doc.artifact})
	if err != nil {
		return nil, err
	}
	if len(requests) > 100 {
		return nil, fmt.Errorf("stop active document reviews before deleting feedback")
	}
	sort.Slice(requests, func(a, b int) bool { return asString(requests[a]["requestId"]) < asString(requests[b]["requestId"]) })
	locked := map[string]string{}
	for _, request := range requests {
		requestID := asString(request["requestId"])
		ids, _, _, _, err := i.revisionRequest(ctx, requestID)
		if err != nil {
			return nil, err
		}
		locked[requestID] = ids.ApprovalID
		if ids.ApprovalID != "" {
			if i.reviewGoals == nil {
				return nil, fmt.Errorf("document review jobs are unavailable on this node")
			}
			release, err := i.reviewGoals.LockPlanReview(ctx, ids.ApprovalID)
			if err != nil {
				return nil, err
			}
			defer release()
		}
	}
	doc, release, err := i.lockReviewDocument(ctx, artifact)
	if err != nil {
		return nil, err
	}
	defer release()
	raw, err := reviewstore.ByID(ctx, i.engine, doc.owner, comment)
	if err != nil {
		return nil, err
	}
	rows := extractRows(raw)
	if len(rows) != 1 || memql.BareShortId(stringField(rows[0], "artifactId")) != doc.artifact || memql.BareShortId(stringField(rows[0], "authorUserId")) != memql.BareShortId(access.UserId) {
		return nil, fmt.Errorf("permission denied: only the author can delete this feedback or note")
	}
	if boolField(rows[0], "removed") {
		return reviewResult(map[string]any{"removed": true, "commentId": comment})
	}
	if stringField(rows[0], "purpose") != "note" {
		current, err := i.revisionRows(ctx, "workActiveDocumentRevisionRequests", map[string]any{"artifactId": doc.artifact})
		if err != nil {
			return nil, err
		}
		for _, request := range current {
			requestID := asString(request["requestId"])
			ids, proposal, run, approval, err := i.revisionRequest(ctx, requestID)
			if err != nil {
				return nil, err
			}
			expected, ok := locked[requestID]
			if !ok || ids.ApprovalID != expected {
				return nil, fmt.Errorf("the review changed; reopen it before deleting feedback")
			}
			comments, err := revisionCommentIDs(proposal["commentIds"])
			if err != nil {
				return nil, err
			}
			for _, id := range comments {
				if id != comment {
					continue
				}
				terminal := run["status"] == "succeeded" || run["status"] == "failed" || run["status"] == "cancelled"
				if !terminal && (approval["decision"] == "approved" || run["cancelRequested"] != true) {
					return nil, fmt.Errorf("stop the current review before deleting its feedback; approved changes must finish first")
				}
			}
		}
	}

	if _, err = reviewstore.Remove(ctx, i.engine, doc.owner, comment); err != nil {
		return nil, err
	}
	return reviewResult(map[string]any{"removed": true, "commentId": comment})
}

// A fresh tombstone read invalidates every captured proposal, including an old
// editor, a resumed run or a Nexus approval on another replica.
func (i *Integration) removedRevisionComments(ctx context.Context, proposal map[string]any) ([]string, error) {
	ids, err := revisionCommentIDs(proposal["commentIds"])
	if err != nil {
		return nil, err
	}
	doc, err := i.reviewDocument(ctx, asString(proposal["artifactId"]))
	if err != nil {
		return nil, err
	}
	result, err := reviewstore.Removed(ctx, i.engine, doc.owner, doc.artifact, ids)
	if err != nil {
		return nil, err
	}
	removed := make([]string, 0)
	for _, row := range extractRows(result) {
		removed = append(removed, memql.BareShortId(stringField(row, "id")))
	}
	return removed, nil
}
