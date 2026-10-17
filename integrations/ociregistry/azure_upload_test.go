package ociregistry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// ACR's Start Upload response preserves _nouploadcache on subsequent requests:
// https://learn.microsoft.com/en-us/rest/api/registry-dataplane/container-registry-blob/start-upload
// Exercise the whole uploader and immutable readback, not just URL admission.
func TestPublishAzureUploadCacheFlag(t *testing.T) {
	for _, flag := range []string{"false", "true"} {
		t.Run(flag, func(t *testing.T) {
			image, want := verifiedFixture(t)
			handler := quietRegistry()
			var uploads atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/blobs/uploads/") && r.Method != http.MethodPost {
					if r.URL.Query().Get("_nouploadcache") != flag {
						t.Error("upload lost the registry's cache flag")
					}
					uploads.Add(1)
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, r)
				for key, values := range response.Header() {
					w.Header()[key] = values
				}
				if raw := w.Header().Get("Location"); strings.Contains(raw, "/blobs/uploads/") {
					location, err := url.Parse(raw)
					if err != nil {
						t.Error(err)
						return
					}
					query := location.Query()
					query.Set("_nouploadcache", flag)
					location.RawQuery = query.Encode()
					w.Header().Set("Location", location.String())
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
		})
	}
}

func TestUploadCacheFlagCannotBroadenRequest(t *testing.T) {
	p := publisher(t, "http://127.0.0.1:1234")
	digest := sha([]byte("blob"))
	s := &scopedTransport{publisher: p, image: sha([]byte("image")), blobs: map[string]int64{digest: 4}}
	for _, suffix := range []string{
		"_nouploadcache=", "_nouploadcache=1", "_nouploadcache=FALSE",
		"_nouploadcache=false&_nouploadcache=true", "_nouploadcache=false&other=x",
		"_nouploadcache=false&digest=" + sha(nil),
	} {
		r, _ := http.NewRequest(http.MethodPatch, p.target.Origin+"/v2/receipts/image/blobs/uploads/id?"+suffix, nil)
		if _, err := s.permitted(r); err == nil {
			t.Fatalf("accepted %q", suffix)
		}
	}
	for _, path := range []string{"/v2/other/image/blobs/uploads/id", "/v2/receipts/image/manifests/" + s.image, "/v2/receipts/image/blobs/uploads/"} {
		r, _ := http.NewRequest(http.MethodPost, p.target.Origin+path+"?_nouploadcache=false", nil)
		if _, err := s.permitted(r); err == nil {
			t.Fatalf("accepted cache flag outside a scoped upload session: %s", path)
		}
	}
}
