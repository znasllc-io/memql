package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// A developer reads a pipeline log of a run they do not own through
// GET /artifacts/{id}/content?pipelineRun=<runId> (design record
// docs/superpowers/specs/2026-11-24-pipelines-developer-view-design.md).
// Against a real engine, because every check is a DSL fact: who may read the
// run (pipelineRunView's floor), and which artifact is that run's log.
func TestADeveloperReadsAPipelineLogThroughTheRunPointer(t *testing.T) {
	eng, _ := libraryDBEngine(t)
	store := NewEngineLibraryStore(replicaFileExecutor{eng}, libraryGatePool(t))
	blob := newFakeBlob()
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "memql-library", Uploader: blob, Downloader: blob,
		Store: store, UserQuotaBytes: -1, PromotionWait: 50 * time.Millisecond,
	})
	marker := fmt.Sprintf("pipeline-log-%d", time.Now().UnixNano())
	owner, dev := marker+"-owner", marker+"-dev"
	ownerCtx := wireCallerCtx(owner)
	devCtx := auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: dev}),
		&auth.AccessContext{UserId: dev, Role: auth.RoleDeveloper})
	runID, workRun := marker+"-run", marker+"-work"

	// The run, as the runner writes it: under its owner, stamped internal, tied to no account.
	for _, m := range []struct {
		name string
		args map[string]any
	}{
		{"createPipelineRun", map[string]any{"runId": runID, "pipelineId": marker, "repository": "acme/" + marker, "sha": "abc123",
			"mode": "full", "event": "push", "runKey": marker, "attempt": 1, "trigger": "webhook", "status": "completed",
			"queuedAt": time.Now().UTC().Format(time.RFC3339Nano)}},
		{"updatePipelineRun", map[string]any{"runId": runID, "workRunId": workRun}},
	} {
		call, err := dslCall(m.name, m.args)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := eng.Execute(auth.ContextWithInternalOrigin(ownerCtx), "mutation "+call); err != nil {
			t.Fatalf("%s: %v", m.name, err)
		}
	}

	// The owner's files: this run's log, another run's log, an upload this run produced.
	file := func(name, source, producedBy string) string {
		t.Helper()
		up := postUpload(t, h, owner, name, "text/plain", []byte(name+" FAIL\n"), nil)
		if up.Code != http.StatusCreated {
			t.Fatalf("upload %s answered %d: %s", name, up.Code, up.Body.String())
		}
		fileId := decodeUpload(t, up).FileId
		if _, err := store.exec(ownerCtx, "createArtifact", map[string]any{
			"sourceConceptRef": LibraryFileConceptRef(fileId), "ownerUserId": owner, "lens": "artifact", "kind": libraryKindFile,
			"source": source, "title": name, "format": "text", "mimeType": "text/plain", "producedByRunId": producedBy,
		}); err != nil {
			t.Fatalf("promote %s: %v", name, err)
		}
		artifactId, err := store.ArtifactForFile(memql.ContextWithFreshRead(ownerCtx), fileId)
		if err != nil || artifactId == "" {
			t.Fatalf("artifact for %s: %q %v", name, artifactId, err)
		}
		return artifactId
	}
	runLog := file("tests.unit.log", "pipeline", workRun)
	otherRunLog := file("other.log", "pipeline", marker+"-other-work")
	upload := file("notes.txt", "uploaded", workRun)

	get := func(ctx context.Context, artifactId, run string) *httptest.ResponseRecorder {
		target := "/artifacts/" + artifactId + "/content"
		if run != "" {
			target += "?pipelineRun=" + url.QueryEscape(run)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx))
		return rec
	}
	if got := get(devCtx, runLog, runID); got.Code != http.StatusOK || got.Body.String() != "tests.unit.log FAIL\n" {
		t.Fatalf("the developer's read of the run's log answered %d: %q", got.Code, got.Body.String())
	}
	for name, got := range map[string]*httptest.ResponseRecorder{
		"without the pointer":      get(devCtx, runLog, ""),
		"another run's log":        get(devCtx, otherRunLog, runID),
		"a non-pipeline artifact":  get(devCtx, upload, runID),
		"a caller below developer": get(wireCallerCtx(marker+"-writer"), runLog, runID),
	} {
		if got.Code != http.StatusNotFound {
			t.Errorf("%s answered %d, want 404", name, got.Code)
		}
	}
}
