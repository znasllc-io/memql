package library

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type reviewStream struct {
	io.Reader
	closed bool
}

func (s *reviewStream) Close() error { s.closed = true; return nil }

type reviewFetcher struct {
	stream *reviewStream
	err    error
	url    string
}

func (f *reviewFetcher) DownloadStreamURL(_ context.Context, url string) (io.ReadCloser, error) {
	f.url = url
	return f.stream, f.err
}

type reviewReadError struct{}

func (reviewReadError) Read([]byte) (int, error) { return 0, errors.New("interrupted") }

func TestReviewPassageChecksSavedBytesAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reader    io.Reader
		err       error
		wantError bool
	}{
		{"crlf", strings.NewReader("# Heading\r\n\r\nA paragraph.\r\n"), nil, false},
		{"wrong passage", strings.NewReader("# Heading\n\nAnother paragraph."), nil, true},
		{"range beyond file", strings.NewReader("short"), nil, true},
		{"invalid utf8", strings.NewReader("# Heading\n\nA paragraph.\xff"), nil, true},
		{"oversize", strings.NewReader(strings.Repeat("x", 2*1024*1024+1)), nil, true},
		{"open failure", nil, errors.New("unavailable"), true},
		{"stream failure", reviewReadError{}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &reviewStream{Reader: tc.reader}
			fetcher := &reviewFetcher{stream: stream, err: tc.err}
			integration := &Integration{blobFetcher: fetcher}
			doc := reviewDocument{kind: "file", backing: map[string]any{"blobUrl": "https://configured-store.example/review.md"}}
			anchor := map[string]any{"startLine": 2, "endLine": 3, "sourceQuote": "A paragraph."}
			err := integration.verifyReviewPassage(context.Background(), doc, anchor)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v", err)
			}
			if tc.err == nil && !stream.closed {
				t.Fatal("document stream left open")
			}
			if fetcher.url != doc.backing["blobUrl"] {
				t.Fatal("did not read the authorized backing URL")
			}
		})
	}
	if err := (&Integration{}).verifyReviewPassage(context.Background(), reviewDocument{kind: "file"}, nil); err == nil {
		t.Fatal("missing storage accepted")
	}
}

func TestReviewAnchorRefusesFractionalLineClaims(t *testing.T) {
	for _, key := range []string{"startLine", "endLine"} {
		anchor := map[string]any{"kind": "markdown", "startLine": 0, "endLine": 1, "quote": "text", "sourceQuote": "text"}
		anchor[key] = 0.5
		if _, err := validateReviewAnchor(anchor); err == nil {
			t.Fatalf("fractional %s accepted", key)
		}
	}
}
