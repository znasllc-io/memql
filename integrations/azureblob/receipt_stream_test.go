package azureblob

import (
	"context"
	"crypto/sha256"
	"io"
	"strings"
	"testing"
)

func TestReceiptStreamChecksVersionAndBytesBeforeEOF(t *testing.T) {
	for _, tc := range []struct {
		name           string
		body           string
		size           int64
		etag, digest   string
		extra, badETag bool
		bad            bool
	}{
		{"match", "data", 4, `"stored-v1"`, verifiedSHA([]byte("data")), false, false, false},
		{"empty", "", 0, `"stored-v1"`, verifiedSHA(nil), false, false, false},
		{"replaced", "data", 4, `"old"`, verifiedSHA([]byte("data")), false, false, true},
		{"size", "data", 3, `"stored-v1"`, verifiedSHA([]byte("data")), false, false, true},
		{"hash", "data", 4, `"stored-v1"`, strings.Repeat("0", 64), false, false, true},
		{"extra body", "data", 4, `"stored-v1"`, verifiedSHA([]byte("data")), true, false, true},
		{"response etag", "data", 4, `"stored-v1"`, verifiedSHA([]byte("data")), false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &verifiedFixture{body: []byte(tc.body), exists: true, extraBody: tc.extra, badETag: tc.badETag}
			u := verifiedClient(t, f)
			r, err := u.OpenReceiptStream(context.Background(), "memql", "artifact", VerifiedBlob{ETag: tc.etag, Size: tc.size, SHA256: tc.digest, URL: "https://must-not-follow.test"})
			var b []byte
			if err == nil {
				b, err = io.ReadAll(r)
				r.Close()
			}
			if tc.bad {
				if err == nil {
					t.Fatal("invalid receipt reached successful EOF")
				}
			} else if err != nil || string(b) != tc.body {
				t.Fatal("valid stream", err)
			}
		})
	}
}

func TestReceiptStreamEarlyEOFIsFailure(t *testing.T) {
	s := &receiptStream{body: io.NopCloser(strings.NewReader("x")), remaining: 2, digest: verifiedSHA([]byte("xx")), hash: sha256.New()}
	if _, err := io.ReadAll(s); err == nil {
		t.Fatal("truncated stream reached successful EOF")
	}
}

func TestAzuriteReceiptStreamExactVersion(t *testing.T) {
	u := azuriteUploader(t)
	object := verifiedAzuriteObject(t, u)
	const value = "a version-bound release artifact"
	receipt, err := u.CreateVerifiedStream(t.Context(), "blockstest", object, strings.NewReader(value), int64(len(value)), verifiedSHA([]byte(value)), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	r, err := u.OpenReceiptStream(t.Context(), "blockstest", object, receipt)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(body) != value {
		t.Fatal("real receipt read", err)
	}
	receipt.ETag = `"wrong-version"`
	if r, err := u.OpenReceiptStream(t.Context(), "blockstest", object, receipt); err == nil {
		r.Close()
		t.Fatal("replaced version accepted")
	}
}
