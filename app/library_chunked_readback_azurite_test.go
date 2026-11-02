package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/server/uploadsession"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/librarykey"
	"github.com/znasllc-io/memql/integrations/azureblob"
	"github.com/znasllc-io/memql/integrations/library"
)

// library_chunked_readback_azurite_test.go -- a Library file is read back from
// real storage, through the readers a person reaches, by the value its row stores.
//
// The handler suites in component/server use fakes that accept whatever string a
// row names, and integrations/azureblob's Azurite suite streams a full URL it
// composed itself. Neither can see which VALUE a route stores, so neither could say
// whether the real client opens it, and for a chunked upload it did not: the row
// held a bare key, and the download, the analysis pass and the text readers all
// answered "not an azure blob URL" without asking storage. This test joins the two:
// the real routes, the real session and file rows, the real client, a real block
// blob.

// A name the route's sanitizer keeps and a URL parser would not, so a value read
// the wrong way names a different object.
const chunkedAwkwardName = "quarterly #1 100%25 report?.txt"

// chunkedExtractor answers as a real extractor would and records what it was given.
type chunkedExtractor struct {
	err  error
	data []byte
}

func (e *chunkedExtractor) Extract(_ context.Context, _ string, data []byte) (string, error) {
	e.data = data
	return string(data), e.err
}

type chunkedReadback struct {
	ctx       context.Context
	owner     string
	container string
	engine    *memql.MemQLEngine
	blobs     *azureblob.AzureBlobUploader
	store     *server.EngineLibraryStore
	handler   http.Handler
	lib       *library.Integration
}

func newChunkedReadback(t *testing.T) *chunkedReadback {
	t.Helper()
	conn := os.Getenv("MEMQL_AZURITE_TEST_CONNECTION_STRING")
	if conn == "" {
		t.Skip("MEMQL_AZURITE_TEST_CONNECTION_STRING is required for Library read-back evidence")
	}
	t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", conn)
	client, err := azblob.NewClientFromConnectionString(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	container := "library-readback-" + strings.ToLower(id.NewShortId())
	if _, err := client.CreateContainer(ctx, container, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := client.DeleteContainer(ctx, container, nil); err != nil {
			t.Error(err)
		}
	})
	// The container a node is configured with, read by the same constructor every
	// node uses: a row holding a bare key depends on this and nothing else.
	t.Setenv("MEMQL_AZURE_BLOB_CONTAINER", container)

	engine, db := workTemplateDBEngineAndDB(t)
	blobs, err := azureblob.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner := "readback-" + strings.ToLower(id.NewShortId())
	preparePipelineAzuriteAccounting(t, engine, db.DB, blobs, owner)
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gates.Close() })
	gate := func() *sql.DB { return gates }
	store := server.NewEngineLibraryStore(&AttachmentEngineAdapter{Engine: engine}, gate)
	lib := library.NewIntegration(engine, gate)
	lib.SetBlobFetcher(blobs)
	lib.SetArtifactPoll(1, time.Millisecond)
	if err := engine.RegisterIntegration(lib); err != nil {
		t.Fatal(err)
	}
	return &chunkedReadback{
		ctx:       auth.ContextWithAccess(auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: owner}), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter}),
		owner:     owner,
		container: container,
		engine:    engine,
		blobs:     blobs,
		store:     store,
		lib:       lib,
		handler: server.NewArtifactHandler(server.ArtifactHandlerOptions{
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Bucket: container,
			Uploader: blobs, Downloader: blobs, Blocks: blobs, Streamer: blobs,
			Store:         store,
			Sessions:      uploadsession.NewStore(&AttachmentEngineAdapter{Engine: engine}),
			PromotionWait: time.Millisecond, PromotionPoll: time.Millisecond,
		}),
	}
}

func TestLibraryFilesReadBackFromAzurite(t *testing.T) {
	f := newChunkedReadback(t)

	t.Run("an upload over the one-shot line", func(t *testing.T) {
		// Past the clients' 32 MiB line, at the production chunk size, so the bytes
		// really do span blocks.
		data := make([]byte, 32<<20+5)
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
		fileID, row := f.chunkedUpload(t, chunkedAwkwardName, "application/octet-stream", data, nil)
		f.holdsItsOwnersKey(t, row, fileID)

		artifactID := f.promote(t, fileID, chunkedAwkwardName)
		f.assertContent(t, artifactID, "", data)
		boundary := server.LibraryChunkSizeBytes
		f.assertRange(t, artifactID, boundary-4, data[boundary-4:boundary+4])

		f.analyze(t, fileID, artifactID, "application/octet-stream", row.BlobUrl)
		after := f.file(t, fileID)
		digest := sha256.Sum256(data)
		if after.Sha256 != hex.EncodeToString(digest[:]) {
			t.Fatalf("analysis stamped sha256 %q, want the upload's digest %s", after.Sha256, hex.EncodeToString(digest[:]))
		}
	})

	t.Run("a new version keeps every version readable", func(t *testing.T) {
		first, second := []byte("the first version\n"), []byte("the second version\n")
		fileID, _ := f.chunkedUpload(t, "versioned.txt", "text/plain", first, nil)
		artifactID := f.promote(t, fileID, "versioned.txt")

		versionedID, head := f.chunkedUpload(t, "versioned.txt", "text/plain", second, map[string]any{"targetArtifactId": artifactID})
		if versionedID != fileID {
			t.Fatalf("the version answered file %q, want the file's own %q", versionedID, fileID)
		}
		f.holdsItsOwnersKey(t, head, fileID)
		if head.VersionNumber != 2 {
			t.Fatalf("the head is at version %d, want 2", head.VersionNumber)
		}
		f.assertContent(t, artifactID, "", second)
		f.assertContent(t, artifactID, "1", first)
		f.assertContent(t, artifactID, "2", second)
	})

	t.Run("the text readers", func(t *testing.T) {
		text := []byte("ok\nFAIL one test\nok\n")
		fileID, _ := f.chunkedUpload(t, "step.log", "text/plain", text, nil)
		f.assertTextReaders(t, fileID, text)
	})

	t.Run("analysis reads a chunked file's text", func(t *testing.T) {
		text := []byte("the words of a long report\n")
		fileID, row := f.chunkedUpload(t, "report.txt", "text/plain", text, nil)
		artifactID := f.promote(t, fileID, "report.txt")

		extractor := &chunkedExtractor{}
		f.lib.SetExtractor(extractor)
		defer f.lib.SetExtractor(nil)
		f.analyze(t, fileID, artifactID, "text/plain", row.BlobUrl)

		if !bytes.Equal(extractor.data, text) {
			t.Fatalf("the extractor was given %q, want the upload's %q", extractor.data, text)
		}
		after := f.file(t, fileID)
		digest := sha256.Sum256(text)
		if after.Status != "ready" || after.Sha256 != hex.EncodeToString(digest[:]) {
			t.Fatalf("the file is %q with sha256 %q", after.Status, after.Sha256)
		}
	})

	// A row that stores the key alone: every folder-sync file, and a chunked upload
	// from before that route stored the URL. Nothing is migrated, so this is the
	// row as it stands.
	t.Run("a row that stores a bare key", func(t *testing.T) {
		data := []byte("stored by folder sync, or by an older engine\nFAIL one test\n")
		fileID := id.NewShortId()
		key := fmt.Sprintf("library/%s/%s/%s", f.owner, fileID, chunkedAwkwardName)
		const chunk = 10
		ids := []string{chunkedBlockID(1), chunkedBlockID(2)}
		if err := f.blobs.StageBlock(f.ctx, f.container, key, ids[0], data[:chunk]); err != nil {
			t.Fatal(err)
		}
		if err := f.blobs.StageBlock(f.ctx, f.container, key, ids[1], data[chunk:]); err != nil {
			t.Fatal(err)
		}
		if err := f.blobs.CommitBlockList(f.ctx, f.container, key, ids, "text/plain"); err != nil {
			t.Fatal(err)
		}
		f.createFile(t, fileID, chunkedAwkwardName, "text/plain", key, len(data))
		artifactID := f.promote(t, fileID, chunkedAwkwardName)

		f.assertContent(t, artifactID, "", data)
		f.assertRange(t, artifactID, chunk-2, data[chunk-2:chunk+2])
		f.assertTextReaders(t, fileID, data)

		// The pass measures it too, which is what a re-analysis of one would need.
		f.analyze(t, fileID, artifactID, "application/octet-stream", key)
		digest := sha256.Sum256(data)
		if got := f.file(t, fileID).Sha256; got != hex.EncodeToString(digest[:]) {
			t.Fatalf("analysis stamped sha256 %q", got)
		}
	})

	// The clients' rule, from the server's side (the Files Inspector and
	// editors/productivity's documents.ts): a `failed` file with no URL is an upload
	// whose bytes never landed, and they offer no download. Shipped editors read it
	// too, so a chunked file whose analysis fails must not look like one.
	t.Run("an analysis that fails leaves the upload readable", func(t *testing.T) {
		// A scanned PDF is the ordinary case: stored, and no text to extract.
		data := []byte("%PDF-1.7 scanned pages with no text in them")
		fileID, row := f.chunkedUpload(t, "scan.pdf", "application/pdf", data, nil)
		artifactID := f.promote(t, fileID, "scan.pdf")

		f.lib.SetExtractor(&chunkedExtractor{err: errors.New("no text in this file")})
		defer f.lib.SetExtractor(nil)
		err := f.lib.AnalyzeFile(f.ctx, library.AnalyzeFileParams{
			FileId: fileID, ArtifactId: artifactID, OwnerUserId: f.owner, Name: "scan.pdf", MimeType: "application/pdf", BlobUrl: row.BlobUrl,
		})
		if err == nil {
			t.Fatal("the pass read a file its extractor refused")
		}
		after := f.file(t, fileID)
		if after.Status != "failed" {
			t.Fatalf("the file is %q, want failed", after.Status)
		}
		if chunkedClientsCallItUnfinished(after) {
			t.Fatalf("a stored file reads as an upload that never finished: status %q, blobUrl %q", after.Status, after.BlobUrl)
		}
		f.assertContent(t, artifactID, "", data)
	})

	// The rule's other half, unchanged: a one-shot upload storage refused keeps its
	// intended key, and nothing is there. Storage says so now, and the answer the
	// route gives is the one it always gave.
	t.Run("an upload whose bytes never landed", func(t *testing.T) {
		fileID := id.NewShortId()
		key := fmt.Sprintf("library/%s/%s/lost.txt", f.owner, fileID)
		f.createFile(t, fileID, "lost.txt", "text/plain", key, 9)
		if err := f.store.SetFileStatus(f.ctx, server.LibraryFileStatusParams{FileId: fileID, Status: "failed", FailureReason: "object storage refused the upload, so the bytes were not stored"}); err != nil {
			t.Fatal(err)
		}
		if !chunkedClientsCallItUnfinished(f.file(t, fileID)) {
			t.Fatal("the clients no longer recognize an upload that never landed")
		}
		artifactID := f.promote(t, fileID, "lost.txt")
		if got := f.request(http.MethodGet, "/artifacts/"+artifactID+"/content", nil, ""); got.Code != http.StatusNotFound {
			t.Fatalf("GET content answered %d: %s", got.Code, got.Body.String())
		}
	})
}

// chunkedClientsCallItUnfinished is the rule both clients apply.
func chunkedClientsCallItUnfinished(row *server.LibraryFileRow) bool {
	return row.Status == "failed" && !strings.Contains(row.BlobUrl, "://")
}

// chunkedBlockID is a block id of the route's own fixed-width form.
func chunkedBlockID(n int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%08d", n)))
}

// holdsItsOwnersKey says a chunked row stores the URL the one-shot route stores,
// that the check every reader runs admits it, and that it names this owner's file.
func (f *chunkedReadback) holdsItsOwnersKey(t *testing.T, row *server.LibraryFileRow, fileID string) {
	t.Helper()
	if !strings.Contains(row.BlobUrl, "://") {
		t.Fatalf("the row stores %q; a chunked upload stores the URL", row.BlobUrl)
	}
	if err := librarykey.Check(row.BlobUrl, row.OwnerUserId, f.container); err != nil {
		t.Fatalf("the owner's own row %q was refused: %v", row.BlobUrl, err)
	}
	container, object, ok := librarykey.Split(row.BlobUrl)
	if !ok || container != f.container || !strings.HasPrefix(object, "library/"+f.owner+"/"+fileID+"/") {
		t.Fatalf("%q names %q %q %t", row.BlobUrl, container, object, ok)
	}
}

// chunkedUpload drives POST /artifacts/uploads, each PUT chunk and POST complete,
// the way the clients do, and returns the head row they left.
func (f *chunkedReadback) chunkedUpload(t *testing.T, name, mimeType string, data []byte, extra map[string]any) (string, *server.LibraryFileRow) {
	t.Helper()
	args := map[string]any{"name": name, "size": len(data), "mimeType": mimeType}
	for k, v := range extra {
		args[k] = v
	}
	body, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	opened := f.request(http.MethodPost, "/artifacts/uploads", body, "")
	if opened.Code != http.StatusCreated {
		t.Fatalf("open the session: %d %s", opened.Code, opened.Body.String())
	}
	var session struct {
		UploadId  string `json:"uploadId"`
		ChunkSize int64  `json:"chunkSize"`
	}
	if err := json.Unmarshal(opened.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if session.ChunkSize != server.LibraryChunkSizeBytes {
		t.Fatalf("chunk size %d, want the production %d", session.ChunkSize, server.LibraryChunkSizeBytes)
	}
	for n, offset := 1, int64(0); offset < int64(len(data)); n, offset = n+1, offset+session.ChunkSize {
		end := min(offset+session.ChunkSize, int64(len(data)))
		put := f.request(http.MethodPut, fmt.Sprintf("/artifacts/uploads/%s/chunks/%d", session.UploadId, n), data[offset:end], "")
		if put.Code != http.StatusNoContent {
			t.Fatalf("chunk %d: %d %s", n, put.Code, put.Body.String())
		}
	}
	done := f.request(http.MethodPost, "/artifacts/uploads/"+session.UploadId+"/complete", nil, "")
	if done.Code != http.StatusCreated {
		t.Fatalf("complete: %d %s", done.Code, done.Body.String())
	}
	var receipt server.ArtifactUploadResponse
	if err := json.Unmarshal(done.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt.FileId, f.file(t, receipt.FileId)
}

func (f *chunkedReadback) createFile(t *testing.T, fileID, name, mimeType, blobURL string, size int) {
	t.Helper()
	if err := f.store.CreateFile(f.ctx, server.LibraryFileCreateParams{
		FileId: fileID, Name: name, MimeType: mimeType, Size: size, BlobUrl: blobURL, Source: "uploaded", Format: server.LibraryFormatForMIME(mimeType),
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *chunkedReadback) file(t *testing.T, fileID string) *server.LibraryFileRow {
	t.Helper()
	row, err := f.store.File(memql.ContextWithFreshRead(f.ctx), server.LibraryFileConceptRef(fileID))
	if err != nil || row == nil {
		t.Fatalf("read file %s: %+v %v", fileID, row, err)
	}
	return row
}

// promote writes the index row indexFileOnCreate writes for a new file. The
// automation does not run in this fixture, and the content route is reached through
// that row.
func (f *chunkedReadback) promote(t *testing.T, fileID, name string) string {
	t.Helper()
	templateMutation(t, f.engine, f.ctx, "createArtifact", map[string]any{
		"sourceConceptRef": server.LibraryFileConceptRef(fileID),
		"ownerUserId":      f.owner,
		"lens":             "artifact",
		"kind":             "file",
		"source":           "uploaded",
		"title":            name,
		"format":           "text",
		"mimeType":         "application/octet-stream",
	})
	artifactID, err := f.store.ArtifactForFile(memql.ContextWithFreshRead(f.ctx), fileID)
	if err != nil || artifactID == "" {
		t.Fatalf("the promoted artifact for %s is not readable: %q %v", fileID, artifactID, err)
	}
	return artifactID
}

func (f *chunkedReadback) analyze(t *testing.T, fileID, artifactID, mimeType, blobURL string) {
	t.Helper()
	if err := f.lib.AnalyzeFile(f.ctx, library.AnalyzeFileParams{
		FileId: fileID, ArtifactId: artifactID, OwnerUserId: f.owner, Name: "file", MimeType: mimeType, BlobUrl: blobURL,
	}); err != nil {
		t.Fatalf("analysis: %v", err)
	}
}

func (f *chunkedReadback) assertContent(t *testing.T, artifactID, version string, want []byte) {
	t.Helper()
	path := "/artifacts/" + artifactID + "/content"
	if version != "" {
		path += "?version=" + version
	}
	got := f.request(http.MethodGet, path, nil, "")
	if got.Code != http.StatusOK {
		t.Fatalf("GET %s answered %d: %s", path, got.Code, got.Body.String())
	}
	if !bytes.Equal(got.Body.Bytes(), want) {
		t.Fatalf("GET %s returned %d bytes that are not the %d stored", path, got.Body.Len(), len(want))
	}
}

func (f *chunkedReadback) assertRange(t *testing.T, artifactID string, start int64, want []byte) {
	t.Helper()
	header := fmt.Sprintf("bytes=%d-%d", start, start+int64(len(want))-1)
	got := f.request(http.MethodGet, "/artifacts/"+artifactID+"/content", nil, header)
	if got.Code != http.StatusPartialContent {
		t.Fatalf("%s answered %d: %s", header, got.Code, got.Body.String())
	}
	if !bytes.Equal(got.Body.Bytes(), want) {
		t.Fatalf("%s returned %q, want %q", header, got.Body.Bytes(), want)
	}
}

// assertTextReaders reads a saved text file through the two builtins an agent and
// the failed-run diagnosis use, under the file's version gate, by the real client.
func (f *chunkedReadback) assertTextReaders(t *testing.T, fileID string, want []byte) {
	t.Helper()
	read := f.builtin(t, "libraryReadFile", map[string]any{"fileId": fileID})
	if read["content"] != string(want) {
		t.Fatalf("libraryReadFile answered %v", read)
	}
	excerpts := f.builtin(t, "libraryReadFileExcerpts", map[string]any{
		"files":    []any{map[string]any{"fileId": fileID, "label": "the log"}},
		"patterns": []any{"FAIL"},
		"maxBytes": 4096,
	})
	files, _ := excerpts["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("libraryReadFileExcerpts answered %v", excerpts)
	}
	entry, _ := files[0].(map[string]any)
	if entry["excerpt"] != string(want) || entry["complete"] != true || entry["label"] != "the log" {
		t.Fatalf("libraryReadFileExcerpts answered %v", entry)
	}
}

func (f *chunkedReadback) builtin(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.engine.Execute(memql.ContextWithFreshRead(f.ctx), "builtin "+call)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	rows := memql.MaterializeRows(result)
	if len(rows) != 1 {
		t.Fatalf("%s answered %d rows", name, len(rows))
	}
	return rows[0]
}

func (f *chunkedReadback) request(method, path string, body []byte, rangeHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body)).WithContext(f.ctx)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}
