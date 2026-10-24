package appsession

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the multipart wire, not just a classifier: CreateFormFile silently
// assigned octet-stream to JSON and made otherwise intact evidence unreadable.
func TestLibraryPushContentTypePreservesBytes(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"checkpoint.json", "{\"verified\":true,\"label\":\"café\"}\n", "application/json"},
		{"CHECKPOINT.JSON", "[1, 2]\n", "application/json"},
		{"report.md", "# Verification\nPassed.\n", "text/markdown; charset=utf-8"},
		{"records.jsonl", "{\"a\":1}\n{\"a\":2}\n", "text/plain; charset=utf-8"},
		{"build.log", "ok\t1\r\n", "text/plain; charset=utf-8"},
		{"invalid.json", "{unfinished", "text/plain; charset=utf-8"},
		{"empty.json", "", "application/octet-stream"},
		{"image.json", "\x89PNG\r\n\x1a\nrest", "image/png"},
		{"archive.md", "PK\x03\x04rest", "application/zip"},
		{"binary.json", strings.Repeat(" ", 600) + "\x00", "application/octet-stream"},
		{"invalid-utf8.md", strings.Repeat("x", 600) + "\xff", "application/octet-stream"},
		{"control.txt", "text\x1b[31m", "application/octet-stream"},
		{"quote\"\\\r\nname.json", "{\"ok\":true}", "application/json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/artifacts" || r.Header.Get("Authorization") != "Bearer "+testBearer {
					t.Errorf("upload identity or endpoint changed")
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Fatal(err)
				}
				defer r.MultipartForm.RemoveAll()
				file, header, err := r.FormFile("file")
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				body, err := io.ReadAll(file)
				if err != nil {
					t.Fatal(err)
				}
				if got := header.Header.Get("Content-Type"); got != tc.want {
					t.Errorf("MIME = %q, want %q", got, tc.want)
				}
				if !bytes.Equal(body, []byte(tc.body)) || sha256.Sum256(body) != sha256.Sum256([]byte(tc.body)) {
					t.Error("upload bytes changed")
				}
				if r.FormValue("name") != tc.name {
					t.Error("name field changed")
				}
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"artifactId":"saved","fileId":"file"}`)
			}))
			defer srv.Close()
			lib := NewLibrary(srv.URL, testBearer, srv.Client())
			if id, err := lib.PushBytes(context.Background(), []byte(tc.body), tc.name); err != nil || id != "saved" {
				t.Fatalf("PushBytes: %q %v", id, err)
			}
			path := filepath.Join(t.TempDir(), "output")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			if id, err := lib.Push(context.Background(), path, tc.name); err != nil || id != "saved" {
				t.Fatalf("Push: %q %v", id, err)
			}
			if calls != 2 {
				t.Fatalf("sent %d requests", calls)
			}
		})
	}
}
