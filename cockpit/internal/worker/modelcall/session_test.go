package modelcall

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
)

// -----------------------------------------------------------------------------
// Harness
// -----------------------------------------------------------------------------

type recorder struct {
	mu     sync.Mutex
	deltas []*memqlv1.ModelCallDelta
	end    *memqlv1.ModelCallEnd
	done   chan struct{}
	once   sync.Once
}

func newRecorder() *recorder { return &recorder{done: make(chan struct{})} }

func (r *recorder) SendModelCallDelta(requestID string, seq uint64, content string, keepalive bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deltas = append(r.deltas, &memqlv1.ModelCallDelta{
		RequestId: requestID, Seq: seq, Content: content, Keepalive: keepalive,
	})
	return nil
}

func (r *recorder) SendModelCallEnd(end *memqlv1.ModelCallEnd) error {
	r.mu.Lock()
	r.end = end
	r.mu.Unlock()
	r.once.Do(func() { close(r.done) })
	return nil
}

func (r *recorder) wait(t *testing.T) *memqlv1.ModelCallEnd {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for ModelCallEnd")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.end
}

func (r *recorder) content() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, d := range r.deltas {
		b.WriteString(d.GetContent())
	}
	return b.String()
}

// stubInventory is a fixed view of what this machine offers.
type stubInventory struct{ inv models.Inventory }

func (s stubInventory) Models(context.Context) models.Inventory { return s.inv }

func inventoryWith(infos ...models.Info) stubInventory {
	return stubInventory{models.Inventory{Floor: models.FloorVerdict{Met: true}, Models: infos}}
}

func ollamaModel(url, id string, attrs models.Attributes) models.Info {
	return models.Info{ID: id, Kind: models.KindOllama, Runtime: models.KindOllama, BaseURL: url, Allowed: true, Attributes: attrs}
}

func start(requestID, model, kind string) *memqlv1.ModelCallStart {
	return &memqlv1.ModelCallStart{
		RequestId: requestID, Model: model, Kind: kind,
		Messages: []*memqlv1.ModelCallMessage{{Role: "user", Content: "hello"}},
		Limits:   &memqlv1.ModelCallLimits{TimeoutSeconds: 10, IdleTimeoutSeconds: 5, KeepaliveSeconds: 1},
	}
}

// ollamaChatStub streams the given tokens as NDJSON, then a done frame.
func ollamaChatStub(t *testing.T, tokens []string, usage bool) (*httptest.Server, *[]byte) {
	t.Helper()
	var seen []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = io.ReadAll(r.Body)
		flusher, _ := w.(http.Flusher)
		for _, tok := range tokens {
			fmt.Fprintf(w, "%s\n", mustJSON(map[string]any{
				"model": "llama3.1:8b", "message": map[string]string{"content": tok}, "done": false,
			}))
			if flusher != nil {
				flusher.Flush()
			}
		}
		final := map[string]any{"model": "llama3.1:8b", "done": true, "done_reason": "stop"}
		if usage {
			final["prompt_eval_count"] = 11
			final["eval_count"] = 22
		}
		fmt.Fprintf(w, "%s\n", mustJSON(final))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// awaitRequest blocks until the stub runtime has actually received the
// call.
//
// Live() is NOT that signal, and the difference is the whole reason this
// exists: a call is in m.live the moment it is ADMITTED, which is before
// its goroutine has run and therefore before any HTTP request exists.
// Cancelling on Live() alone cancels the context before Do is called, the
// runtime is never contacted, and a test that then waits for the runtime
// to notice a disconnect waits forever for a connection nobody opened.
func awaitRequest(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("the runtime never received the call")
	}
}

// drain reads the request body before the handler blocks.
//
// It is not decoration. Go's HTTP server only starts the background read
// that detects a client disconnect once the request body has been
// consumed -- with an unread POST body the connection reader is parked
// mid-body, and `r.Context()` is never cancelled however hard the client
// hangs up. A stub that skips this cannot observe the cancellation these
// tests exist to assert.
func drain(r *http.Request) { _, _ = io.Copy(io.Discard, r.Body) }

func mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func managerFor(inv Inventory) *Manager {
	return NewManager(Options{Inventory: inv, Getenv: func(string) string { return "" }})
}

// -----------------------------------------------------------------------------
// Chat
// -----------------------------------------------------------------------------

// TestChat_MonotonicSeqAndUsage. The engine drops out-of-order and
// duplicate deltas rather than repairing them, so the sequence has to be
// right on this side or the generation is silently corrupted.
func TestChat_MonotonicSeqAndUsage(t *testing.T) {
	srv, _ := ollamaChatStub(t, []string{"Hel", "lo, ", "world"}, true)
	m := managerFor(inventoryWith(ollamaModel(srv.URL, "llama3.1:8b", models.Attributes{ContextWindow: 8192, MaxConcurrent: 2})))
	rec := newRecorder()

	m.Start(context.Background(), rec, start("r1", "llama3.1:8b", KindChat))
	end := rec.wait(t)

	if got := rec.content(); got != "Hello, world" {
		t.Errorf("content = %q", got)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.deltas) < 3 {
		t.Fatalf("want at least 3 deltas, got %d", len(rec.deltas))
	}
	var last uint64
	for i, d := range rec.deltas {
		if d.GetRequestId() != "r1" {
			t.Errorf("delta %d carries request id %q", i, d.GetRequestId())
		}
		if i > 0 && d.GetSeq() <= last {
			t.Fatalf("seq not strictly increasing: %d then %d", last, d.GetSeq())
		}
		last = d.GetSeq()
	}
	if rec.deltas[0].GetSeq() != 0 {
		t.Errorf("first seq = %d, want 0", rec.deltas[0].GetSeq())
	}
	if end.GetFinishReason() != FinishStop || end.GetErrorCode() != "" {
		t.Errorf("end = %+v", end)
	}
	u := end.GetUsage()
	if u == nil || !u.GetKnown() || u.GetInputTokens() != 11 || u.GetOutputTokens() != 22 || u.GetModel() != "llama3.1:8b" {
		t.Errorf("usage = %+v", u)
	}
}

// TestChat_UsageAbsentStaysAbsent. Silence is recorded as billing
// "unknown"; a confident zero would be recorded as measured.
func TestChat_UsageAbsentStaysAbsent(t *testing.T) {
	srv, _ := ollamaChatStub(t, []string{"hi"}, false)
	m := managerFor(inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})))
	rec := newRecorder()
	m.Start(context.Background(), rec, start("r", "m", KindChat))
	end := rec.wait(t)
	if u := end.GetUsage(); u != nil && u.GetKnown() {
		t.Errorf("usage must not be claimed as known: %+v", u)
	}
}

// TestChat_SchemaReachesTheRuntime. The router only sends a schema to a
// machine that advertised the capability, so a silent downgrade to prose
// here would defeat the gating that put the call on this machine.
func TestChat_SchemaReachesTheRuntime(t *testing.T) {
	srv, body := ollamaChatStub(t, []string{"{}"}, false)
	m := managerFor(inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{StructuredOutput: true, MaxConcurrent: 1})))
	rec := newRecorder()

	s := start("r", "m", KindChat)
	s.ResponseFormatSchema = []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)
	m.Start(context.Background(), rec, s)
	rec.wait(t)

	var sent map[string]any
	if err := json.Unmarshal(*body, &sent); err != nil {
		t.Fatalf("request body: %v", err)
	}
	format, ok := sent["format"]
	if !ok {
		t.Fatal("the schema never reached the runtime")
	}
	if !strings.Contains(mustJSON(format), `"ok"`) {
		t.Errorf("format = %s", mustJSON(format))
	}
}

// TestChat_UnsetParamsAreOmitted. Temperature 0 is a real setting and is
// not the same request as "the caller expressed no preference".
func TestChat_UnsetParamsAreOmitted(t *testing.T) {
	srv, body := ollamaChatStub(t, []string{"x"}, false)
	m := managerFor(inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})))
	rec := newRecorder()

	s := start("r", "m", KindChat)
	s.Params = &memqlv1.ModelCallParams{TopP: 0.9, TopPSet: true}
	m.Start(context.Background(), rec, s)
	rec.wait(t)

	var sent struct {
		Options map[string]any `json:"options"`
	}
	_ = json.Unmarshal(*body, &sent)
	if _, present := sent.Options["temperature"]; present {
		t.Error("an unset temperature must not be sent")
	}
	if got := sent.Options["top_p"]; got != 0.9 {
		t.Errorf("top_p = %v", got)
	}
}

// -----------------------------------------------------------------------------
// Embeddings
// -----------------------------------------------------------------------------

func TestEmbed_VectorsInInputOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":             "nomic",
			"embeddings":        [][]float32{{1, 2}, {3, 4}},
			"prompt_eval_count": 7,
		})
	}))
	defer srv.Close()

	m := managerFor(inventoryWith(ollamaModel(srv.URL, "nomic", models.Attributes{Embeddings: true, MaxConcurrent: 1})))
	rec := newRecorder()
	s := start("r", "nomic", KindEmbedding)
	s.EmbeddingInput = []string{"a", "b"}
	m.Start(context.Background(), rec, s)
	end := rec.wait(t)

	if len(end.GetEmbeddings()) != 2 {
		t.Fatalf("embeddings = %+v", end.GetEmbeddings())
	}
	if end.GetEmbeddings()[0].GetValues()[0] != 1 || end.GetEmbeddings()[1].GetValues()[0] != 3 {
		t.Errorf("vectors out of input order: %+v", end.GetEmbeddings())
	}
	if !end.GetUsage().GetKnown() || end.GetUsage().GetInputTokens() != 7 {
		t.Errorf("usage = %+v", end.GetUsage())
	}
}

// -----------------------------------------------------------------------------
// Admission
// -----------------------------------------------------------------------------

// TestAdmission_Refusals. Each code has to name a DIFFERENT fix -- they
// are read by the engine's refusal report, which is what an operator sees
// when nothing ran.
func TestAdmission_Refusals(t *testing.T) {
	srv, _ := ollamaChatStub(t, []string{"x"}, false)
	blocked := ollamaModel(srv.URL, "blocked", models.Attributes{MaxConcurrent: 1})
	blocked.Allowed = false
	inv := inventoryWith(
		ollamaModel(srv.URL, "plain", models.Attributes{MaxConcurrent: 1}),
		ollamaModel(srv.URL, "embedder", models.Attributes{Embeddings: true, MaxConcurrent: 1}),
		blocked,
	)

	tests := []struct {
		name  string
		start *memqlv1.ModelCallStart
		code  string
	}{
		{"unknown model", start("a", "nope", KindChat), CodeModelNotOffered},
		{"blocked model", start("b", "blocked", KindChat), CodeModelNotOffered},
		{"unsupported kind", start("c", "plain", "summarise"), CodeUnsupportedKind},
		{"embedding on a chat model", start("d", "plain", KindEmbedding), CodeModelNotOffered},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := managerFor(inv)
			rec := newRecorder()
			m.Start(context.Background(), rec, tt.start)
			end := rec.wait(t)
			if end.GetErrorCode() != tt.code {
				t.Errorf("error_code = %q, want %q (error %q)", end.GetErrorCode(), tt.code, end.GetError())
			}
			if end.GetFinishReason() != FinishError {
				t.Errorf("finish_reason = %q", end.GetFinishReason())
			}
			if strings.TrimSpace(end.GetError()) == "" {
				t.Error("a refusal must carry a sentence a person can act on")
			}
		})
	}
}

// TestAdmission_SchemaWithoutTheCapability. A stale advertisement, not a
// routing bug -- and answering prose would surface as a parse failure
// three layers away.
func TestAdmission_SchemaWithoutTheCapability(t *testing.T) {
	srv, _ := ollamaChatStub(t, []string{"prose"}, false)
	m := managerFor(inventoryWith(ollamaModel(srv.URL, "plain", models.Attributes{MaxConcurrent: 1})))
	rec := newRecorder()
	s := start("r", "plain", KindChat)
	s.ResponseFormatSchema = []byte(`{"type":"object"}`)
	m.Start(context.Background(), rec, s)
	if end := rec.wait(t); end.GetErrorCode() != CodeSchemaUnsupported {
		t.Errorf("error_code = %q, want %q", end.GetErrorCode(), CodeSchemaUnsupported)
	}
}

// TestAdmission_ToolsWithoutTheCapability. A runtime handed a tool
// catalogue it cannot honour does not refuse -- it answers in prose, and
// the caller reads prose where it was waiting for a call. So the refusal
// has to happen before the request is built.
func TestAdmission_ToolsWithoutTheCapability(t *testing.T) {
	tools := []Tool{{Name: "lookup", ParametersJSON: `{"type":"object"}`}}
	plain := models.Info{ID: "plain", Kind: models.KindOllama, Allowed: true}

	end := toolsRefusal(plain, tools)
	if end == nil {
		t.Fatal("a model that does not advertise tool calling must be refused before any request reaches the runtime")
	}
	if end.GetErrorCode() != CodeToolsUnsupported {
		t.Errorf("error_code = %q, want %q", end.GetErrorCode(), CodeToolsUnsupported)
	}
	if end.GetFinishReason() != FinishError {
		t.Errorf("finish_reason = %q", end.GetFinishReason())
	}
	if !strings.Contains(end.GetError(), "plain") {
		t.Errorf("error = %q, want it to name the model", end.GetError())
	}

	capable := plain
	capable.Tools = true
	if end := toolsRefusal(capable, tools); end != nil {
		t.Errorf("a model advertising tools must not be refused: %+v", end)
	}
	// The gate is about the CATALOGUE, not the model: an ordinary chat
	// call against a model without tools is the common case and must
	// pass.
	if end := toolsRefusal(plain, nil); end != nil {
		t.Errorf("a call that offers no tools must not be refused: %+v", end)
	}
}

// TestAdmission_PerModelCap. The engine rations by the advertised number,
// but the advertisement is a claim about THIS hardware and two replicas
// selecting at the same moment is an ordinary race.
func TestAdmission_PerModelCap(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drain(r)
		once.Do(func() { close(arrived) })
		<-release
		fmt.Fprintf(w, "%s\n", mustJSON(map[string]any{"done": true, "done_reason": "stop"}))
	}))
	defer srv.Close()
	defer close(release)

	m := managerFor(inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})))
	first := newRecorder()
	m.Start(context.Background(), first, start("one", "m", KindChat))

	// The runtime receiving the call is the signal, not Live(). The slot
	// is taken when the model RESOLVES, which happens in the call's own
	// goroutine -- so a second call racing that goroutine would be
	// refused by the machine-wide ceiling instead of the per-model one,
	// and this test would be asserting whichever one won the race.
	awaitRequest(t, arrived)
	if m.Live() != 1 {
		t.Fatalf("Live() = %d, want 1", m.Live())
	}

	// The second call waits for the slot (admit), for as long as its own
	// idle ceiling -- one second here -- and is refused only then.
	second := newRecorder()
	late := start("two", "m", KindChat)
	late.Limits = &memqlv1.ModelCallLimits{TimeoutSeconds: 10, IdleTimeoutSeconds: 1, KeepaliveSeconds: 1}
	m.Start(context.Background(), second, late)
	end := second.wait(t)
	if end.GetErrorCode() != CodeConcurrencyExceeded {
		t.Errorf("error_code = %q, want %q", end.GetErrorCode(), CodeConcurrencyExceeded)
	}
	if !strings.Contains(end.GetError(), "concurrency limit of 1") {
		t.Errorf("error = %q, want it to name the limit", end.GetError())
	}
}

// TestAdmission_NoRequestIdIsDropped. There is nothing to correlate a
// refusal with, so there is nothing to send.
func TestAdmission_NoRequestIdIsDropped(t *testing.T) {
	m := managerFor(inventoryWith())
	rec := newRecorder()
	m.Start(context.Background(), rec, &memqlv1.ModelCallStart{Model: "m", Kind: KindChat})
	select {
	case <-rec.done:
		t.Fatal("a call with no request id must not produce an End nobody can match")
	case <-time.After(200 * time.Millisecond):
	}
}

// -----------------------------------------------------------------------------
// Deadlines and cancellation
// -----------------------------------------------------------------------------

// TestCancel_EndsCancelledAndStopsTheRuntime. A call that merely stops
// being read leaves a GPU busy for the length of a generation nobody will
// use.
func TestCancel_EndsCancelledAndStopsTheRuntime(t *testing.T) {
	gone := make(chan struct{}, 1)
	arrived := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drain(r)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "%s\n", mustJSON(map[string]any{"message": map[string]string{"content": "tick"}, "done": false}))
		if flusher != nil {
			flusher.Flush()
		}
		once.Do(func() { close(arrived) })
		<-r.Context().Done()
		gone <- struct{}{}
	}))
	defer srv.Close()

	m := managerFor(inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})))
	rec := newRecorder()
	m.Start(context.Background(), rec, start("r", "m", KindChat))

	awaitRequest(t, arrived)
	m.Cancel(&memqlv1.ModelCallCancel{RequestId: "r", Reason: "the plan was abandoned"})

	end := rec.wait(t)
	if end.GetFinishReason() != FinishCancelled || end.GetErrorCode() != CodeCancelled {
		t.Errorf("end = %+v", end)
	}
	if end.GetError() != "the plan was abandoned" {
		t.Errorf("error = %q, want the reason the cluster gave", end.GetError())
	}
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream request was never aborted; the GPU is still busy")
	}
}

// TestIdleTimeout_EndsTimeout. A runtime that accepted the request and
// then went quiet is THIS machine's problem to notice: waiting for the
// engine's copy of the same deadline would not free the hardware.
func TestIdleTimeout_EndsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drain(r)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "%s\n", mustJSON(map[string]any{"message": map[string]string{"content": "one"}, "done": false}))
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := managerFor(inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})))
	rec := newRecorder()
	s := start("r", "m", KindChat)
	s.Limits = &memqlv1.ModelCallLimits{TimeoutSeconds: 30, IdleTimeoutSeconds: 1}
	m.Start(context.Background(), rec, s)

	end := rec.wait(t)
	if end.GetFinishReason() != FinishTimeout || end.GetErrorCode() != CodeTimeout {
		t.Errorf("end = %+v", end)
	}
}

// TestKeepalive_ProvesLifeWithoutContent. The keepalive is what makes the
// idle ceiling enforceable on the other side rather than a guess, and it
// takes a seq of its own so it can never be read as a replayed content
// delta.
func TestKeepalive_ProvesLifeWithoutContent(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drain(r)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprintf(w, "%s\n", mustJSON(map[string]any{"done": true, "done_reason": "stop"}))
	}))
	defer srv.Close()

	m := managerFor(inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})))
	rec := newRecorder()
	s := start("r", "m", KindChat)
	// A generous idle ceiling with a fast keepalive: the point is that
	// silence is filled, not that the call dies.
	s.Limits = &memqlv1.ModelCallLimits{TimeoutSeconds: 30, IdleTimeoutSeconds: 10, KeepaliveSeconds: 1}
	m.Start(context.Background(), rec, s)

	time.Sleep(2500 * time.Millisecond)
	close(release)
	rec.wait(t)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var keepalives int
	var last uint64
	for i, d := range rec.deltas {
		if d.GetKeepalive() {
			keepalives++
			if d.GetContent() != "" {
				t.Errorf("keepalive %d carried content %q", i, d.GetContent())
			}
		}
		if i > 0 && d.GetSeq() <= last {
			t.Fatalf("keepalives must share the monotonic counter: %d then %d", last, d.GetSeq())
		}
		last = d.GetSeq()
	}
	if keepalives == 0 {
		t.Fatal("no keepalive was emitted during a silent generation")
	}
}

// TestLimits_KeepaliveTightenedUnderTheIdleCeiling. A keepalive at or
// above the idle ceiling cannot keep anything alive; the first one would
// arrive after the deadline it exists to push back.
func TestLimits_KeepaliveTightenedUnderTheIdleCeiling(t *testing.T) {
	got := limitsFrom(&memqlv1.ModelCallLimits{IdleTimeoutSeconds: 10, KeepaliveSeconds: 30})
	if got.keepalive >= got.idle {
		t.Fatalf("keepalive %v is not under the idle ceiling %v", got.keepalive, got.idle)
	}
	defaults := limitsFrom(nil)
	if defaults.timeout != DefaultTimeout || defaults.idle != DefaultIdleTimeout || defaults.keepalive != DefaultKeepalive {
		t.Errorf("silence must get the envelope defaults, got %+v", defaults)
	}
}

// TestStopAll_EndsEveryLiveCall. Whether the End reaches the cluster is
// secondary; freeing the hardware is not.
func TestStopAll_EndsEveryLiveCall(t *testing.T) {
	gone := make(chan struct{}, 1)
	arrived := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drain(r)
		once.Do(func() { close(arrived) })
		<-r.Context().Done()
		gone <- struct{}{}
	}))
	defer srv.Close()

	m := managerFor(inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})))
	rec := newRecorder()
	m.Start(context.Background(), rec, start("r", "m", KindChat))
	awaitRequest(t, arrived)

	m.StopAll("the worker's stream to the cluster was lost")
	if end := rec.wait(t); end.GetErrorCode() != CodeWorkerStopped {
		t.Errorf("error_code = %q, want %q", end.GetErrorCode(), CodeWorkerStopped)
	}
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream generation was never stopped")
	}
	// The slot is released by a defer that runs AFTER the End is sent,
	// so a bare read here races the goroutine that is already on its way
	// out. Poll rather than sleep: the property is "it is released",
	// not "it is released within one scheduler tick".
	released := time.Now().Add(5 * time.Second)
	for m.Live() != 0 && time.Now().Before(released) {
		time.Sleep(5 * time.Millisecond)
	}
	if m.Live() != 0 {
		t.Errorf("Live() = %d after StopAll; the concurrency slot leaked", m.Live())
	}
}

// A KEEPALIVE MUST NOT PUSH BACK THE IDLE DEADLINE, and this is the
// unit-level statement of the bug that made TestIdleTimeout_EndsTimeout
// take anywhere between one second and never.
//
// The two clocks answer different questions. `lastSend` is "when did
// this worker last put anything on the stream", which is what the
// keepalive cadence is measured against. `lastContent` is "when did the
// RUNTIME last produce output", which is the only thing the idle
// ceiling can honestly be about -- a keepalive is this worker's own
// output and says nothing whatever about the runtime behind it.
//
// One clock for both is the trap: with keepalive at half the idle
// ceiling, every keepalive resets the deadline it exists to enforce, so
// the ceiling is reached only when scheduling jitter lands a tick late.
// In production that is worse than a flaky test -- a wedged runtime is
// never noticed, the call runs to the whole-call timeout holding the
// GPU, and the engine, receiving keepalives, believes the machine is
// healthy the entire time.
func TestKeepaliveDoesNotResetTheIdleClock(t *testing.T) {
	s := &deltaStream{sender: newRecorder(), requestID: "r"}
	s.touch()

	time.Sleep(20 * time.Millisecond)
	if err := s.keepalive(); err != nil {
		t.Fatal(err)
	}

	if s.idleFor() < 20*time.Millisecond {
		t.Fatalf("a keepalive reset the idle clock: idleFor = %s", s.idleFor())
	}
	// And it DID reset the send cadence, which is the half that keeps
	// the engine's own idle timer alive.
	if s.sinceSend() > 5*time.Millisecond {
		t.Fatalf("a keepalive did not reset the send cadence: sinceSend = %s", s.sinceSend())
	}
}

// Content moves BOTH clocks: the runtime spoke, so the idle ceiling
// restarts, and something went out, so the keepalive cadence restarts
// too.
func TestContentResetsBothClocks(t *testing.T) {
	s := &deltaStream{sender: newRecorder(), requestID: "r"}
	s.touch()

	time.Sleep(20 * time.Millisecond)
	if err := s.emit("hello"); err != nil {
		t.Fatal(err)
	}

	if s.idleFor() > 5*time.Millisecond {
		t.Fatalf("content did not reset the idle clock: idleFor = %s", s.idleFor())
	}
	if s.sinceSend() > 5*time.Millisecond {
		t.Fatalf("content did not reset the send cadence: sinceSend = %s", s.sinceSend())
	}
}

// The whole point, at the level it actually matters: a runtime that
// goes quiet is ended by THIS machine, even though the worker keeps
// answering the engine with keepalives the entire time. Both facts are
// asserted together, because fixing one by dropping the other would
// pass a narrower test and break the engine's own idle detection.
func TestASilentRuntimeIsEndedWhileKeepalivesStillFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drain(r)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "%s\n", mustJSON(map[string]any{"message": map[string]string{"content": "one"}, "done": false}))
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := managerFor(inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})))
	rec := newRecorder()
	s := start("r", "m", KindChat)
	s.Limits = &memqlv1.ModelCallLimits{TimeoutSeconds: 30, IdleTimeoutSeconds: 2, KeepaliveSeconds: 1}
	m.Start(context.Background(), rec, s)

	end := rec.wait(t)
	if end.GetFinishReason() != FinishTimeout || end.GetErrorCode() != CodeTimeout {
		t.Fatalf("end = %+v, want the idle ceiling to have ended it", end)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	keepalives := 0
	for _, d := range rec.deltas {
		if d.GetKeepalive() {
			keepalives++
		}
	}
	if keepalives == 0 {
		t.Fatal("no keepalive was sent; the engine had no reason to believe this worker was alive")
	}
}

// KEEPALIVES MUST FIRE RELIABLY, not on a coin flip.
//
// The watchdog checks `sinceSend() >= limits.keepalive`, so a check
// cadence equal to that threshold makes every tick land within
// scheduling jitter of the value it is testing -- and roughly half the
// calls emit no keepalive at all. The engine then sees silence it
// cannot distinguish from a wedged machine, which is the exact
// confusion the keepalive contract exists to remove.
//
// Asserted over repetitions, because a single run of a timing property
// proves nothing about the one that fails under load.
func TestKeepalivesFireOnEveryRun(t *testing.T) {
	for i := 0; i < 5; i++ {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			drain(r)
			flusher, _ := w.(http.Flusher)
			fmt.Fprintf(w, "%s\n", mustJSON(map[string]any{"message": map[string]string{"content": "one"}, "done": false}))
			if flusher != nil {
				flusher.Flush()
			}
			<-r.Context().Done()
		}))

		m := managerFor(inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})))
		rec := newRecorder()
		s := start("r", "m", KindChat)
		s.Limits = &memqlv1.ModelCallLimits{TimeoutSeconds: 30, IdleTimeoutSeconds: 2, KeepaliveSeconds: 1}
		m.Start(context.Background(), rec, s)

		end := rec.wait(t)
		srv.Close()

		if end.GetFinishReason() != FinishTimeout {
			t.Fatalf("run %d: finish = %q, want the idle ceiling to have ended it", i, end.GetFinishReason())
		}
		rec.mu.Lock()
		keepalives := 0
		for _, d := range rec.deltas {
			if d.GetKeepalive() {
				keepalives++
			}
		}
		rec.mu.Unlock()
		if keepalives == 0 {
			t.Fatalf("run %d: no keepalive was sent before the idle ceiling fired", i)
		}
	}
}

// TestEmbed_SendsTheWorkingContext pins the one option an embedding call
// carries: without it Ollama loads the embedder at its VRAM-tier default
// context, which on a 24 GB card is a 3.8 GB cache in front of a 639 MB
// model, and the embedder cannot sit beside the 27B the class recommends.
func TestEmbed_SendsTheWorkingContext(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "nomic", "embeddings": [][]float32{{1, 2}}, "prompt_eval_count": 3,
		})
	}))
	defer srv.Close()

	m := managerFor(inventoryWith(ollamaModel(srv.URL, "nomic", models.Attributes{Embeddings: true, MaxConcurrent: 1})))
	rec := newRecorder()
	s := start("r", "nomic", KindEmbedding)
	s.EmbeddingInput = []string{"a"}
	m.Start(context.Background(), rec, s)
	rec.wait(t)

	opts, _ := body["options"].(map[string]any)
	if got, _ := opts["num_ctx"].(float64); int(got) != embedWorkingContext {
		t.Fatalf("num_ctx = %v, want %d; body %v", opts["num_ctx"], embedWorkingContext, body)
	}
}

func TestChatForwardsContextFromWorkerEnvelopeToOllama(t *testing.T) {
	for _, tokens := range []int64{32768, 0} {
		t.Run(fmt.Sprint(tokens), func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/chat" {
					t.Errorf("path = %s", r.URL.Path)
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				io.WriteString(w, `{"model":"chat","message":{"content":"ok"},"done":true}`+"\n")
			}))
			defer srv.Close()
			m := managerFor(inventoryWith(ollamaModel(srv.URL, "chat", models.Attributes{ContextWindow: 65536, MaxConcurrent: 1})))
			rec := newRecorder()
			request := start("r", "chat", KindChat)
			request.Params = &memqlv1.ModelCallParams{ContextTokens: tokens}
			m.Start(context.Background(), rec, request)
			if end := rec.wait(t); end.Error != "" {
				t.Fatalf("call failed: %v", end.Error)
			}
			opts, _ := body["options"].(map[string]any)
			got, exists := opts["num_ctx"]
			if tokens > 0 && got != float64(tokens) {
				t.Errorf("num_ctx = %v, want %d", got, tokens)
			}
			if tokens == 0 && exists {
				t.Errorf("absent context became %v", got)
			}
		})
	}
}
