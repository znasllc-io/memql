package library

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/library/reviewstore"
	"github.com/znasllc-io/memql/integrations/work"
)

const documentReviewType = "library-document"
const documentRevisionTemplate = "reviseLibraryDocument"
const revisionContentLimit = 128 * 1024

type ReviewGoalOpener interface {
	LockPlanReview(context.Context, string) (func(), error)
	OpenAnalysisGoal(context.Context, string, work.DirectGoal) (work.ReviewGoalReceipt, error)
	AskPlanReview(context.Context, string, string, map[string]any, string) (string, error)
	SetPlanReviewValidator(string, func(context.Context, map[string]any) error)
	SetPlanReviewAnswerValidator(string, func(context.Context, map[string]any, map[string]any) error)
}

// SetReviewGoals joins the registered Library and Nexus instances. No editor
// runner, borrowed cluster-owner authority, or second approval inbox is created.
func (i *Integration) SetReviewGoals(opener ReviewGoalOpener) {
	i.reviewGoals = opener
	if opener != nil {
		opener.SetPlanReviewValidator(documentReviewType, i.ValidateRevisionProposal)
		opener.SetPlanReviewAnswerValidator(documentReviewType, func(ctx context.Context, proposal, answer map[string]any) error {
			_, err := selectedRevisionProposal(proposal, answer)
			return err
		})
	}
}

func (i *Integration) revisionRow(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		return nil, err
	}
	raw, err := i.engine.Execute(ctx, "query "+call)
	if err != nil {
		return nil, err
	}
	rows := extractRows(raw)
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}
func revisionIdentity(ctx context.Context, requestID string) (work.ReviewGoalReceipt, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || ac.UserId == "" || !reviewRequestID.MatchString(requestID) {
		return work.ReviewGoalReceipt{}, fmt.Errorf("sign in and supply a revision request identifier")
	}
	return work.ReviewGoalIdentity(ac.UserId, "library-revision:"+requestID), nil
}
func revisionWriter(ctx context.Context) error {
	ac, ok := auth.AccessFromContext(ctx)
	subject, hasSubject := auth.SubjectFromContext(ctx)
	if !ok || ac == nil || ac.UserId == "" || ac.Synthetic || ac.Unranked || !hasSubject || !auth.CapableFor(ctx, subject, auth.VerbCreate, auth.ResourceData) {
		return fmt.Errorf("you do not have permission to request document changes")
	}
	return nil
}
func (i *Integration) revisionContent(ctx context.Context, doc reviewDocument) (string, error) {
	var content []byte
	if doc.kind == "file" {
		if i.blobFetcher == nil {
			return "", fmt.Errorf("document storage is unavailable on this node")
		}
		stream, err := i.blobFetcher.DownloadStreamURL(ctx, stringField(doc.backing, "blobUrl"))
		if err != nil {
			return "", fmt.Errorf("could not read the saved document")
		}
		defer stream.Close()
		content, err = io.ReadAll(io.LimitReader(stream, revisionContentLimit+1))
		if err != nil {
			return "", err
		}
	} else {
		content = []byte(stringField(doc.backing, "body"))
	}
	if len(content) > revisionContentLimit || !utf8.Valid(content) {
		return "", fmt.Errorf("revision requests support UTF-8 Markdown documents up to 128 KiB")
	}
	return string(content), nil
}
func (i *Integration) lockReviewDocument(ctx context.Context, artifact string) (reviewDocument, func(), error) {
	doc, err := i.reviewDocument(ctx, artifact)
	if err != nil {
		return doc, nil, err
	}
	gate := i.versionGate
	if doc.kind == "file" {
		gate = i.fileVersionGate
	}
	release, err := gate(ctx, doc.source)
	if err != nil {
		return doc, nil, err
	}
	doc, err = i.reviewDocument(ctx, artifact)
	if err != nil {
		release()
		return doc, nil, err
	}
	return doc, release, nil
}
func revisionMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func revisionCommentIDs(v any) ([]string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var ids []string
	if json.Unmarshal(raw, &ids) != nil || len(ids) == 0 || len(ids) > 100 {
		return nil, fmt.Errorf("select between 1 and 100 current comments")
	}
	for n, id := range ids {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("select saved comments")
		}
		ids[n] = memql.BareShortId(id)
	}
	sort.Strings(ids)
	for n := 1; n < len(ids); n++ {
		if ids[n] == ids[n-1] {
			return nil, fmt.Errorf("select each comment once")
		}
	}
	return ids, nil
}

func (i *Integration) handleRequestDocumentRevision(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := revisionWriter(ctx); err != nil {
		return nil, err
	}
	if run, ok := common.RunFromContext(ctx); ok && run.RunId != "" {
		return nil, fmt.Errorf("a person must prepare the document revision request")
	}
	if i.reviewGoals == nil {
		return nil, fmt.Errorf("document review jobs are unavailable on this node")
	}
	requestID := asString(args["requestId"])
	ids, err := revisionIdentity(ctx, requestID)
	if err != nil {
		return nil, err
	}
	instruction := strings.TrimSpace(asString(args["instruction"]))
	if len(instruction) > 8000 {
		return nil, fmt.Errorf("additional direction must be at most 8000 bytes")
	}
	comments, err := revisionCommentIDs(args["commentIds"])
	if err != nil {
		return nil, err
	}
	ctx = memql.ContextWithFreshRead(ctx)
	doc, release, err := i.lockReviewDocument(ctx, asString(args["artifactId"]))
	if err != nil {
		return nil, err
	}
	defer release()
	version, valid := intArg(args["expectedVersion"])
	if !valid {
		return nil, fmt.Errorf("read the saved document version first")
	}
	// Recover a partial bootstrap using its captured source. A changed document
	// does not turn a lost response into a second request or a new proposal.
	goal, err := i.revisionRow(ctx, "workGoalForOwner", map[string]any{"goalId": ids.GoalID})
	if err != nil {
		return nil, err
	}
	proposal := revisionMap(revisionMap(goal["input"])["proposal"])
	if goal != nil {
		oldIDs, parseErr := revisionCommentIDs(proposal["commentIds"])
		oldVersion, _ := intArg(proposal["version"])
		if parseErr != nil || !reflect.DeepEqual(oldIDs, comments) || proposal["artifactId"] != doc.artifact || proposal["instruction"] != instruction || proposal["revision"] != asString(args["expectedRevision"]) || oldVersion != version {
			return nil, fmt.Errorf("this request identifier was already used for different feedback")
		}
	} else {
		if version != doc.version || asString(args["expectedRevision"]) != doc.revision {
			return nil, fmt.Errorf("the document changed; review its current revision first")
		}
		format := stringField(doc.backing, "format")
		name := stringField(doc.backing, "name")
		if name == "" {
			name = stringField(doc.backing, "title")
		}
		if format != "markdown" && !strings.HasSuffix(strings.ToLower(name), ".md") && !strings.HasSuffix(strings.ToLower(name), ".markdown") {
			return nil, fmt.Errorf("feedback-driven revision currently supports Markdown documents")
		}
		content, err := i.revisionContent(ctx, doc)
		if err != nil {
			return nil, err
		}
		selected := make([]any, 0, len(comments))
		total := 0
		for _, commentID := range comments {
			raw, err := reviewstore.ByID(ctx, i.engine, doc.owner, commentID)
			if err != nil {
				return nil, err
			}
			rows := extractRows(raw)
			if len(rows) != 1 || stringField(rows[0], "purpose") == "note" || memql.BareShortId(stringField(rows[0], "artifactId")) != doc.artifact || stringField(rows[0], "revision") != doc.revision {
				return nil, fmt.Errorf("one selected comment is unavailable or belongs to an earlier revision")
			}
			row := rows[0]
			anchor, err := validateReviewAnchor(row["anchor"])
			if err != nil {
				return nil, err
			}
			if err = i.verifyReviewPassage(ctx, doc, anchor); err != nil {
				return nil, err
			}
			total += len(stringField(row, "body"))
			if total > 64000 {
				return nil, fmt.Errorf("select less feedback for one revision request")
			}
			selected = append(selected, map[string]any{"id": commentID, "authorUserId": memql.BareShortId(stringField(row, "authorUserId")), "body": stringField(row, "body"), "anchor": anchor})
		}
		proposal = map[string]any{"reviewType": documentReviewType, "requestId": requestID, "artifactId": doc.artifact, "sourceId": memql.BareShortId(doc.source), "documentKind": doc.kind, "revision": doc.revision, "version": doc.version, "name": name, "format": "markdown", "content": content, "blobURL": stringField(doc.backing, "blobUrl"), "commentIds": comments, "comments": selected, "instruction": instruction}
		// Bind a retry to the same owner's immediately preceding failed or stopped request.
		// DSL chooses which completed evidence receipts can be reused; a changed
		// source or feedback never inherits this link.
		prior, err := i.revisionRow(ctx, "workDocumentRevisionRequest", map[string]any{"artifactId": doc.artifact})
		if err != nil {
			return nil, err
		}
		if priorID := asString(prior["requestId"]); priorID != "" {
			previous, previousProposal, previousRun, _, err := i.revisionRequest(ctx, priorID)
			if err != nil {
				return nil, err
			}
			if previousRun["status"] == "failed" || previousRun["status"] == "cancelled" || previousRun["cancelRequested"] == true {
				matches := true
				for _, key := range []string{"artifactId", "sourceId", "revision", "content", "comments", "instruction", "amendment"} {
					if workstate.ArtifactHash(map[string]any{key: proposal[key]}) != workstate.ArtifactHash(map[string]any{key: previousProposal[key]}) {
						matches = false
						break
					}
				}
				if matches {
					proposal["previousRunId"] = previous.RunID
				}
			}
		}
	}
	ac, _ := auth.AccessFromContext(ctx)
	receipt, err := i.reviewGoals.OpenAnalysisGoal(ctx, "library-revision:"+requestID, work.DirectGoal{
		OwnerUserId: ac.UserId, Statement: "Revise " + asString(proposal["name"]), AutomationName: documentRevisionTemplate,
		RequestedVia: "library", TriggeredBy: "document-review", Ceilings: map[string]any{"maxModelCalls": 16},
		Input: map[string]any{"requestId": requestID, "proposal": proposal},
	})
	if err != nil {
		return nil, err
	}
	return reviewResult(map[string]any{"goalId": receipt.GoalID, "runId": receipt.RunID, "requestId": requestID, "proposal": proposal})
}

// ValidateRevisionProposal is used by the shared Nexus decision path as well
// as by execution. A valid hash alone cannot attest still-current source bytes.
func (i *Integration) ValidateRevisionProposal(ctx context.Context, proposal map[string]any) error {
	if err := revisionWriter(ctx); err != nil {
		return err
	}
	ctx = memql.ContextWithFreshRead(ctx)
	doc, release, err := i.lockReviewDocument(ctx, asString(proposal["artifactId"]))
	if err != nil {
		return err
	}
	defer release()
	if err := i.validateCurrentRevision(ctx, proposal); err != nil {
		return err
	}
	version, valid := intArg(proposal["version"])
	if !valid || version != doc.version || proposal["revision"] != doc.revision || proposal["sourceId"] != memql.BareShortId(doc.source) || proposal["documentKind"] != doc.kind {
		return fmt.Errorf("the document changed; prepare a new request against the current revision")
	}
	content, err := i.revisionContent(ctx, doc)
	if err != nil {
		return err
	}
	if content != proposal["content"] {
		return fmt.Errorf("the saved document bytes changed; prepare a new revision request")
	}
	return nil
}

func (i *Integration) revisionRequest(ctx context.Context, requestID string) (work.ReviewGoalReceipt, map[string]any, map[string]any, map[string]any, error) {
	ids, err := revisionIdentity(ctx, requestID)
	if err != nil {
		return ids, nil, nil, nil, err
	}
	goal, err := i.revisionRow(ctx, "workGoalForOwner", map[string]any{"goalId": ids.GoalID})
	if err != nil {
		return ids, nil, nil, nil, err
	}
	if goal == nil || asString(goal["requestFingerprint"]) == "" {
		return ids, nil, nil, nil, fmt.Errorf("revision request is unavailable")
	}
	proposal := revisionMap(revisionMap(goal["input"])["proposal"])
	if proposal["reviewType"] != documentReviewType || proposal["requestId"] != requestID {
		return ids, nil, nil, nil, fmt.Errorf("revision request does not match its receipt")
	}
	// A saved request never grants continued access to a revoked document.
	if _, err = i.reviewDocument(ctx, asString(proposal["artifactId"])); err != nil {
		return ids, nil, nil, nil, err
	}
	run, err := i.revisionRow(ctx, "workRunForOwner", map[string]any{"runId": ids.RunID})
	if err != nil {
		return ids, nil, nil, nil, err
	}
	approvals, err := i.revisionRows(ctx, "workApprovalsForOwnedRun", map[string]any{"runId": ids.RunID})
	var approval map[string]any
	ids.ApprovalID = ""
	for _, candidate := range approvals {
		subject := revisionMap(candidate["subject"])
		if candidate["kind"] == workstate.ApprovalKindPlanReview && subject["reviewType"] == documentReviewType && subject["requestId"] == requestID {
			if approval != nil {
				return ids, nil, nil, nil, fmt.Errorf("revision has conflicting approval receipts")
			}
			approval = candidate
			ids.ApprovalID = memql.BareShortId(asString(candidate["id"]))
		}
	}
	return ids, proposal, run, approval, err
}

func (i *Integration) revisionRows(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		return nil, err
	}
	raw, err := i.engine.Execute(ctx, "query "+call)
	if err != nil {
		return nil, err
	}
	return extractRows(raw), nil
}

func (i *Integration) handleDocumentRevisionStatus(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ids, captured, run, approval, err := i.revisionRequest(memql.ContextWithFreshRead(ctx), asString(args["requestId"]))
	if err != nil {
		return nil, err
	}
	proposal := captured
	if approval != nil {
		proposal = revisionMap(approval["subject"])
	}
	items, err := revisionItems(proposal)
	if err != nil {
		return nil, err
	}
	successor, err := i.revisionRow(memql.ContextWithFreshRead(ctx), "workDocumentRevisionAmendment", map[string]any{"requestId": args["requestId"]})
	if err != nil {
		return nil, err
	}
	drafts, err := i.revisionRows(ctx, "workDraftsForOwnerRun", map[string]any{"runId": ids.RunID})
	if err != nil {
		return nil, err
	}
	var draft map[string]any
	if len(drafts) > 0 {
		draft = revisionMap(revisionMap(drafts[0]["data"])["execution"])
	}
	return reviewResult(map[string]any{"draft": draft, "cancelRequested": run["cancelRequested"], "items": items, "proposalHash": workstate.ArtifactHash(proposal), "answer": approval["answer"], "supersededBy": successor["requestId"], "prepared": run != nil, "goalId": ids.GoalID, "runId": ids.RunID, "approvalId": ids.ApprovalID,
		"proposal": proposal, "decision": approval["decision"], "status": run["status"], "errorMessage": run["errorMessage"], "waitingOn": map[string]any{"kind": revisionMap(run["waitingOn"])["kind"], "resumeAt": revisionMap(run["waitingOn"])["resumeAt"]}, "retryCount": revisionMap(run["spent"])["retries"], "result": revisionMap(run["outcome"])["returned"]})
}

// Only a live instance of the captured run can reach the bounded operations.
// Each replica reconstructs authority and inputs from the durable goal/run.
func (i *Integration) revisionRun(ctx context.Context, requestID string) (work.ReviewGoalReceipt, map[string]any, map[string]any, error) {
	ids, captured, run, approval, err := i.revisionRequest(memql.ContextWithFreshRead(ctx), requestID)
	if err != nil {
		return ids, nil, nil, err
	}
	rc, ok := common.RunFromContext(ctx)
	if !ok || memql.BareShortId(rc.RunId) != ids.RunID || memql.BareShortId(rc.GoalId) != ids.GoalID || asString(run["automationName"]) != documentRevisionTemplate || asString(run["status"]) != "running" || run["cancelRequested"] == true {
		return ids, nil, nil, fmt.Errorf("this live run does not own the document revision request")
	}
	if err := revisionWriter(ctx); err != nil {
		return ids, nil, nil, err
	}
	goal, err := i.revisionRow(ctx, "workGoalForOwner", map[string]any{"goalId": ids.GoalID})
	if err != nil {
		return ids, nil, nil, err
	}
	if goal["status"] != "open" {
		return ids, nil, nil, fmt.Errorf("the document revision goal is closed")
	}
	return ids, captured, approval, nil
}

func (i *Integration) handleRevisionInput(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ids, captured, _, err := i.revisionRun(ctx, asString(args["requestId"]))
	if err != nil {
		return nil, err
	}
	if err = i.ValidateRevisionProposal(ctx, captured); err != nil {
		return nil, err
	}
	promptSource := captured
	if amendment := revisionMap(captured["amendment"]); len(amendment) > 0 {
		promptSource = amendmentCapture(captured, amendment)
	}
	passages, err := revisionPassages(promptSource)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(passages)
	if err != nil {
		return nil, err
	}
	return reviewResult(map[string]any{"runId": ids.RunID, "previousRunId": captured["previousRunId"], "content": captured["content"], "passages": passages, "passagesJSON": string(encoded), "instruction": captured["instruction"], "amendment": captured["amendment"]})
}

func (i *Integration) handleRevisionProposal(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	_, captured, _, err := i.revisionRun(ctx, asString(args["requestId"]))
	if err != nil {
		return nil, err
	}
	proposal, err := buildAmendedRevisionProposal(captured, args["response"])
	if err != nil {
		return nil, err
	}
	attribution, err := i.revisionAttribution(ctx, captured, proposal)
	if err != nil {
		return nil, err
	}
	proposal["attribution"] = attribution
	return reviewResult(map[string]any{"proposal": proposal, "changed": proposal["revisedContent"] != captured["content"], "summary": proposal["summary"]})
}

func (i *Integration) handleReviewRevision(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ids, captured, existing, err := i.revisionRun(ctx, asString(args["requestId"]))
	if err != nil {
		return nil, err
	}
	proposal := revisionMap(args["proposal"])
	if err = validateRevisionResult(captured, proposal); err != nil {
		return nil, err
	}
	if existing == nil {
		measured, err := i.revisionAttribution(ctx, captured, proposal)
		if err != nil {
			return nil, err
		}
		if workstate.ArtifactHash(measured) != workstate.ArtifactHash(revisionMap(proposal["attribution"])) {
			return nil, fmt.Errorf("the proposal's model attribution differs from the recorded calls")
		}
	}
	if proposal["revisedContent"] == captured["content"] {
		return nil, fmt.Errorf("there are no proposed changes to approve")
	}
	if checkpoint, ok := ctx.Value(revisionCheckpointKey{}).(string); ok && checkpoint != ids.ApprovalID {
		return nil, fmt.Errorf("the document review checkpoint changed")
	}
	if existing != nil && workstate.ArtifactHash(revisionMap(existing["subject"])) != workstate.ArtifactHash(proposal) {
		return nil, fmt.Errorf("this request already has a different proposal; submit new feedback")
	}
	if err = i.ValidateRevisionProposal(ctx, proposal); err != nil {
		return nil, err
	}
	ac, _ := auth.AccessFromContext(ctx)
	approvalID, err := i.reviewGoals.AskPlanReview(ctx, ac.UserId, ids.RunID, proposal, asString(args["question"]))
	if err != nil {
		return nil, err
	}
	approval, err := i.revisionRow(memql.ContextWithFreshRead(ctx), "workApprovalForOwner", map[string]any{"approvalId": approvalID})
	if err != nil {
		return nil, err
	}
	if approval["decision"] == "approved" {
		return reviewResult(map[string]any{"approvalId": approvalID})
	}
	if approval["decision"] == "rejected" {
		return nil, fmt.Errorf("the proposed changes were declined")
	}
	return nil, &workstate.HumanWait{ApprovalID: approvalID}
}

// This operation only applies an already validated and approved result. AI,
// sequencing and the review question belong to the journaled DSL template.
func (i *Integration) handleExecuteDocumentRevision(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ids, captured, approval, err := i.revisionRun(ctx, asString(args["requestId"]))
	if err != nil {
		return nil, err
	}
	proposal := revisionMap(approval["subject"])
	if approval["decision"] != "approved" || approval["artifactHash"] != workstate.ArtifactHash(proposal) {
		return nil, fmt.Errorf("this exact revision proposal has not been approved")
	}
	if err = validateRevisionResult(captured, proposal); err != nil {
		return nil, err
	}
	selected, err := selectedRevisionProposal(proposal, revisionMap(approval["answer"]))
	if err != nil {
		return nil, err
	}
	return i.applyRevision(ctx, ids, selected)
}
