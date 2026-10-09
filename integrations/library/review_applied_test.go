package library

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
)

func TestAppliedFeedbackCannotBeDeletedAcrossReplicas(t *testing.T) {
	f := newRevisionDB(t)
	if f == nil {
		return
	}
	artifact, doc := f.document("# Guide\n\nOriginal paragraph.\n\nOther paragraph.\n")
	// Audit history is internal even for a document's author.
	call, _ := langparser.RenderCall("workDocumentApprovedRevisions", map[string]any{"artifactId": memql.BareShortId(artifact), "versions": []int{doc.version}})
	if _, err := f.engine.Execute(f.ctx, "query "+call); err == nil {
		t.Fatal("client read internal review receipts")
	}
	// Put the actual application beyond the first page. Approval alone, even a
	// hundred times, must not protect feedback without a matching saved version.
	for n := 0; n < 100; n++ {
		id := fmt.Sprintf("%s-old-%d", f.owner, n)
		f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "createWorkApproval", map[string]any{"approvalId": id, "runId": "old-run", "kind": "planReview", "artifactHash": "fixture", "requestedAt": time.Now().UTC().Format(time.RFC3339Nano), "subject": map[string]any{"reviewType": "library-document", "artifactId": memql.BareShortId(artifact), "sourceId": memql.BareShortId(doc.source), "documentKind": doc.kind, "version": doc.version}})
		f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "decideWorkApproval", map[string]any{"approvalId": id, "decision": "approved", "decidedBy": f.owner, "decidedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	}
	ids := map[string]string{}
	for _, purpose := range []string{"feedback", "extension", "declined", "note"} {
		anchor := map[string]any{"kind": "document"}
		if purpose == "extension" {
			anchor = map[string]any{"kind": "document-end"}
		}
		if purpose == "note" {
			anchor = map[string]any{"kind": "markdown", "startLine": 2, "endLine": 3, "sourceQuote": "Original paragraph.", "quote": "paragraph"}
		}
		add := f.first.handleAddDocumentComment
		if purpose == "note" {
			add = f.first.handleAddDocumentNote
		}
		rows, err := add(f.ctx, map[string]any{"artifactId": artifact, "expectedVersion": doc.version, "expectedRevision": doc.revision, "anchor": anchor, "body": "Clarify " + purpose, "requestId": "protect-" + purpose}, 0)
		ids[purpose] = asString(historyResult(t, rows, err)["commentId"])
	}
	request := "protect-applied-review"
	rows, err := f.first.handleRequestDocumentRevision(f.ctx, map[string]any{"artifactId": artifact, "expectedVersion": doc.version, "expectedRevision": doc.revision, "commentIds": []string{ids["feedback"], ids["extension"], ids["declined"]}, "requestId": request}, 0)
	historyResult(t, rows, err)
	f.ai.answer = revisionAnswer{Summary: "Clarify and extend", Edits: []revisionReplacement{
		{Before: "Original paragraph.", After: "Clear paragraph.", Reason: "Clarify", CommentIDs: []string{ids["feedback"]}},
		{Before: "Other paragraph.", After: "Other paragraph.\n\n## Sources\n\nNew references.", Reason: "Extend", CommentIDs: []string{ids["extension"]}},
		{Before: "# Guide", After: "# Renamed guide", Reason: "Rename", CommentIDs: []string{ids["declined"]}},
	}}
	var wait *workstate.HumanWait
	if err = f.execute(f.engine, request, false); !errors.As(err, &wait) {
		t.Fatalf("prepare: %v", err)
	}
	// Prime the OTHER replica's permission reads before application.
	rows, err = f.second.handleDocumentReview(f.ctx, map[string]any{"artifactId": artifact}, 0)
	historyResult(t, rows, err)
	receipt, _, _, approval, err := f.first.revisionRequest(memql.ContextWithFreshRead(f.ctx), request)
	if err != nil {
		t.Fatal(err)
	}
	proposal := revisionMap(approval["subject"])
	items, err := revisionItems(proposal)
	if err != nil {
		t.Fatal(err)
	}
	accepted := []string{}
	for _, item := range items {
		if item.CommentIDs[0] != ids["declined"] {
			accepted = append(accepted, item.ID)
		}
	}
	f.query(f.other, f.ctx, "query", "decideApproval", map[string]any{"approvalId": memql.BareShortId(asString(approval["id"])), "decision": "approved", "answer": map[string]any{"proposalHash": workstate.ArtifactHash(proposal), "acceptedItemIds": accepted}})
	if err = f.execute(f.engine, request, true); err != nil {
		t.Fatal(err)
	}
	for _, replica := range []*Integration{f.second, f.first} {
		rows, err = replica.handleDocumentReview(f.ctx, map[string]any{"artifactId": artifact}, 0)
		for _, value := range historyResult(t, rows, err)["comments"].([]any) {
			row := revisionMap(value)
			want := row["id"] != ids["declined"]
			if row["applied"] != want || row["canRemove"] == want {
				t.Fatalf("wrong applied permission: %#v", row)
			}
		}
	}
	// A save remains protected even if final job acknowledgment fails.
	f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "updateWorkRun", map[string]any{"runId": receipt.RunID, "status": "failed", "errorMessage": "lost completion acknowledgment"})
	// A new review does not make the previous applied requests deletable.
	current, err := f.first.reviewDocument(memql.ContextWithFreshRead(f.ctx), artifact)
	if err != nil {
		t.Fatal(err)
	}
	f.submit(artifact, current, map[string]any{"kind": "document"}, "Another request")
	for _, purpose := range []string{"feedback", "extension"} {
		call, _ := langparser.RenderCall("libraryRemoveDocumentAnnotation", map[string]any{"artifactId": artifact, "commentId": ids[purpose]})
		for _, engine := range []*memql.MemQLEngine{f.other, f.engine} {
			if _, err = engine.Execute(f.ctx, "builtin "+call); err == nil || !strings.Contains(err.Error(), "applied feedback") {
				t.Fatalf("%s deletion: %v", purpose, err)
			}
		}
	}
	// Declined requests are outdated too, but were never applied. Notes are personal.
	for _, purpose := range []string{"declined", "note"} {
		result := f.query(f.other, f.ctx, "builtin", "libraryRemoveDocumentAnnotation", map[string]any{"artifactId": artifact, "commentId": ids[purpose]})
		if len(result) != 1 || result[0]["removed"] != true {
			t.Fatalf("%s deletion: %v", purpose, result)
		}
	}
}

func TestSavedRevisionReceiptRequiresExactOperation(t *testing.T) {
	run := "review-run"
	digest := strings.Repeat("a", 64)
	operation := sha256.Sum256([]byte("revision-" + run))
	file := map[string]any{"sha256": digest, "blobUrl": fmt.Sprintf("https://storage/file/revisions/%x/%s/document.md", operation, digest)}
	if !savedRevisionReceipt("file", file, run) || savedRevisionReceipt("file", file, "another-run") {
		t.Fatal("file operation identity lost")
	}
	file["sha256"] = strings.Repeat("b", 64)
	if savedRevisionReceipt("file", file, run) {
		t.Fatal("mismatched bytes accepted")
	}
	if !savedRevisionReceipt("generated_output", map[string]any{"producedByRunId": "v1:work:run:" + run}, run) || savedRevisionReceipt("generated_output", map[string]any{"producedByRunId": "another-run"}, run) {
		t.Fatal("generated document receipt identity lost")
	}
}
