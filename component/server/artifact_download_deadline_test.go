package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type slowDownloadStream struct{}

func (slowDownloadStream) DownloadStreamURL(context.Context, string) (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	go func() {
		defer writer.Close()
		_, _ = io.WriteString(writer, strings.Repeat("a", 8192))
		time.Sleep(75 * time.Millisecond)
		_, _ = io.WriteString(writer, "end")
	}()
	return reader, nil
}

func (s slowDownloadStream) DownloadRangeURL(ctx context.Context, url string, _, _ int64) (io.ReadCloser, error) {
	return s.DownloadStreamURL(ctx, url)
}

func TestArtifactBodySurvivesServerWriteDeadline(t *testing.T) {
	h := &ArtifactHandler{streamer: slowDownloadStream{}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.streamFileBytes(w, r, &LibraryFileRow{ID: "owned", Size: 8195, BlobUrl: "blob://fixture"}, "release.tar")
	}))
	s.Config.WriteTimeout = 20 * time.Millisecond
	s.Start()
	defer s.Close()
	response, err := s.Client().Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != strings.Repeat("a", 8192)+"end" {
		t.Fatalf("download ended before declared length: bytes=%d error=%v", len(body), err)
	}
}

func TestRecoveryPreservesPartialResponseAbort(t *testing.T) {
	w := httptest.NewRecorder()
	h := PanicRecoveryMiddleware(nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "partial")
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("transport abort was swallowed: %v", got)
		}
		if w.Body.String() != "partial" {
			t.Fatal("recovery appended error JSON to a partial file")
		}
	}()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
}
