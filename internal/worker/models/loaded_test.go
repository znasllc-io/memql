package models

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// psFixture is a recorded GET /api/ps from Ollama 0.34.2: one model kept
// forever (expires_at is the far future Ollama uses for -1) and one kept
// for a finite time, the second only partly on the GPU.
const psFixture = `{"models":[
 {"name":"qwen3.8:27b","model":"qwen3.8:27b","size":18400000000,"digest":"d1","details":{"family":"qwen35"},
  "expires_at":"2319-01-28T19:03:29.099855393Z","size_vram":18400000000,"context_length":16384,"runner":"llamacpp"},
 {"name":"big:70b","model":"big:70b","size":40000000000,"digest":"d2","details":{"family":"llama"},
  "expires_at":"2026-10-18T14:30:00-07:00","size_vram":24000000000,"context_length":8192,"runner":"llamacpp"}
]}`

func psStub(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLoaded(t *testing.T) {
	srv := psStub(t, 200, psFixture)
	got, err := discovererFor(srv.URL, nil).Loaded(context.Background())
	if err != nil {
		t.Fatalf("Loaded: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Loaded = %+v, want two models", got)
	}
	now := time.Date(2026, 10, 18, 12, 30, 0, 0, time.FixedZone("PDT", -7*3600))

	big := got[0]
	if big.ID != "qwen3.8:27b" || big.SizeBytes != 18400000000 || big.ContextLength != 16384 {
		t.Errorf("first model = %+v", big)
	}
	if !big.Forever(now) {
		t.Errorf("expires_at %v is Ollama's spelling of keep_alive -1, and must read as forever", big.ExpiresAt)
	}
	if p := big.GPUPercent(); p != 100 {
		t.Errorf("GPUPercent = %d, want 100", p)
	}

	part := got[1]
	if part.Forever(now) {
		t.Errorf("a model expiring in two hours read as forever")
	}
	if d := part.ExpiresAt.Sub(now); d != 2*time.Hour {
		t.Errorf("expires in %v, want 2h", d)
	}
	if p := part.GPUPercent(); p != 60 {
		t.Errorf("GPUPercent = %d, want 60 (24 GB of 40 GB on the card)", p)
	}
}

// TestLoadedNothingLoadedIsAnAnswer. An Ollama with nothing in memory is
// the ordinary state of an untuned machine, and it is not an error.
func TestLoadedNothingLoadedIsAnAnswer(t *testing.T) {
	srv := psStub(t, 200, `{"models":[]}`)
	got, err := discovererFor(srv.URL, nil).Loaded(context.Background())
	if err != nil || len(got) != 0 {
		t.Errorf("Loaded = %+v, %v; want an empty list and no error", got, err)
	}
}

// TestLoadedSaysWhyItCouldNotAsk. A runtime that is down, or an Ollama too
// old to have /api/ps, is a sentence for the report and never a failure of
// the command that prints it. The sentence names the URL once, where the
// question went; the cause after it must not repeat the URL.
func TestLoadedSaysWhyItCouldNotAsk(t *testing.T) {
	check := func(name, base string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: want an error", name)
			return
		}
		if !strings.HasPrefix(err.Error(), "no answer from Ollama's /api/ps at "+base+" (") {
			t.Errorf("%s: the sentence is %q", name, err)
		}
		if n := strings.Count(err.Error(), base); n != 1 {
			t.Errorf("%s: the URL %s appears %d times in %q, want once", name, base, n, err)
		}
	}
	for name, srv := range map[string]*httptest.Server{
		"a route Ollama does not have": psStub(t, 404, "not found"),
		"a body that is not JSON":      psStub(t, 200, "<html>"),
	} {
		_, err := discovererFor(srv.URL, nil).Loaded(context.Background())
		check(name, srv.URL, err)
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	_, err := discovererFor(dead.URL, nil).Loaded(context.Background())
	check("a runtime that is down", dead.URL, err)
}
