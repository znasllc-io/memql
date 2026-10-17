package library

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/znasllc-io/memql/component/auth"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
)

func testViewerEndInsertionAcrossReplicas(t *testing.T, shared *revisionDB) {
	for _, repair := range []bool{false, true} {
		t.Run(fmt.Sprintf("repair=%t", repair), func(t *testing.T) {
			f := shared.isolatedCase(t)
			f.ctx = revisionActor(f.owner, auth.RoleReader)
			testViewerRewordAndAppend(t, f, repair)
		})
	}
}

func testViewerRewordAndAppend(t *testing.T, f *revisionDB, repair bool) {
	source := "# Coffee\n\nCoffee is enjoyed worldwide.\n"
	artifact, doc := f.document(source)
	// Submit both notes together, as the editor did in the production report.
	rows, err := f.first.handleAddDocumentComment(f.ctx, map[string]any{"artifactId": artifact, "expectedVersion": doc.version, "expectedRevision": doc.revision, "anchor": map[string]any{"kind": "markdown", "startLine": 2, "endLine": 3, "quote": "Coffee is enjoyed worldwide.", "sourceQuote": "Coffee is enjoyed worldwide."}, "body": "Change this wording", "requestId": "wording-" + f.owner}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var note map[string]any
	_ = json.Unmarshal(rows[0].Payload, &note)
	args, endNote := f.submit(artifact, doc, map[string]any{"kind": "document-end"}, "Give me a latte recipe")
	args["commentIds"] = []string{asString(note["commentId"]), endNote}
	args["requestId"] = "combined-" + f.owner
	if _, err = f.first.handleRequestDocumentRevision(f.ctx, args, 0); err != nil {
		t.Fatal(err)
	}
	request := asString(args["requestId"])
	f.ai.answer = revisionAnswer{Summary: "Reworded and added the latte recipe", Edits: []revisionReplacement{
		{Before: "Coffee is enjoyed worldwide.", After: "People around the world enjoy coffee.", Reason: "Rephrase", CommentIDs: []string{asString(note["commentId"])}},
		{Before: "", After: "\n## Latte\n\nCombine espresso and steamed milk.\n", Reason: "Add recipe", CommentIDs: []string{endNote}},
	}}
	if repair {
		// Production sequence: a no-op reword + EOF insertion, then a repair
		// repeating the reword both alone and inside its combined replacement.
		first := f.ai.answer
		first.Edits = append([]revisionReplacement(nil), first.Edits...)
		first.Edits[0].After = first.Edits[0].Before
		f.ai.firstAnswer = &first
		f.ai.completionReply = "incomplete: selected wording was left unchanged"
		f.ai.answer.Edits[1].Before = f.ai.answer.Edits[0].Before
		f.ai.answer.Edits[1].After = f.ai.answer.Edits[0].After + "\n\n## Latte\n\nCombine espresso and steamed milk."
		f.ai.answer.Edits[1].CommentIDs = []string{asString(note["commentId"]), endNote}
	}
	var wait *workstate.HumanWait
	if err = f.execute(f.other, request, false); !errors.As(err, &wait) {
		t.Fatalf("viewer did not reach approval: %v", err)
	}
	wantCalls, wantRepairs := int32(1), int32(0)
	if repair {
		wantCalls, wantRepairs = 2, 1
	}
	if f.ai.calls.Load() != wantCalls || f.ai.repairCalls.Load() != wantRepairs {
		t.Fatalf("unexpected repair calls: %d / %d", f.ai.calls.Load(), f.ai.repairCalls.Load())
	}
	unchanged, err := f.second.reviewDocument(memql.ContextWithFreshRead(f.ctx), artifact)
	if err != nil || asString(unchanged.backing["body"]) != source {
		t.Fatal("document changed before human approval")
	}
	f.query(f.engine, f.ctx, "query", "decideApproval", map[string]any{"approvalId": wait.ApprovalID, "decision": "approved", "answer": f.accepted(request)})
	if err = f.execute(f.engine, request, true); err != nil {
		t.Fatal(err)
	}
	changed, err := f.second.reviewDocument(memql.ContextWithFreshRead(f.ctx), artifact)
	want := "# Coffee\n\nPeople around the world enjoy coffee.\n\n## Latte\n\nCombine espresso and steamed milk.\n"
	if err != nil || asString(changed.backing["body"]) != want {
		t.Fatalf("approved result: %q / %v", changed.backing["body"], err)
	}
}

func testViewerProvenanceAnchorAcrossReplicas(t *testing.T, shared *revisionDB) {
	f := shared.isolatedCase(t)
	f.ctx = revisionActor(f.owner, auth.RoleReader)
	source := "---\nstatement: \"Create a file with Grind fresh beans before brewing.\"\n---\n# Coffee\n\nGrind fresh beans before brewing.\n"
	artifact, doc := f.document(source)
	args, note := f.submit(artifact, doc, map[string]any{"kind": "document-end"}, "Append Enjoy: Serve the coffee warm.")
	if _, err := f.first.handleRequestDocumentRevision(f.ctx, args, 0); err != nil {
		t.Fatal(err)
	}
	request := asString(args["requestId"])
	f.ai.answer = revisionAnswer{Summary: "Add Enjoy", Edits: []revisionReplacement{{Before: "Grind fresh beans before brewing.", After: "Grind fresh beans before brewing.\n\n## Enjoy\n\nServe the coffee warm.", Reason: "Append the requested section", CommentIDs: []string{note}}}}
	var wait *workstate.HumanWait
	if err := f.execute(f.other, request, false); !errors.As(err, &wait) {
		t.Fatalf("first attempt did not reach approval: %v", err)
	}
	if f.ai.calls.Load() != 1 || f.ai.repairCalls.Load() != 0 {
		t.Fatal("unnecessary repair for a unique body passage")
	}
	f.query(f.engine, f.ctx, "query", "decideApproval", map[string]any{"approvalId": wait.ApprovalID, "decision": "approved", "answer": f.accepted(request)})
	if err := f.execute(f.engine, request, true); err != nil {
		t.Fatal(err)
	}
	changed, err := f.second.reviewDocument(memql.ContextWithFreshRead(f.ctx), artifact)
	want := source[:len(source)-1] + "\n\n## Enjoy\n\nServe the coffee warm.\n"
	if err != nil || asString(changed.backing["body"]) != want {
		t.Fatalf("applied content: %v %v", changed.backing, err)
	}
}
