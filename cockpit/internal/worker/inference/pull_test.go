package inference

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
)

// The bodies below are Ollama's own, recorded from docs/api.md's "Pull a
// Model" section (read 2026-09-07) and from server/routes.go's
// PullHandler, which pushes a pull failure into the SAME 200 stream as
// gin.H{"error": err.Error()} rather than answering with a status code.
// They are pasted verbatim rather than built from the Progress struct so
// that a change to this package's own types cannot quietly change what
// the tests claim the runtime sends.

// ollamaStub serves one recorded NDJSON body and records what was asked
// for. flushEach makes each line reach the client on its own, which is
// what makes the progress callbacks observable in order rather than all
// at once at EOF.
type ollamaStub struct {
	*httptest.Server

	mu     sync.Mutex
	path   string
	body   map[string]any
	method string
}

func newOllamaStub(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, s *ollamaStub)) *ollamaStub {
	t.Helper()
	s := &ollamaStub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		s.mu.Lock()
		s.path, s.method, s.body = r.URL.Path, r.Method, decoded
		s.mu.Unlock()
		handle(w, r, s)
	}))
	t.Cleanup(s.Close)
	return s
}

// stream writes each line and flushes, so the client sees them one at a
// time exactly as Ollama's chunked NDJSON arrives.
func stream(w http.ResponseWriter, lines ...string) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	for _, line := range lines {
		_, _ = io.WriteString(w, line+"\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func collect(p *[]Progress) func(Progress) {
	return func(pr Progress) { *p = append(*p, pr) }
}

// TestPullReportsProgressInOrder over a recorded multi-chunk download.
func TestPullReportsProgressInOrder(t *testing.T) {
	srv := newOllamaStub(t, func(w http.ResponseWriter, r *http.Request, s *ollamaStub) {
		stream(w,
			`{"status":"pulling manifest"}`,
			`{"status":"pulling 8934d96d3f08","digest":"sha256:8934d96d3f08","total":2142590208}`,
			`{"status":"pulling 8934d96d3f08","digest":"sha256:8934d96d3f08","total":2142590208,"completed":241970}`,
			`{"status":"pulling 8934d96d3f08","digest":"sha256:8934d96d3f08","total":2142590208,"completed":2142590208}`,
			`{"status":"verifying sha256 digest"}`,
			`{"status":"writing manifest"}`,
			`{"status":"removing any unused layers"}`,
			`{"status":"success"}`,
		)
	})

	var got []Progress
	if err := Pull(context.Background(), srv.URL, "llama3.1:8b", collect(&got)); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	want := []Progress{
		{Model: "llama3.1:8b", Status: "pulling manifest"},
		{Model: "llama3.1:8b", Status: "pulling 8934d96d3f08", Total: 2142590208},
		{Model: "llama3.1:8b", Status: "pulling 8934d96d3f08", Total: 2142590208, Completed: 241970},
		{Model: "llama3.1:8b", Status: "pulling 8934d96d3f08", Total: 2142590208, Completed: 2142590208},
		{Model: "llama3.1:8b", Status: "verifying sha256 digest"},
		{Model: "llama3.1:8b", Status: "writing manifest"},
		{Model: "llama3.1:8b", Status: "removing any unused layers"},
		{Model: "llama3.1:8b", Status: "success"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("progress:\n got %+v\nwant %+v", got, want)
	}
}

// TestPullAsksTheRuntimeOverItsHTTPAPI. The endpoint and the request shape
// are the contract with Ollama, and getting either wrong is a 404 an
// operator reads as "the runtime is broken".
func TestPullAsksTheRuntimeOverItsHTTPAPI(t *testing.T) {
	srv := newOllamaStub(t, func(w http.ResponseWriter, r *http.Request, s *ollamaStub) {
		stream(w, `{"status":"success"}`)
	})

	if err := Pull(context.Background(), srv.URL, "nomic-embed-text", nil); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.method != http.MethodPost || srv.path != "/api/pull" {
		t.Errorf("asked %s %s, want POST /api/pull", srv.method, srv.path)
	}
	if srv.body["model"] != "nomic-embed-text" {
		t.Errorf("model = %v, want nomic-embed-text", srv.body["model"])
	}
	if srv.body["stream"] != true {
		t.Errorf("stream = %v, want true -- without it Ollama answers once at the end and there is no progress at all", srv.body["stream"])
	}
}

// TestPullPassesHuggingFaceIdsThroughUnchanged. Ollama resolves
// hf.co/<owner>/<repo> itself, and the `:<quant>` suffix is part of the
// id rather than a tag this side may normalise -- lowercasing it, or
// splitting on the colon and keeping the left half, silently pulls a
// DIFFERENT quantization from the one the person asked for, and the
// machine then advertises a model id that no longer matches what is on
// disk.
func TestPullPassesHuggingFaceIdsThroughUnchanged(t *testing.T) {
	for _, id := range []string{
		"hf.co/bartowski/Llama-3.2-3B-Instruct-GGUF",
		"hf.co/bartowski/Llama-3.2-3B-Instruct-GGUF:Q8_0",
		"hf.co/Qwen/Qwen2.5-7B-Instruct-GGUF:IQ4_XS",
	} {
		srv := newOllamaStub(t, func(w http.ResponseWriter, r *http.Request, s *ollamaStub) {
			stream(w, `{"status":"success"}`)
		})
		var got []Progress
		if err := Pull(context.Background(), srv.URL, id, collect(&got)); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		srv.mu.Lock()
		sent := srv.body["model"]
		srv.mu.Unlock()
		if sent != id {
			t.Errorf("sent %v, want %q byte for byte", sent, id)
		}
		if len(got) != 1 || got[0].Model != id {
			t.Errorf("progress named %+v, want the id as asked", got)
		}
	}
}

// TestPullCarriesTheRuntimesOwnErrorOutOfA200Stream.
//
// THE 200 IS THE POINT. Ollama's PullHandler emits "pulling manifest"
// before it fetches anything, so the response headers are long gone by the
// time a failure happens, and the failure arrives as an ordinary object in
// the body with an "error" key. A client that trusted the status code
// would read "no such model" as a completed pull -- the same trap as the
// engine's /memql/query, which answers 200 with the refusal in an errors
// array (CLAUDE.md).
func TestPullCarriesTheRuntimesOwnErrorOutOfA200Stream(t *testing.T) {
	srv := newOllamaStub(t, func(w http.ResponseWriter, r *http.Request, s *ollamaStub) {
		stream(w,
			`{"status":"pulling manifest"}`,
			`{"error":"pull model manifest: file does not exist"}`,
		)
	})

	var got []Progress
	err := Pull(context.Background(), srv.URL, "llama3.1:8b-does-not-exist", collect(&got))
	if err == nil {
		t.Fatal("a 200 whose body carries an error is a FAILED pull")
	}
	if !errors.Is(err, ErrPullFailed) {
		t.Errorf("err = %v, want ErrPullFailed", err)
	}
	// The runtime's own words: they are the only thing that tells "no such
	// model" apart from "no space left on device" and from a registry that
	// stopped answering.
	if !strings.Contains(err.Error(), "pull model manifest: file does not exist") {
		t.Errorf("err = %q, want the runtime's own sentence in it", err)
	}
	if !strings.Contains(err.Error(), "pulling manifest") {
		t.Errorf("err = %q, want the last status line in it", err)
	}
	if len(got) != 1 || got[0].Status != "pulling manifest" {
		t.Errorf("progress %+v, want the statuses seen before the failure", got)
	}
}

// TestPullOnAStreamThatEndsWithoutSuccess. A runtime killed mid-pull
// closes the body cleanly, and nothing in what was received says the pull
// did not finish. Reading that as success writes the model into
// models.allow, the machine advertises a model it does not have, and the
// calls fail on somebody else's prompt -- so the absence of the final
// "success" is itself the error.
func TestPullOnAStreamThatEndsWithoutSuccess(t *testing.T) {
	srv := newOllamaStub(t, func(w http.ResponseWriter, r *http.Request, s *ollamaStub) {
		stream(w,
			`{"status":"pulling manifest"}`,
			`{"status":"pulling 8934d96d3f08","digest":"sha256:8934d96d3f08","total":2142590208,"completed":18000000}`,
		)
	})

	err := Pull(context.Background(), srv.URL, "llama3.1:8b", nil)
	if !errors.Is(err, ErrPullIncomplete) {
		t.Fatalf("err = %v, want ErrPullIncomplete", err)
	}
	if !strings.Contains(err.Error(), "pulling 8934d96d3f08") {
		t.Errorf("err = %q, want the last status line in it", err)
	}
}

// TestPullOnAStreamCutMidObject is the same failure one byte earlier: the
// connection died with half a JSON object on the wire.
func TestPullOnAStreamCutMidObject(t *testing.T) {
	srv := newOllamaStub(t, func(w http.ResponseWriter, r *http.Request, s *ollamaStub) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"status":"pulling manifest"}`+"\n"+`{"status":"pulling 89`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})

	err := Pull(context.Background(), srv.URL, "llama3.1:8b", nil)
	if err == nil {
		t.Fatal("a truncated stream is not a completed pull")
	}
	if !strings.Contains(err.Error(), "pulling manifest") {
		t.Errorf("err = %q, want the last status line in it", err)
	}
}

// TestPullStopsPromptlyOnACancelledContextAndDoesNotReportSuccess.
func TestPullStopsPromptlyOnACancelledContextAndDoesNotReportSuccess(t *testing.T) {
	srv := newOllamaStub(t, func(w http.ResponseWriter, r *http.Request, s *ollamaStub) {
		stream(w,
			`{"status":"pulling manifest"}`,
			`{"status":"pulling 8934d96d3f08","digest":"sha256:8934d96d3f08","total":2142590208,"completed":1}`,
		)
		// Hold the response open the way a real multi-gigabyte pull does,
		// and let go the moment the client hangs up.
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var got []Progress
	err := Pull(ctx, srv.URL, "llama3.1:8b", func(p Progress) {
		got = append(got, p)
		if len(got) == 2 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(got) != 2 {
		t.Errorf("progress %+v, want the two seen before the cancel", got)
	}
}

// TestPullOnAnHTTPError. Ollama answers 400 before streaming starts for a
// model reference it cannot even parse, and the message is in the body.
func TestPullOnAnHTTPError(t *testing.T) {
	srv := newOllamaStub(t, func(w http.ResponseWriter, r *http.Request, s *ollamaStub) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid model name"}`)
	})

	err := Pull(context.Background(), srv.URL, "::::", nil)
	if err == nil {
		t.Fatal("want an error for a 400")
	}
	if !strings.Contains(err.Error(), "invalid model name") {
		t.Errorf("err = %q, want the runtime's own message", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("err = %q, want the status code", err)
	}
}

// TestPullRefusesAnEmptyModel before it opens a socket. Ollama would
// answer 400, but "" is a caller bug and the error should say so rather
// than quote a runtime that was never at fault.
func TestPullRefusesAnEmptyModel(t *testing.T) {
	if err := Pull(context.Background(), "http://127.0.0.1:1", "  ", nil); err == nil {
		t.Fatal("want an error for an empty model id")
	}
}

// TestPullURL. The empty base is the ordinary machine, and it must resolve
// to the same default the models package probes -- a second spelling of
// the port here would make `memql worker models` and a pull disagree about
// where the runtime is.
func TestPullURL(t *testing.T) {
	for base, want := range map[string]string{
		"":                             models.DefaultOllamaBaseURL + "/api/pull",
		"http://127.0.0.1:11434":       "http://127.0.0.1:11434/api/pull",
		"http://127.0.0.1:11434/":      "http://127.0.0.1:11434/api/pull",
		"  http://10.0.0.4:11434/  ":   "http://10.0.0.4:11434/api/pull",
		"http://ollama.internal:11434": "http://ollama.internal:11434/api/pull",
	} {
		if got := pullURL(base); got != want {
			t.Errorf("pullURL(%q) = %q, want %q", base, got, want)
		}
	}
}

// TestPullSurvivesANilProgressCallback. The remote path in PR 2 always has
// one; a caller that only wants the model on disk should not have to
// invent one.
func TestPullSurvivesANilProgressCallback(t *testing.T) {
	srv := newOllamaStub(t, func(w http.ResponseWriter, r *http.Request, s *ollamaStub) {
		stream(w, `{"status":"pulling manifest"}`, `{"status":"success"}`)
	})
	if err := Pull(context.Background(), srv.URL, "llama3.1:8b", nil); err != nil {
		t.Fatalf("Pull: %v", err)
	}
}
