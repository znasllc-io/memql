package library

import (
	"context"
	"strings"
	"testing"
)

// file_bytes_test.go -- every document read that streams a Library file holds
// the row's blobUrl to its owner's own library/{owner}/ prefix before storage
// is asked. The reader owns the row in every case below, so row admission
// passes; the key is the only thing left to refuse on.

const (
	foreignKey = "https://acct.blob.core.windows.net/memql-library/library/user-a/f-a/payroll.md"
	ownKey     = "https://acct.blob.core.windows.net/memql-library/library/user-b/f-b/notes.md"
)

func fileDocument(blobURL string) reviewDocument {
	return reviewDocument{
		kind: "file", version: 1, revision: "file:1",
		// Stored canonical, while the path carries the bare id.
		owner:   "v1:identity:user:user-b",
		backing: map[string]any{"blobUrl": blobURL, "ownerUserId": "v1:identity:user:user-b", "name": "notes.md"},
	}
}

func TestDocumentFileReadsRefuseAKeyOutsideTheOwnersLibrary(t *testing.T) {
	anchor := map[string]any{"startLine": 0, "endLine": 1, "sourceQuote": "# Notes"}
	reads := map[string]func(*Integration, reviewDocument) error{
		"a saved version's preview": func(i *Integration, doc reviewDocument) error {
			_, _, err := i.historicalContent(context.Background(), doc, 1)
			return err
		},
		"a passage check": func(i *Integration, doc reviewDocument) error {
			return i.verifyReviewPassage(context.Background(), doc, anchor)
		},
		"a revision's source": func(i *Integration, doc reviewDocument) error {
			_, err := i.revisionContent(context.Background(), doc)
			return err
		},
	}
	for name, read := range reads {
		fetcher := &reviewFetcher{stream: &reviewStream{Reader: strings.NewReader("# Notes\n")}}
		if err := read(&Integration{blobFetcher: fetcher}, fileDocument(foreignKey)); err == nil {
			t.Errorf("%s: a row naming another owner's key was read", name)
		}
		if fetcher.url != "" {
			t.Errorf("%s: storage was asked for %q before the refusal", name, fetcher.url)
		}

		// The owner's own key still reads, through the same function.
		fetcher = &reviewFetcher{stream: &reviewStream{Reader: strings.NewReader("# Notes\n")}}
		if err := read(&Integration{blobFetcher: fetcher}, fileDocument(ownKey)); err != nil {
			t.Errorf("%s: the owner's own file was refused: %v", name, err)
		}
		if fetcher.url != ownKey {
			t.Errorf("%s: read %q, want the row's own key", name, fetcher.url)
		}
	}
}
