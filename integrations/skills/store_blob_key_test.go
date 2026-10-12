package skills

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// store_blob_key_test.go -- a script's bytes are shipped to a machine and RUN,
// so the file row's blobUrl is held to its owner's own library/{owner}/ prefix
// before storage is asked. The caller owns the row in every case below; the
// key is the only thing left to refuse on.

type fileRowEngine struct{ row map[string]any }

func (e *fileRowEngine) Execute(context.Context, string) (any, error) { return nil, nil }

func (e *fileRowEngine) ExecuteRows(_ context.Context, query string) ([]map[string]any, error) {
	if strings.HasPrefix(query, "query libraryFileById(") {
		return []map[string]any{e.row}, nil
	}
	return nil, nil
}

type askedFetcher struct{ asked []string }

func (f *askedFetcher) DownloadStreamURL(_ context.Context, blobURL string) (io.ReadCloser, error) {
	f.asked = append(f.asked, blobURL)
	return io.NopCloser(bytes.NewReader([]byte("#!/bin/sh\necho hi\n"))), nil
}

func TestReadArtifactRefusesAKeyOutsideTheOwnersLibrary(t *testing.T) {
	const fileRef = "v1:library:file:f-b"
	row := func(blobURL string) map[string]any {
		return map[string]any{"id": fileRef, "name": "deploy.sh", "blobUrl": blobURL, "ownerUserId": "v1:identity:user:user-b"}
	}

	fetcher := &askedFetcher{}
	store := NewEngineStore(&fileRowEngine{row: row("https://acct.blob.core.windows.net/memql-library/library/user-a/f-a/deploy.sh")}, fetcher)
	if _, err := store.ReadArtifact(context.Background(), fileRef); err == nil {
		t.Fatal("a script row naming another owner's key was read")
	}
	if len(fetcher.asked) != 0 {
		t.Fatalf("storage was asked for %q before the refusal", fetcher.asked)
	}

	own := "https://acct.blob.core.windows.net/memql-library/library/user-b/f-b/deploy.sh"
	store = NewEngineStore(&fileRowEngine{row: row(own)}, fetcher)
	got, err := store.ReadArtifact(context.Background(), fileRef)
	if err != nil || len(got.Data) == 0 {
		t.Fatalf("the owner's own script was refused: %v", err)
	}
	if len(fetcher.asked) != 1 || fetcher.asked[0] != own {
		t.Fatalf("read %q, want the row's own key", fetcher.asked)
	}
}
