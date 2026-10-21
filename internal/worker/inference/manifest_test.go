package inference

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestURLFollowsOllamasSpellings(t *testing.T) {
	old := registryBase
	registryBase = "https://registry.example"
	t.Cleanup(func() { registryBase = old })
	for id, want := range map[string]string{
		"qwen3.5:9b":                   "https://registry.example/v2/library/qwen3.5/manifests/9b",
		"qwen3-embedding":              "https://registry.example/v2/library/qwen3-embedding/manifests/latest",
		"someone/model:q4":             "https://registry.example/v2/someone/model/manifests/q4",
		"hf.co/Qwen/Qwen3-GGUF:Q4_K_M": "https://hf.co/v2/Qwen/Qwen3-GGUF/manifests/Q4_K_M",
		"":                             "",
		"bad id":                       "",
		"localhost:5000/a/b:c":         "",
	} {
		if got := manifestURL(id); got != want {
			t.Errorf("manifestURL(%q) = %q, want %q", id, got, want)
		}
	}
}

// The preflight counts only what is not already complete on disk: a
// resumed pull whose big layer landed earlier needs only the rest.
func TestMissingBytesSubtractsCompleteBlobs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/library/qwen3.5/manifests/9b" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"config":{"digest":"sha256:c0","size":500},"layers":[
			{"digest":"sha256:aa","size":6000},{"digest":"sha256:bb","size":40}]}`))
	}))
	defer srv.Close()
	old := registryBase
	registryBase = srv.URL
	t.Cleanup(func() { registryBase = old })

	models := t.TempDir()
	if got, ok := MissingBytes(context.Background(), models, "qwen3.5:9b"); !ok || got != 6540 {
		t.Fatalf("nothing on disk: MissingBytes = %d, %v; want 6540, true", got, ok)
	}
	blobs := filepath.Join(models, "blobs")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	// The big layer complete, the small one partial (a different size).
	if err := os.WriteFile(filepath.Join(blobs, "sha256-aa"), make([]byte, 6000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobs, "sha256-bb"), make([]byte, 10), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok := MissingBytes(context.Background(), models, "qwen3.5:9b"); !ok || got != 540 {
		t.Fatalf("big layer on disk: MissingBytes = %d, %v; want 540, true", got, ok)
	}
	// An unknown model, or no registry answer, establishes nothing.
	if _, ok := MissingBytes(context.Background(), models, "nope:1"); ok {
		t.Error("a model the registry does not know must be unknown, not zero")
	}
}

func TestDiskShortfallNamesBothNumbersAndKeepsAMargin(t *testing.T) {
	if s := DiskShortfall(5e9, 7e9, "/x"); s != "" {
		t.Errorf("5 GB missing with 7 GB free fits with the margin, got %q", s)
	}
	s := DiskShortfall(5e9, 5.5e9, "/Users/op/.ollama/models")
	for _, want := range []string{"about 6.0 GB more", "5.5 GB is free", "(/Users/op/.ollama/models)"} {
		if !strings.Contains(s, want) {
			t.Errorf("shortfall must say %q, got %q", want, s)
		}
	}
	if DiskShortfall(0, 1, "/x") != "" || DiskShortfall(1, 0, "/x") != "" {
		t.Error("nothing missing, or free space not established, refuses nothing")
	}
}
