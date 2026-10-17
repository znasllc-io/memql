package ociregistry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPublishGitHubSingularUploadSession(t *testing.T) {
	image, want := verifiedFixture(t)
	handler := quietRegistry()
	var uploads atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Model GHCR's session Location while retaining the fixture registry's
		// actual blob persistence and digest verification.
		if strings.Contains(r.URL.Path, "/blobs/upload/") {
			uploads.Add(1)
			r.URL.Path = strings.Replace(r.URL.Path, "/blobs/upload/", "/blobs/uploads/", 1)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		for key, values := range response.Header() {
			w.Header()[key] = values
		}
		if location := w.Header().Get("Location"); location != "" {
			w.Header().Set("Location", strings.Replace(location, "/blobs/uploads/", "/blobs/upload/", 1))
		}
		w.WriteHeader(response.Code)
		_, _ = io.Copy(w, response.Body)
	}))
	defer s.Close()
	receipt, err := publisher(t, s.URL).Publish(context.Background(), image)
	if err != nil {
		t.Fatal(err)
	}
	if uploads.Load() == 0 || receipt.ImageDigest != want.ImageDigest {
		t.Fatalf("upload/readback not proven: uploads=%d receipt=%+v", uploads.Load(), receipt)
	}
}

func TestSingularUploadSessionKeepsRepositoryAndDigestScope(t *testing.T) {
	p := publisher(t, "http://127.0.0.1:1234")
	digest := sha([]byte("blob"))
	s := &scopedTransport{publisher: p, image: sha([]byte("image")), blobs: map[string]int64{digest: 4}}
	for _, path := range []string{
		"/v2/other/image/blobs/upload/id?digest=" + digest,
		"/v2/receipts/image/blobs/upload/id?digest=" + sha(nil),
		"/v2/receipts/image/blobs/upload/../id?digest=" + digest,
		"/v2/receipts/image/blobs/upload/id/extra?digest=" + digest,
		"/v2/receipts/image/blobs/upload/id?digest=" + digest + "&redirect=elsewhere",
	} {
		r, _ := http.NewRequest(http.MethodPut, p.target.Origin+path, nil)
		if _, err := s.permitted(r); err == nil {
			t.Fatalf("accepted unscoped upload: %s", path)
		}
	}
	r, _ := http.NewRequest(http.MethodPost, p.target.Origin+"/v2/receipts/image/blobs/upload/", nil)
	if _, err := s.permitted(r); err == nil {
		t.Fatal("singular path created an upload")
	}
}
