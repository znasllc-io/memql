package library

// analysis_sha256_test.go -- the pass stamps a chunked upload's hash
// (memql#4782, design D10).
//
// A chunked upload's handler never holds the whole file, so the row lands
// in `stored` with sha256 ABSENT and Data nil. The analysis pass is the
// one place that reads the committed blob afterwards, so it is the one
// place the hash can be measured honestly -- streamed ONCE, in constant
// memory, with extraction (when the type is readable) fed from the same
// pass rather than a second download.
//
// The claims:
//
//  1. An opaque chunked file (video) ends `ready` with sha256 stamped from
//     the streamed bytes -- and the blob was streamed exactly once.
//  2. An extractable chunked file gets chunks AND the hash from one
//     stream.
//  3. A file whose hash is already known (every one-shot upload) never
//     pays a blob read for hashing.
//  4. No fetcher wired + no hash known: the pass proceeds and the row
//     stays hash-absent -- absent means "not measured", and inventing or
//     failing would both be worse.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"testing"
)

type fakeBlobStreamer struct {
	data    []byte
	streams int
	err     error
}

func (f *fakeBlobStreamer) DownloadStreamURL(_ context.Context, _ string) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.streams++
	return io.NopCloser(strings.NewReader(string(f.data))), nil
}

func hexSum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestAnalysisStampsSha256ForAChunkedOpaqueFile(t *testing.T) {
	stub := newLibStub()
	stub.seedFile("f-1", "user-a", "clip.mp4", "video/mp4", "other")
	body := []byte("not really a video, but the bytes are the bytes")
	fetcher := &fakeBlobStreamer{data: body}

	i := NewIntegration(stub)
	i.SetBlobFetcher(fetcher)
	if err := i.AnalyzeFile(context.Background(), AnalyzeFileParams{
		FileId: "f-1", OwnerUserId: "user-a", Name: "clip.mp4", MimeType: "video/mp4",
		BlobUrl: "https://x.blob/b/library/user-a/f-1/clip.mp4",
	}); err != nil {
		t.Fatalf("AnalyzeFile: %v", err)
	}

	row := stub.files["f-1"]
	if got := asString(row["status"]); got != "ready" {
		t.Fatalf("status = %q, want ready -- an opaque type is a terminal success", got)
	}
	if got := asString(row["sha256"]); got != hexSum(body) {
		t.Fatalf("sha256 = %q, want the streamed digest %q -- the pass is what stamps a chunked "+
			"upload's hash (design D10)", got, hexSum(body))
	}
	if fetcher.streams != 1 {
		t.Fatalf("blob streamed %d times, want exactly once", fetcher.streams)
	}
}

func TestAnalysisHashesAndExtractsFromOneStream(t *testing.T) {
	stub := newLibStub()
	stub.seedFile("f-1", "user-a", "notes.md", "text/markdown", "markdown")
	stub.seedPromotedArtifact("f-1", "user-a", nil)
	body := []byte("# Heading\n\nEnough text to extract and chunk.")
	fetcher := &fakeBlobStreamer{data: body}

	i := NewIntegration(stub)
	i.SetBlobFetcher(fetcher)
	i.SetExtractor(passthroughExtractor{})
	i.SetArtifactPoll(1, 0)
	if err := i.AnalyzeFile(context.Background(), AnalyzeFileParams{
		FileId: "f-1", OwnerUserId: "user-a", Name: "notes.md", MimeType: "text/markdown",
		BlobUrl: "https://x.blob/b/library/user-a/f-1/notes.md",
	}); err != nil {
		t.Fatalf("AnalyzeFile: %v", err)
	}

	row := stub.files["f-1"]
	if got := asString(row["status"]); got != "ready" {
		t.Fatalf("status = %q, want ready; row: %v", got, row)
	}
	if got := asString(row["sha256"]); got != hexSum(body) {
		t.Fatalf("sha256 = %q, want %q", got, hexSum(body))
	}
	if len(stub.chunks) == 0 {
		t.Fatalf("no chunks written -- the fetched bytes must feed extraction, not only the hash")
	}
	if fetcher.streams != 1 {
		t.Fatalf("blob streamed %d times, want exactly once -- hash and extraction share the stream", fetcher.streams)
	}
}

func TestAnalysisNeverRefetchesAKnownHash(t *testing.T) {
	stub := newLibStub()
	stub.seedFile("f-1", "user-a", "clip.mp4", "video/mp4", "other")
	stub.files["f-1"]["sha256"] = "already-there"
	fetcher := &fakeBlobStreamer{data: []byte("bytes")}

	i := NewIntegration(stub)
	i.SetBlobFetcher(fetcher)
	if err := i.AnalyzeFile(context.Background(), AnalyzeFileParams{
		FileId: "f-1", OwnerUserId: "user-a", Name: "clip.mp4", MimeType: "video/mp4",
		BlobUrl: "https://x.blob/b/library/user-a/f-1/clip.mp4",
		Sha256:  "already-there",
	}); err != nil {
		t.Fatalf("AnalyzeFile: %v", err)
	}
	if fetcher.streams != 0 {
		t.Fatalf("blob streamed %d times for a file whose hash is known and whose type is opaque, want 0", fetcher.streams)
	}
	if got := asString(stub.files["f-1"]["sha256"]); got != "already-there" {
		t.Fatalf("a known hash was overwritten: %q", got)
	}
}

func TestAnalysisProceedsHashlessWithNoFetcher(t *testing.T) {
	stub := newLibStub()
	stub.seedFile("f-1", "user-a", "clip.mp4", "video/mp4", "other")

	i := NewIntegration(stub)
	if err := i.AnalyzeFile(context.Background(), AnalyzeFileParams{
		FileId: "f-1", OwnerUserId: "user-a", Name: "clip.mp4", MimeType: "video/mp4",
		BlobUrl: "https://x.blob/b/library/user-a/f-1/clip.mp4",
	}); err != nil {
		t.Fatalf("AnalyzeFile: %v", err)
	}
	row := stub.files["f-1"]
	if got := asString(row["status"]); got != "ready" {
		t.Fatalf("status = %q, want ready -- a missing fetcher is an operator condition, not a file fault", got)
	}
	if _, present := row["sha256"]; present {
		t.Fatalf("sha256 was written with nothing to measure it from: %v -- absent means 'not measured', "+
			"and writing anything else would be an invented fact", row["sha256"])
	}
}

// passthroughExtractor hands the bytes back as text, so the chunk pipeline
// runs against exactly what the fetch produced.
type passthroughExtractor struct{}

func (passthroughExtractor) Extract(_ context.Context, _ string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("no bytes to extract")
	}
	return string(data), nil
}
