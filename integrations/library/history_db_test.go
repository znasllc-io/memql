package library

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func historyResult(t *testing.T, rows []memorynodes.MemoryNode, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("missing receipt: %d", len(rows))
	}
	var result map[string]any
	if err = json.Unmarshal(rows[0].Payload, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func TestHistoryBranchesAcrossReplicas(t *testing.T) {
	f := newRevisionDB(t)
	if f == nil {
		return
	}
	original := "# Original\n\nAlice keeps **this**.\n"
	artifact, doc := f.document(original)
	for n := 1; n <= 53; n++ {
		_, err := f.first.handleEditDocument(f.ctx, map[string]any{"documentId": doc.source, "content": fmt.Sprintf("# Version %d\n", n), "expectedVersion": n - 1}, 0)
		if err != nil {
			t.Fatalf("edit %d: %v", n, err)
		}
	}
	rows, err := f.second.handleDocumentHistory(f.ctx, map[string]any{"artifactId": artifact}, 0)
	page := historyResult(t, rows, err)
	if page["hasMore"] != true || len(page["versions"].([]any)) != 51 {
		t.Fatalf("first page: %v", page)
	}
	rows, err = f.second.handleDocumentHistory(f.ctx, map[string]any{"artifactId": artifact, "beforeVersion": page["beforeVersion"]}, 0)
	older := historyResult(t, rows, err)
	versions := older["versions"].([]any)
	if older["hasMore"] != false || len(versions) != 3 || revisionMap(versions[2])["version"] != float64(0) {
		t.Fatalf("earliest page: %v", older)
	}
	rows, err = f.second.handleDocumentVersion(f.ctx, map[string]any{"artifactId": artifact, "versionNumber": 0}, 0)
	snapshot := historyResult(t, rows, err)
	if snapshot["content"] != original {
		t.Fatal("original content was not retained")
	}
	args := map[string]any{"artifactId": artifact, "versionNumber": 0, "revision": snapshot["revision"], "name": "Alternative", "requestId": "fork-original-request"}
	var wg sync.WaitGroup
	results := make(chan map[string]any, 2)
	failures := make(chan error, 2)
	for _, lib := range []*Integration{f.first, f.second} {
		wg.Add(1)
		go func(lib *Integration) {
			defer wg.Done()
			out, e := lib.handleForkDocumentVersion(f.ctx, args, 0)
			if e != nil {
				failures <- e
				return
			}
			var result map[string]any
			e = json.Unmarshal(out[0].Payload, &result)
			if e != nil {
				failures <- e
				return
			}
			results <- result
		}(lib)
	}
	wg.Wait()
	close(failures)
	for e := range failures {
		t.Fatal(e)
	}
	a, b := <-results, <-results
	if a["artifactId"] != b["artifactId"] {
		t.Fatal("retry created another branch")
	}
	branch, e := f.second.reviewDocument(memql.ContextWithFreshRead(f.ctx), asString(a["artifactId"]))
	if e != nil {
		t.Fatal(e)
	}
	if branch.backing["body"] != original || branch.version != 0 {
		t.Fatalf("wrong branch snapshot: %v", branch.backing)
	}
	_, err = f.second.handleEditDocument(f.ctx, map[string]any{"documentId": branch.source, "content": "# Independent change\n", "expectedVersion": 0, "expectedRevision": branch.revision}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Lost-response retry after editing must recover the same branch, not reset it.
	rows, err = f.first.handleForkDocumentVersion(f.ctx, args, 0)
	retry := historyResult(t, rows, err)
	if retry["artifactId"] != a["artifactId"] {
		t.Fatal("lost branch receipt")
	}
	advanced, err := f.first.reviewDocument(memql.ContextWithFreshRead(f.ctx), branch.artifact)
	if err != nil || advanced.backing["body"] != "# Independent change\n" || advanced.version != 1 {
		t.Fatalf("retry overwrote branch: %v %v", advanced, err)
	}
	current, err := f.first.reviewDocument(memql.ContextWithFreshRead(f.ctx), artifact)
	if err != nil || current.version != 53 || current.backing["body"] != "# Version 53\n" {
		t.Fatal("branch changed parent")
	}
	rows, err = f.first.handleDocumentHistory(f.ctx, map[string]any{"artifactId": branch.artifact}, 0)
	history := historyResult(t, rows, err)
	if history["parentArtifactId"] != memql.BareShortId(artifact) || history["parentVersion"] != float64(0) || len(history["branches"].([]any)) != 1 {
		t.Fatalf("lost branch lineage: %v", history)
	}
	for _, denied := range []context.Context{revisionActor(f.owner+"-outsider", auth.RoleWriter), revisionActor(f.owner, auth.RoleReader)} {
		if _, err = f.first.handleForkDocumentVersion(denied, args, 0); err == nil {
			t.Fatal("unauthorized fork accepted")
		}
	}
	if _, err = f.first.handleDocumentVersion(revisionActor(f.owner+"-outsider", auth.RoleWriter), map[string]any{"artifactId": artifact, "versionNumber": 0}, 0); err == nil {
		t.Fatal("outsider read old content")
	}
	args["revision"] = "forged"
	args["requestId"] = "new-forged-request"
	if _, err = f.first.handleForkDocumentVersion(f.ctx, args, 0); err == nil {
		t.Fatal("forged revision accepted")
	}
}

func TestHistoryForkReadsOwnedFileBytes(t *testing.T) {
	f := newRevisionDB(t)
	if f == nil {
		return
	}
	fileID := f.owner + "-imported"
	f.query(f.engine, f.ctx, "mutation", "createLibraryFile", map[string]any{"fileId": fileID, "name": "Imported.md", "mimeType": "text/markdown", "size": 19, "blobUrl": "https://store.example/imported.md", "source": "uploaded", "format": "markdown"})
	artifact := f.query(f.engine, f.ctx, "mutation", "createArtifact", map[string]any{"sourceConceptRef": fileID, "ownerUserId": f.owner, "lens": "artifact", "kind": "file", "source": "uploaded", "title": "Imported.md", "format": "markdown"})[0]
	fetcher := &fakeBlobStreamer{data: []byte("# Imported\n\nKeep me.\n")}
	f.second.SetBlobFetcher(fetcher)
	args := map[string]any{"artifactId": asString(artifact["id"]), "versionNumber": 1, "revision": "file:1", "name": "Imported branch", "requestId": "imported-branch-request"}
	if _, err := f.second.handleForkDocumentVersion(revisionActor(f.owner+"-other", auth.RoleWriter), args, 0); err == nil || fetcher.streams != 0 {
		t.Fatal("file read before authorization")
	}
	rows, err := f.second.handleForkDocumentVersion(f.ctx, args, 0)
	result := historyResult(t, rows, err)
	doc, err := f.first.reviewDocument(memql.ContextWithFreshRead(f.ctx), asString(result["artifactId"]))
	if err != nil || doc.backing["body"] != string(fetcher.data) {
		t.Fatalf("file bytes lost: %v %v", doc, err)
	}
	// The first replica has no blob fetcher. The durable seed must suffice.
	rows, err = f.first.handleForkDocumentVersion(f.ctx, args, 0)
	retry := historyResult(t, rows, err)
	if retry["artifactId"] != result["artifactId"] {
		t.Fatal("file fork receipt changed")
	}
}

func TestPersonalDocumentNotesNeverEnterAIRevision(t *testing.T) {
	f := newRevisionDB(t)
	if f == nil {
		return
	}
	artifact, doc := f.document("# Guide\n\nA selected paragraph.\n")
	args := map[string]any{"artifactId": artifact, "expectedVersion": doc.version, "expectedRevision": doc.revision, "anchor": map[string]any{"kind": "markdown", "startLine": 2, "endLine": 3, "sourceQuote": "A selected paragraph.", "quote": "selected"}, "body": "Remember to discuss this privately.", "requestId": "personal-note-request"}
	rows, err := f.first.handleAddDocumentNote(f.ctx, args, 0)
	saved := historyResult(t, rows, err)
	rows, err = f.second.handleAddDocumentNote(f.ctx, args, 0)
	retry := historyResult(t, rows, err)
	if saved["commentId"] != retry["commentId"] {
		t.Fatal("duplicate note")
	}
	rows, err = f.second.handleDocumentNotes(f.ctx, map[string]any{"artifactId": artifact}, 0)
	notes := historyResult(t, rows, err)
	if len(notes["notes"].([]any)) != 1 {
		t.Fatal("personal note missing")
	}
	rows, err = f.second.handleDocumentReview(f.ctx, map[string]any{"artifactId": artifact}, 0)
	feedback := historyResult(t, rows, err)
	if len(feedback["comments"].([]any)) != 0 {
		t.Fatal("note leaked into feedback")
	}
	if _, err = f.second.handleRequestDocumentRevision(f.ctx, map[string]any{"artifactId": artifact, "expectedVersion": doc.version, "expectedRevision": doc.revision, "commentIds": []string{asString(saved["commentId"])}, "requestId": "must-refuse-personal-note"}, 0); err == nil {
		t.Fatal("personal note accepted as AI feedback")
	}
	if _, err = f.first.handleDocumentNotes(revisionActor(f.owner+"-other", auth.RoleWriter), map[string]any{"artifactId": artifact}, 0); err == nil {
		t.Fatal("outsider read personal notes")
	}
	current, err := f.second.reviewDocument(memql.ContextWithFreshRead(f.ctx), artifact)
	if err != nil || current.version != doc.version || current.revision != doc.revision {
		t.Fatal("saving a note edited the document")
	}
	if _, err = f.second.handleAddDocumentComment(f.ctx, args, 0); err == nil {
		t.Fatal("idempotency receipt changed its purpose")
	}
}
