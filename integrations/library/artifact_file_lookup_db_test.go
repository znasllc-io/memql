package library

import (
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

func testArtifactForFileResolvesBareReceiptAcrossEngines(t *testing.T, f *revisionDB) {
	fileID := f.owner + "-upload"
	for _, kind := range []string{"file", "generated_output"} {
		concept := "file"
		if kind == "generated_output" {
			concept = "generatedOutput"
		}
		f.query(f.engine, f.ctx, "mutation", "createArtifact", map[string]any{
			"sourceConceptRef": "v1:library:" + concept + ":" + fileID,
			"ownerUserId":      f.owner, "lens": "artifact", "kind": kind,
			"source": "uploaded", "title": "Test upload", "format": "markdown", "mimeType": "text/markdown",
		})
	}
	rows := f.query(f.other, f.ctx, "query", "libraryArtifactForFile", map[string]any{"fileId": fileID})
	if len(rows) != 1 || rows[0]["kind"] != "file" {
		t.Fatalf("bare upload did not resolve its own concept: %#v", rows)
	}
	other := revisionActor(f.owner+"-other", auth.RoleWriter)
	if rows = f.query(f.other, other, "query", "libraryArtifactForFile", map[string]any{"fileId": fileID}); len(rows) != 0 {
		t.Fatal("another owner could read the upload")
	}
}
