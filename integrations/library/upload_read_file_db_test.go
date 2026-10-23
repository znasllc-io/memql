package library

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
)

type uploadReadEngine struct{ engine *memql.MemQLEngine }

func (e uploadReadEngine) Execute(ctx context.Context, query string) (any, error) {
	return e.engine.Execute(ctx, query)
}

type uploadReadBlob struct {
	data  map[string][]byte
	reads int
}

func (b *uploadReadBlob) Upload(_ context.Context, bucket, name string, data []byte, _ string) (string, error) {
	url := "https://store.example/" + bucket + "/" + name
	b.data[url] = bytes.Clone(data)
	return url, nil
}
func (b *uploadReadBlob) DownloadStreamURL(_ context.Context, url string) (io.ReadCloser, error) {
	b.reads++
	data, ok := b.data[url]
	if !ok {
		return nil, fmt.Errorf("missing blob")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// Connect real multipart ingestion, owner-stamped persisted rows and the saved
// file reader on another engine. Storage bytes alone are stubbed. In particular,
// no artifact index, text extraction or trusted filename is needed for this read.
func testUploadedJSONReadAcrossEngines(t *testing.T, f *revisionDB) {
	blob := &uploadReadBlob{data: map[string][]byte{}}
	store := server.NewEngineLibraryStore(uploadReadEngine{f.engine})
	h := server.NewArtifactHandler(server.ArtifactHandlerOptions{Store: store, Uploader: blob, Bucket: "library", PromotionWait: time.Millisecond, PromotionPoll: time.Millisecond})
	f.second.SetBlobFetcher(blob)
	data := []byte("{\"verified\":true,\"label\":\"café\"}\n")
	digest := sha256.Sum256(data)
	for _, mediaType := range []string{"application/octet-stream", "application/json"} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="file"; filename="checkpoint.json"`)
		header.Set("Content-Type", mediaType)
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/artifacts", &body).WithContext(f.ctx)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		response := httptest.NewRecorder()
		h.ServeHTTP(response, req)
		if response.Code != http.StatusCreated {
			t.Fatalf("upload: %d %s", response.Code, response.Body.String())
		}
		var receipt server.ArtifactUploadResponse
		if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		row, err := store.File(memql.ContextWithFreshRead(f.ctx), server.LibraryFileConceptRef(receipt.FileId))
		if err != nil {
			t.Fatal(err)
		}
		if memql.BareShortId(row.OwnerUserId) != f.owner || row.MimeType != mediaType || row.Sha256 != hex.EncodeToString(digest[:]) || row.Size != len(data) {
			t.Fatalf("changed upload identity: %+v", row)
		}
		args := map[string]any{"fileId": receipt.FileId}
		before := blob.reads
		for _, denied := range []context.Context{context.Background(), revisionActor(f.owner+"-outsider", auth.RoleReader)} {
			if _, err := f.second.handleReadFile(denied, args, 0); err == nil || blob.reads != before {
				t.Fatal("read before authorization")
			}
		}
		rows, err := f.second.handleReadFile(revisionActor(f.owner, auth.RoleReader), args, 0)
		if mediaType == "application/octet-stream" {
			if err == nil || blob.reads != before {
				t.Fatal("binary MIME gate weakened")
			}
			continue
		}
		result := historyResult(t, rows, err)
		if result["content"] != string(data) || result["mimeType"] != mediaType || result["revision"] != "file:1" {
			t.Fatalf("wrong saved content: %v", result)
		}
		if !bytes.Equal(blob.data[row.BlobUrl], data) {
			t.Fatal("stored bytes changed")
		}
	}
}
