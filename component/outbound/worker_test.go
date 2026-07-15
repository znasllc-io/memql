package outbound

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// memql#2521 -- the outbound-delivery worker. Products stage
// v1:platform:outboundRequest rows from pure DSL; the worker drains
// pending/retrying rows, applies the deploy-layer policy (allowlists,
// payload cap), delivers through the medium's transport, and stamps the
// status transition back. Tests drive drainOnce directly (cron-leader /
// watchdog test precedent) with a fake engine + transport.

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fakeEngine answers the drain scans with canned rows and records every
// mutation the worker stamps.
type fakeEngine struct {
	mu       sync.Mutex
	pending  []map[string]any
	retrying []map[string]any
	calls    []string
}

func (e *fakeEngine) Execute(_ context.Context, q string) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, q)
	switch {
	case strings.Contains(q, `outboundRequestsByStatus(status: "pending")`):
		return rowsEnvelope(e.pending), nil
	case strings.Contains(q, `outboundRequestsByStatus(status: "retrying")`):
		return rowsEnvelope(e.retrying), nil
	default:
		return nil, nil
	}
}

func (e *fakeEngine) stamped() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, c := range e.calls {
		if strings.HasPrefix(c, "mutation ") {
			out = append(out, c)
		}
	}
	return out
}

func rowsEnvelope(rows []map[string]any) any {
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	return map[string]any{"output": out}
}

type fakeTransport struct {
	mu        sync.Mutex
	delivered []Request
	err       error
}

func (t *fakeTransport) Deliver(_ context.Context, req Request) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.delivered = append(t.delivered, req)
	return t.err
}

func (t *fakeTransport) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.delivered)
}

type fakeClaimer struct {
	mu     sync.Mutex
	deny   bool
	claims []string
}

func (c *fakeClaimer) Claim(_ context.Context, name, dedupKey string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claims = append(c.claims, name+"/"+dedupKey)
	return !c.deny
}

func pendingRow(id string) map[string]any {
	return map[string]any{
		"id":       id,
		"medium":   "webhook",
		"target":   "https://hooks.internal.example/notify",
		"body":     `{"k":"v"}`,
		"status":   "pending",
		"attempts": 0,
	}
}

func newTestWorker(eng *fakeEngine, transport Transport, claimer ExecutionClaimer) *Worker {
	w := NewWorker(eng, claimer, nil, map[string]Transport{"webhook": transport, "email": transport}, quietLogger())
	w.cfg = Config{
		Enabled:          true,
		Poll:             time.Second,
		MaxAttempts:      3,
		EmailAllowlist:   []string{"example.com"},
		WebhookAllowlist: []string{"https://hooks.internal.example/"},
		MaxPayloadBytes:  1024,
		HTTPTimeout:      time.Second,
	}
	return w
}

// --- happy path -------------------------------------------------------------

func TestDrainDeliversPendingAndStampsSent(t *testing.T) {
	eng := &fakeEngine{pending: []map[string]any{pendingRow("v1:platform:outboundRequest:r1")}}
	tr := &fakeTransport{}
	w := newTestWorker(eng, tr, nil)

	w.drainOnce(context.Background())

	if tr.count() != 1 {
		t.Fatalf("expected 1 delivery, got %d", tr.count())
	}
	stamps := eng.stamped()
	if len(stamps) != 2 {
		t.Fatalf("expected sending+sent stamps, got %v", stamps)
	}
	if !strings.Contains(stamps[0], `status: "sending"`) {
		t.Fatalf("first stamp must be sending, got %q", stamps[0])
	}
	if !strings.Contains(stamps[1], `status: "sent"`) || !strings.Contains(stamps[1], "sentAt:") {
		t.Fatalf("second stamp must be sent with sentAt, got %q", stamps[1])
	}
}

// --- retry scheduling ---------------------------------------------------------

func TestRetryableFailureStampsRetryingWithBackoff(t *testing.T) {
	eng := &fakeEngine{pending: []map[string]any{pendingRow("v1:platform:outboundRequest:r1")}}
	tr := &fakeTransport{err: errors.New("connect timeout")}
	w := newTestWorker(eng, tr, nil)

	w.drainOnce(context.Background())

	stamps := eng.stamped()
	last := stamps[len(stamps)-1]
	if !strings.Contains(last, `status: "retrying"`) {
		t.Fatalf("retryable failure must stamp retrying, got %q", last)
	}
	if !strings.Contains(last, "attempts: 1") || !strings.Contains(last, "nextAttemptAt:") {
		t.Fatalf("retrying stamp must carry attempts + nextAttemptAt, got %q", last)
	}
}

func TestPermanentFailureStampsFailed(t *testing.T) {
	eng := &fakeEngine{pending: []map[string]any{pendingRow("v1:platform:outboundRequest:r1")}}
	tr := &fakeTransport{err: Permanent(errors.New("410 gone"))}
	w := newTestWorker(eng, tr, nil)

	w.drainOnce(context.Background())

	last := eng.stamped()[len(eng.stamped())-1]
	if !strings.Contains(last, `status: "failed"`) {
		t.Fatalf("permanent failure must stamp failed, got %q", last)
	}
}

func TestAttemptsExhaustedStampsFailed(t *testing.T) {
	row := pendingRow("v1:platform:outboundRequest:r1")
	row["status"] = "retrying"
	row["attempts"] = 2 // MaxAttempts is 3; this attempt is the last
	row["nextAttemptAt"] = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	eng := &fakeEngine{retrying: []map[string]any{row}}
	tr := &fakeTransport{err: errors.New("still down")}
	w := newTestWorker(eng, tr, nil)

	w.drainOnce(context.Background())

	last := eng.stamped()[len(eng.stamped())-1]
	if !strings.Contains(last, `status: "failed"`) || !strings.Contains(last, "attempts: 3") {
		t.Fatalf("exhausted retries must stamp failed with attempts=3, got %q", last)
	}
}

func TestRetryingRowNotDueIsSkipped(t *testing.T) {
	row := pendingRow("v1:platform:outboundRequest:r1")
	row["status"] = "retrying"
	row["attempts"] = 1
	row["nextAttemptAt"] = time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	eng := &fakeEngine{retrying: []map[string]any{row}}
	tr := &fakeTransport{}
	w := newTestWorker(eng, tr, nil)

	w.drainOnce(context.Background())

	if tr.count() != 0 {
		t.Fatal("a retrying row before its nextAttemptAt must not be delivered")
	}
	if len(eng.stamped()) != 0 {
		t.Fatalf("a not-due row must not be stamped, got %v", eng.stamped())
	}
}

func TestRetryingRowWithoutDueTimeIsSkipped(t *testing.T) {
	row := pendingRow("v1:platform:outboundRequest:r1")
	row["status"] = "retrying"
	delete(row, "nextAttemptAt")
	eng := &fakeEngine{retrying: []map[string]any{row}}
	tr := &fakeTransport{}
	w := newTestWorker(eng, tr, nil)

	w.drainOnce(context.Background())

	if tr.count() != 0 {
		t.Fatal("a retrying row with no parseable nextAttemptAt must be left alone")
	}
}

// --- policy (ADR 4.3) ---------------------------------------------------------

func TestAllowlistMissStampsFailedWithoutDelivering(t *testing.T) {
	row := pendingRow("v1:platform:outboundRequest:r1")
	row["target"] = "https://evil.example.net/exfil"
	eng := &fakeEngine{pending: []map[string]any{row}}
	tr := &fakeTransport{}
	w := newTestWorker(eng, tr, nil)

	w.drainOnce(context.Background())

	if tr.count() != 0 {
		t.Fatal("an allowlist miss must never reach the transport")
	}
	last := eng.stamped()[len(eng.stamped())-1]
	if !strings.Contains(last, `status: "failed"`) || !strings.Contains(last, "allowlist") {
		t.Fatalf("allowlist miss must fail fast with an explicit error, got %q", last)
	}
}

func TestDisabledMediumFailsFast(t *testing.T) {
	eng := &fakeEngine{pending: []map[string]any{pendingRow("v1:platform:outboundRequest:r1")}}
	tr := &fakeTransport{}
	w := newTestWorker(eng, tr, nil)
	w.cfg.WebhookAllowlist = nil // medium disabled by deployment config

	w.drainOnce(context.Background())

	if tr.count() != 0 {
		t.Fatal("a disabled medium must never reach the transport")
	}
	last := eng.stamped()[len(eng.stamped())-1]
	if !strings.Contains(last, `status: "failed"`) || !strings.Contains(last, "disabled by deployment config") {
		t.Fatalf("disabled medium must fail loud, got %q", last)
	}
}

func TestPayloadCapFailsFast(t *testing.T) {
	row := pendingRow("v1:platform:outboundRequest:r1")
	row["body"] = strings.Repeat("x", 2048) // cap is 1024
	eng := &fakeEngine{pending: []map[string]any{row}}
	tr := &fakeTransport{}
	w := newTestWorker(eng, tr, nil)

	w.drainOnce(context.Background())

	if tr.count() != 0 {
		t.Fatal("an over-cap payload must never reach the transport")
	}
	if !strings.Contains(eng.stamped()[0], `status: "failed"`) {
		t.Fatalf("over-cap payload must fail, got %v", eng.stamped())
	}
}

// --- cross-replica claim --------------------------------------------------------

func TestClaimDeniedSkipsDelivery(t *testing.T) {
	eng := &fakeEngine{pending: []map[string]any{pendingRow("v1:platform:outboundRequest:r1")}}
	tr := &fakeTransport{}
	claimer := &fakeClaimer{deny: true}
	w := newTestWorker(eng, tr, claimer)

	w.drainOnce(context.Background())

	if tr.count() != 0 {
		t.Fatal("a denied claim must skip delivery (another replica owns it)")
	}
	if len(claimer.claims) != 1 || !strings.Contains(claimer.claims[0], "outboundDelivery/") {
		t.Fatalf("exactly one claim under outboundDelivery expected, got %v", claimer.claims)
	}
}

func TestClaimKeyedPerAttempt(t *testing.T) {
	row := pendingRow("v1:platform:outboundRequest:r1")
	row["attempts"] = 2
	eng := &fakeEngine{pending: []map[string]any{row}}
	claimer := &fakeClaimer{}
	w := newTestWorker(eng, &fakeTransport{}, claimer)

	w.drainOnce(context.Background())

	want := "outboundDelivery/v1:platform:outboundRequest:r1:2"
	if len(claimer.claims) != 1 || claimer.claims[0] != want {
		t.Fatalf("claim must be keyed per (row, attempts): want %q got %v", want, claimer.claims)
	}
}

// --- allowlist matchers ---------------------------------------------------------

func TestEmailAllowed(t *testing.T) {
	domains := []string{"example.com"}
	for target, want := range map[string]bool{
		"ops@example.com":      true,
		"ops@ops.example.com":  true, // subdomain admitted
		"ops@EXAMPLE.COM":      true, // case-insensitive
		"ops@notexample.com":   false,
		"ops@example.com.evil": false,
		"no-at-sign":           false,
		"@example.com":         false,
		"trailing@":            false,
	} {
		if got := emailAllowed(target, domains); got != want {
			t.Fatalf("emailAllowed(%q) = %v, want %v", target, got, want)
		}
	}
	if emailAllowed("ops@example.com", nil) {
		t.Fatal("an empty allowlist must admit nothing")
	}
}

func TestWebhookAllowedRejectsUserinfoTrick(t *testing.T) {
	// "https://host" without a closing slash would prefix-match
	// "https://host@evil.example/x" whose REAL host is evil.example.
	// normalizeWebhookPrefixes closes the authority so it cannot.
	prefixes := normalizeWebhookPrefixes([]string{"https://hooks.internal.example"})
	if webhookAllowed("https://hooks.internal.example@evil.example/x", prefixes) {
		t.Fatal("userinfo trick must not pass the allowlist")
	}
	if !webhookAllowed("https://hooks.internal.example/notify", prefixes) {
		t.Fatal("normalized prefix must still admit real paths under the host")
	}
}

func TestWebhookTransportRefusesUnlistedTarget(t *testing.T) {
	tr := &WebhookTransport{Client: &http.Client{}, Allowlist: []string{"https://hooks.internal.example/"}}
	err := tr.Deliver(context.Background(), Request{Target: "https://evil.example.net/x", Payload: "{}"})
	if err == nil || !IsPermanent(err) {
		t.Fatalf("unlisted target must fail permanently in the transport, got %v", err)
	}
}

func TestWebhookAllowed(t *testing.T) {
	prefixes := []string{"https://hooks.internal.example/", "http://sim.cluster.local:7306/"}
	for target, want := range map[string]bool{
		"https://hooks.internal.example/notify":  true,
		"https://hooks.internal.example":         false, // missing trailing slash of the prefix
		"https://evil.example.net/":              false,
		"http://sim.cluster.local:7306/dispatch": true,  // explicit http:// entry opted in
		"http://hooks.internal.example/notify":   false, // https prefix does not admit plain http
	} {
		if got := webhookAllowed(target, prefixes); got != want {
			t.Fatalf("webhookAllowed(%q) = %v, want %v", target, got, want)
		}
	}
	if webhookAllowed("https://hooks.internal.example/x", nil) {
		t.Fatal("an empty allowlist must admit nothing")
	}
}

// --- backoff ---------------------------------------------------------------------

func TestBackoffScheduleBoundedWithJitter(t *testing.T) {
	for attempt, base := range map[int]time.Duration{
		1: 30 * time.Second,
		2: 2 * time.Minute,
		3: 8 * time.Minute,
		4: 32 * time.Minute,
	} {
		for i := 0; i < 20; i++ {
			d := backoffFor(attempt)
			lo := time.Duration(float64(base) * 0.8)
			hi := time.Duration(float64(base) * 1.2)
			if d < lo || d > hi {
				t.Fatalf("backoffFor(%d) = %v outside [%v, %v]", attempt, d, lo, hi)
			}
		}
	}
	for i := 0; i < 20; i++ {
		if d := backoffFor(10); d > backoffCap {
			t.Fatalf("backoff must cap at %v, got %v", backoffCap, d)
		}
	}
}

// --- webhook transport ---------------------------------------------------------

func TestWebhookTransportPostsPayloadAndHeaders(t *testing.T) {
	var gotBody string
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		gotBody = buf.String()
		gotHeaders = r.Header.Clone()
		rw.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	tr := &WebhookTransport{Client: srv.Client(), Allowlist: normalizeWebhookPrefixes([]string{srv.URL})}
	err := tr.Deliver(context.Background(), Request{
		ID:        "v1:platform:outboundRequest:r1",
		Medium:    "webhook",
		Target:    srv.URL + "/dispatch",
		Payload:   `{"hello":"world"}`,
		DedupeKey: "k-1",
	})
	if err != nil {
		t.Fatalf("2xx must be success, got %v", err)
	}
	if gotBody != `{"hello":"world"}` {
		t.Fatalf("payload must POST verbatim, got %q", gotBody)
	}
	if gotHeaders.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type must be application/json, got %q", gotHeaders.Get("Content-Type"))
	}
	if gotHeaders.Get("X-Memql-Outbound-Id") != "v1:platform:outboundRequest:r1" ||
		gotHeaders.Get("X-Memql-Dedupe-Key") != "k-1" {
		t.Fatalf("dedup headers must ride the request, got %v", gotHeaders)
	}
}

func TestWebhookTransportClassifiesStatuses(t *testing.T) {
	for status, wantPermanent := range map[int]bool{
		http.StatusBadRequest:          true,  // 400 permanent
		http.StatusGone:                true,  // 410 permanent
		http.StatusRequestTimeout:      false, // 408 retryable
		http.StatusTooManyRequests:     false, // 429 retryable
		http.StatusInternalServerError: false, // 500 retryable
		http.StatusBadGateway:          false, // 502 retryable
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
			rw.WriteHeader(status)
		}))
		tr := &WebhookTransport{Client: srv.Client(), Allowlist: normalizeWebhookPrefixes([]string{srv.URL})}
		err := tr.Deliver(context.Background(), Request{Target: srv.URL + "/", Payload: "{}"})
		srv.Close()
		if err == nil {
			t.Fatalf("status %d must be an error", status)
		}
		if IsPermanent(err) != wantPermanent {
			t.Fatalf("status %d: IsPermanent = %v, want %v", status, IsPermanent(err), wantPermanent)
		}
	}
}

// --- config -------------------------------------------------------------------

func TestSplitAllowlist(t *testing.T) {
	got := splitAllowlist(" Example.COM , ,ops.example.org,", true)
	want := fmt.Sprintf("%v", []string{"example.com", "ops.example.org"})
	if fmt.Sprintf("%v", got) != want {
		t.Fatalf("splitAllowlist = %v, want %v", got, want)
	}
}
