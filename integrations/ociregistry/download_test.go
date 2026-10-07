package ociregistry

import (
	"context"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPublishConfiguredBlobDownloadsCarryNoCredentials(t *testing.T) {
	v, _ := verifiedFixture(t)
	registry := quietRegistry()
	var downloads atomic.Int64
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
			t.Errorf("download carried registry authority: %s %v", r.Method, r.Header)
		}
		if r.URL.Query().Get("sig") != "local-test-only" {
			t.Error("signed query not preserved")
		}
		d := strings.TrimPrefix(r.URL.Path, "/objects/")
		body, err := os.ReadFile(filepath.Join(v.dir, "layout", blobPath(d)))
		if err != nil {
			t.Error(err)
			w.WriteHeader(404)
			return
		}
		downloads.Add(1)
		_, _ = w.Write(body)
	}))
	defer storage.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" && r.Header.Get("Authorization") != "Bearer registry-only-token" {
			t.Error("registry credential missing")
		}
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/blobs/sha256:") {
			d := strings.TrimPrefix(r.URL.Path, "/v2/receipts/image/blobs/")
			w.Header().Set("Set-Cookie", "must-not-forward=secret")
			http.Redirect(w, r, storage.URL+"/objects/"+d+"?sig=local-test-only", 307)
			return
		}
		registry.ServeHTTP(w, r)
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(storage.Certificate())
	p, err := NewPublisher(Target{Origin: server.URL, Repository: "receipts/image", AllowLoopbackHTTP: true, BearerToken: "registry-only-token", RootCAs: roots, BlobDownloadOrigins: []string{storage.URL}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publish(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publish(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if downloads.Load() < 4 {
		t.Fatalf("only %d blob reads", downloads.Load())
	}
}

func TestDownloadRedirectConfinement(t *testing.T) {
	for _, attack := range []string{"origin", "port", "scheme", "userinfo", "fragment", "encoded-path", "parent-path", "manifest", "token", "loop", "challenge", "oversized", "corrupt"} {
		t.Run(attack, func(t *testing.T) {
			v, _ := verifiedFixture(t)
			registry := quietRegistry()
			var forbidden atomic.Int64
			var downloads atomic.Int64
			sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forbidden.Add(1); w.WriteHeader(200) }))
			defer sink.Close()
			var storageOrigin string
			storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				downloads.Add(1)
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("credential forwarded")
				}
				switch attack {
				case "loop":
					http.Redirect(w, r, storageOrigin+"/loop?sig=do-not-log", 307)
				case "challenge":
					w.Header().Set("WWW-Authenticate", `Bearer realm="`+sink.URL+`/token"`)
					w.WriteHeader(401)
				case "oversized":
					_, _ = io.WriteString(w, strings.Repeat("x", 100000))
				case "corrupt":
					_, _ = io.WriteString(w, "corrupt")
				default:
					t.Error("invalid redirect reached configured storage endpoint")
					w.WriteHeader(500)
				}
			}))
			defer storage.Close()
			storageOrigin = storage.URL
			destination := storage.URL + "/blob?sig=do-not-log"
			switch attack {
			case "origin":
				destination = "https://unconfigured.invalid/blob"
			case "port":
				destination = sink.URL + "/blob"
			case "scheme":
				destination = strings.Replace(storage.URL, "https:", "http:", 1) + "/blob"
			case "userinfo":
				destination = strings.Replace(sink.URL, "https://", "https://allowed.example@", 1) + "/blob"
			case "fragment":
				destination += "#fragment"
			case "encoded-path":
				destination = storage.URL + "/a%2fb"
			case "parent-path":
				destination = storage.URL + "/a/../blob"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if attack == "token" {
					w.Header().Set("WWW-Authenticate", `Bearer realm="`+storage.URL+`/token",service="test"`)
					w.WriteHeader(401)
					return
				}
				if (attack == "manifest" && strings.Contains(r.URL.Path, "/manifests/")) || (r.Method == "GET" && strings.Contains(r.URL.Path, "/blobs/sha256:")) {
					http.Redirect(w, r, destination, 307)
					return
				}
				registry.ServeHTTP(w, r)
			}))
			defer server.Close()
			roots := x509.NewCertPool()
			roots.AddCert(storage.Certificate())
			roots.AddCert(sink.Certificate())
			p, err := NewPublisher(Target{Origin: server.URL, Repository: "receipts/image", AllowLoopbackHTTP: true, BearerToken: "private-registry-token", RootCAs: roots, BlobDownloadOrigins: []string{storage.URL}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Publish(context.Background(), v)
			if err == nil {
				t.Fatal("unsafe or corrupt download accepted")
			}
			if strings.Contains(err.Error(), "do-not-log") {
				t.Fatal("signed query exposed in error")
			}
			if forbidden.Load() != 0 {
				t.Fatalf("unconfigured endpoint received %d requests", forbidden.Load())
			}
			if attack == "loop" && downloads.Load() != 3 {
				t.Fatalf("redirect loop made %d requests; expected 3", downloads.Load())
			}
			if attack == "manifest" || attack == "token" {
				if downloads.Load() != 0 {
					t.Fatal("storage origin accepted a non-blob operation")
				}
			}
		})
	}
}

func TestDownloadOriginConfigurationIsExactHTTPS(t *testing.T) {
	for _, origin := range []string{"http://127.0.0.1:4567", "https://*.example.com", "https://host.example/path", "https://user:pass@host.example", "https://host.example?token=secret", "https://host.example#fragment"} {
		_, err := NewPublisher(Target{Origin: "https://registry.example", Repository: "receipts/image", BlobDownloadOrigins: []string{origin}})
		if err == nil {
			t.Fatalf("invalid download origin accepted: %s", origin)
		}
	}
	// Exact origin equality includes the port, never a suffix/prefix match.
	u, _ := url.Parse("https://download.example:8443")
	p, err := NewPublisher(Target{Origin: "https://registry.example", Repository: "receipts/image", BlobDownloadOrigins: []string{u.String()}})
	if err != nil {
		t.Fatal(err)
	}
	if p.downloads["https://download.example"] || p.downloads["https://download.example:8443.evil"] {
		t.Fatal("download origin lost its exact authority")
	}
}
