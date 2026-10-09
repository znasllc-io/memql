package library

import (
	"errors"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	sdk "github.com/znasllc-io/memql/sdk/go/client"
)

func TestDeleteDocumentAnnotationsAcrossReplicas(t *testing.T) {
	f := newRevisionDB(t)
	if f == nil {
		return
	}
	artifact, doc := f.document("# Guide\n\nA selected paragraph.\n")
	anchor := map[string]any{"kind": "markdown", "startLine": 2, "endLine": 3, "sourceQuote": "A selected paragraph.", "quote": "selected"}
	for _, purpose := range []string{"feedback", "extension", "note"} {
		t.Run(purpose, func(t *testing.T) {
			selection := anchor
			if purpose == "extension" {
				selection = map[string]any{"kind": "document-end"}
			}
			args := map[string]any{"artifactId": artifact, "expectedVersion": doc.version, "expectedRevision": doc.revision, "anchor": selection, "body": "Keep this thought.", "requestId": "delete-" + purpose}
			add := f.first.handleAddDocumentComment
			if purpose == "note" {
				add = f.first.handleAddDocumentNote
			}
			rows, err := add(f.ctx, args, 0)
			saved := historyResult(t, rows, err)
			id := asString(saved["commentId"])
			remove := map[string]any{"artifactId": artifact, "commentId": id}
			if _, err = f.second.handleRemoveDocumentAnnotation(revisionActor(f.owner+"-outsider", auth.RoleWriter), remove, 0); err == nil {
				t.Fatal("outsider deleted annotation")
			}
			if _, err = f.second.handleRemoveDocumentAnnotation(revisionActor(f.owner, auth.RoleReader), remove, 0); err == nil {
				t.Fatal("reader deleted annotation")
			}
			// Cluster visibility does not make another person's private note yours.
			if _, err = f.second.handleRemoveDocumentAnnotation(revisionActor(f.owner+"-admin", auth.RoleOwner), remove, 0); err == nil {
				t.Fatal("another author deleted annotation")
			}
			other, _ := f.document("# Other\n")
			wrong := map[string]any{"artifactId": other, "commentId": id}
			if _, err = f.second.handleRemoveDocumentAnnotation(f.ctx, wrong, 0); err == nil {
				t.Fatal("wrong artifact accepted")
			}
			// Exercise the public, generated call on both replicas. Direct
			// handler tests cannot catch an SDK that drops the supplied IDs.
			call := sdk.LibraryRemoveDocumentAnnotationBuild(sdk.LibraryRemoveDocumentAnnotationArgs{ArtifactId: artifact, CommentId: id})
			for _, replica := range []*memql.MemQLEngine{f.other, f.engine} {
				result, executeErr := replica.Execute(f.ctx, call)
				if executeErr != nil {
					t.Fatalf("generated deletion call: %v", executeErr)
				}
				receipt := extractRows(result)
				if len(receipt) != 1 || receipt[0]["removed"] != true {
					t.Fatalf("missing deletion receipt: %v", receipt)
				}
			}
			rows, err = f.first.handleDocumentReview(f.ctx, map[string]any{"artifactId": artifact}, 0)
			if len(historyResult(t, rows, err)["comments"].([]any)) != 0 {
				t.Fatal("deleted feedback reappeared")
			}
			rows, err = f.first.handleDocumentNotes(f.ctx, map[string]any{"artifactId": artifact}, 0)
			if len(historyResult(t, rows, err)["notes"].([]any)) != 0 {
				t.Fatal("deleted note reappeared")
			}
			if _, err = add(f.ctx, args, 0); err == nil {
				t.Fatal("delayed add resurrected deleted annotation")
			}
			if purpose != "note" {
				if _, err = f.first.handleRequestDocumentRevision(f.ctx, map[string]any{"artifactId": artifact, "expectedVersion": doc.version, "expectedRevision": doc.revision, "commentIds": []string{id}, "requestId": "deleted-request-" + purpose}, 0); err == nil {
					t.Fatal("captured deleted feedback")
				}
			}
		})
	}
	current, err := f.second.reviewDocument(memql.ContextWithFreshRead(f.ctx), artifact)
	if err != nil || current.version != doc.version || current.revision != doc.revision {
		t.Fatal("deletion changed document contents")
	}
}

func TestDeleteProposedFeedbackInvalidatesStaleReview(t *testing.T) {
	f := newRevisionDB(t)
	if f == nil {
		return
	}
	artifact, doc := f.document("# Guide\n\nOriginal paragraph.\n")
	args, id := f.submit(artifact, doc, map[string]any{"kind": "document"}, "Clarify")
	request := asString(args["requestId"])
	f.ai.answer = revisionAnswer{Summary: "Clarify", Edits: []revisionReplacement{{Before: "Original paragraph.", After: "Clear paragraph.", Reason: "Clarify", CommentIDs: []string{id}}}}
	var wait *workstate.HumanWait
	if err := f.execute(f.engine, request, false); !errors.As(err, &wait) {
		t.Fatalf("prepare: %v", err)
	}
	_, captured, _, approval, err := f.first.revisionRequest(memql.ContextWithFreshRead(f.ctx), request)
	if err != nil {
		t.Fatal(err)
	}
	removal := map[string]any{"artifactId": artifact, "commentId": id}
	if _, err = f.second.handleRemoveDocumentAnnotation(f.ctx, removal, 0); err == nil {
		t.Fatal("live proposal deleted before cancellation")
	}
	statusRows, err := f.second.handleDocumentRevisionStatus(f.ctx, map[string]any{"requestId": request}, 0)
	status := historyResult(t, statusRows, err)
	// Stopping a newer editor's review must not allow deletion beneath an older
	// still-live proposal that captured the same feedback.
	concurrent := map[string]any{}
	for key, value := range args {
		concurrent[key] = value
	}
	concurrent["requestId"] = "another-editor-review"
	newer, err := f.first.handleRequestDocumentRevision(f.ctx, concurrent, 0)
	newerReceipt := historyResult(t, newer, err)
	f.query(f.engine, f.ctx, "query", "cancelGoal", map[string]any{"goalId": newerReceipt["goalId"], "reason": "Stop newer review"})
	if _, err = f.second.handleRemoveDocumentAnnotation(f.ctx, removal, 0); err == nil {
		t.Fatal("older live proposal escaped deletion gate")
	}
	f.query(f.engine, f.ctx, "query", "cancelGoal", map[string]any{"goalId": status["goalId"], "reason": "Delete request"})
	rows, err := f.second.handleRemoveDocumentAnnotation(f.ctx, removal, 0)
	historyResult(t, rows, err)
	if err = f.first.ValidateRevisionProposal(f.ctx, captured); err == nil {
		t.Fatal("captured request stayed valid")
	}
	if err = f.first.ValidateRevisionProposal(f.ctx, revisionMap(approval["subject"])); err == nil {
		t.Fatal("stale approval stayed valid")
	}
	rows, err = f.first.handleDocumentRevisionStatus(f.ctx, map[string]any{"requestId": request}, 0)
	status = historyResult(t, rows, err)
	if len(status["removedCommentIds"].([]any)) != 1 {
		t.Fatalf("missing removal in reopened review: %v", status)
	}
	// A new request still works against exactly the same document revision.
	next, _ := f.submit(artifact, doc, map[string]any{"kind": "document-end"}, "Add an appendix")
	if next["requestId"] == request {
		t.Fatal(fmt.Sprint("reused cancelled request"))
	}
}
