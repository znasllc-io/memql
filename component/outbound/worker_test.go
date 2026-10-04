package outbound

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
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
	mu      sync.Mutex
	deny    bool
	claims  []string
	lastTTL time.Duration
}

func (c *fakeClaimer) ClaimWithTTL(_ context.Context, name, dedupKey string, ttl time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claims = append(c.claims, name+"/"+dedupKey)
	c.lastTTL = ttl
	return !c.deny
}

// leaseClaimer models the automation_execution_claims ledger under an
// attempt-scoped lease (memql#2548): a claim is honoured until it is older
// than ttl, after which a peer re-wins it. A shared instance stands in for
// the cross-replica DB ledger, and a mutable clock stands in for wall time,
// so the dead-claimant recovery can be exercised deterministically with the
// worker's fakes (no Postgres).
type leaseClaimer struct {
	mu    sync.Mutex
	held  map[string]time.Time // key -> claimed_at
	clock time.Time
}

func newLeaseClaimer(start time.Time) *leaseClaimer {
	return &leaseClaimer{held: map[string]time.Time{}, clock: start}
}

func (c *leaseClaimer) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clock = c.clock.Add(d)
}

func (c *leaseClaimer) ClaimWithTTL(_ context.Context, name, dedupKey string, ttl time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := name + "/" + dedupKey
	at, held := c.held[key]
	if !held || (ttl > 0 && c.clock.Sub(at) >= ttl) {
		c.held[key] = c.clock // fresh insert or stale-claim takeover
		return true
	}
	return false // a live claim still holds the key
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
		ClaimTTL:         5 * time.Minute,
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

// TestUnconfiguredNodeSkipsRowLeavesPending pins the memql#2540 fix: the
// worker runs on every node, but a node with no allowlist for the row's
// medium is NOT this row's egress owner. It must skip the row entirely --
// no claim, no stamp, no delivery -- leaving it pending so a configured
// peer can win the claim and deliver. Before the fix such a node claimed
// (row, attempt 0), failed the empty-allowlist admit, and stamped the row
// failed out from under the configured peer.
func TestUnconfiguredNodeSkipsRowLeavesPending(t *testing.T) {
	eng := &fakeEngine{pending: []map[string]any{pendingRow("v1:platform:outboundRequest:r1")}}
	tr := &fakeTransport{}
	claimer := &fakeClaimer{}
	w := newTestWorker(eng, tr, claimer)
	w.cfg.WebhookAllowlist = nil // this node carries no webhook allowlist

	w.drainOnce(context.Background())

	if tr.count() != 0 {
		t.Fatal("an unconfigured node must never reach the transport")
	}
	if len(claimer.claims) != 0 {
		t.Fatalf("an unconfigured node must not claim the row (leaves it for a configured peer), got %v", claimer.claims)
	}
	if len(eng.stamped()) != 0 {
		t.Fatalf("an unconfigured node must not stamp the row (leaves it pending), got %v", eng.stamped())
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

// TestConfiguredNodeClaimsAndDelivers is the memql#2540 counterpart to the
// unconfigured-skip case: a node WITH the medium's allowlist is the egress
// owner. It must pass the pre-claim gate, claim the row, and deliver.
func TestConfiguredNodeClaimsAndDelivers(t *testing.T) {
	eng := &fakeEngine{pending: []map[string]any{pendingRow("v1:platform:outboundRequest:r1")}}
	tr := &fakeTransport{}
	claimer := &fakeClaimer{}
	w := newTestWorker(eng, tr, claimer) // newTestWorker sets a webhook allowlist

	w.drainOnce(context.Background())

	if len(claimer.claims) != 1 {
		t.Fatalf("a configured node must claim the row, got %v", claimer.claims)
	}
	if tr.count() != 1 {
		t.Fatalf("a configured node must deliver the row, got %d deliveries", tr.count())
	}
}

// TestConfiguredNodeStampsFailedOnTargetMiss confirms the target-allowlist
// check stays POST-claim (memql#2540): a node configured for the medium but
// handed a row whose target is outside the allowlist claims the row and
// fails it loudly, rather than skipping it silently.
func TestConfiguredNodeStampsFailedOnTargetMiss(t *testing.T) {
	row := pendingRow("v1:platform:outboundRequest:r1")
	row["target"] = "https://evil.example.net/exfil"
	eng := &fakeEngine{pending: []map[string]any{row}}
	tr := &fakeTransport{}
	claimer := &fakeClaimer{}
	w := newTestWorker(eng, tr, claimer) // configured for webhook

	w.drainOnce(context.Background())

	if len(claimer.claims) != 1 {
		t.Fatalf("a configured node must claim before failing a bad target, got %v", claimer.claims)
	}
	if tr.count() != 0 {
		t.Fatal("a target-allowlist miss must never reach the transport")
	}
	last := eng.stamped()[len(eng.stamped())-1]
	if !strings.Contains(last, `status: "failed"`) || !strings.Contains(last, "allowlist") {
		t.Fatalf("target miss must fail loud post-claim, got %q", last)
	}
}

// TestClaimCarriesAttemptScopedTTL pins the memql#2548 wiring: the worker
// claims through the attempt-scoped-lease variant, passing cfg.ClaimTTL so a
// claim orphaned by a dead claimant expires rather than wedging the row until
// the guard's 1h retention prune.
func TestClaimCarriesAttemptScopedTTL(t *testing.T) {
	eng := &fakeEngine{pending: []map[string]any{pendingRow("v1:platform:outboundRequest:r1")}}
	claimer := &fakeClaimer{}
	w := newTestWorker(eng, &fakeTransport{}, claimer)
	w.cfg.ClaimTTL = 90 * time.Second

	w.drainOnce(context.Background())

	if claimer.lastTTL != 90*time.Second {
		t.Fatalf("worker must claim with the configured attempt-scoped TTL, got %v", claimer.lastTTL)
	}
}

// TestDeadClaimantRowRecoveredAfterLeaseExpiry is the memql#2548 regression:
// a replica wins the claim for (row, attempt 0) and dies before stamping any
// status, leaving the row pending with a persisted claim. Before the lease,
// every peer re-claimed-and-lost that key until the guard's 1h retention
// prune, wedging the row. With the attempt-scoped lease the persisted claim
// expires after cfg.ClaimTTL, so the next drain pass on a peer re-claims the
// same attempt and delivers the still-pending row.
func TestDeadClaimantRowRecoveredAfterLeaseExpiry(t *testing.T) {
	ledger := newLeaseClaimer(time.Unix(1000, 0))

	// A dead claimant: it won (row, attempt 0) but stamped nothing. Model it
	// by occupying the claim in the shared ledger without touching the engine
	// row, which therefore stays pending.
	if !ledger.ClaimWithTTL(context.Background(), "outboundDelivery", "v1:platform:outboundRequest:r1:0", 5*time.Minute) {
		t.Fatal("precondition: the dead claimant must win the initial claim")
	}

	// A peer drains while the claim is still within its lease: it re-claims,
	// loses, and leaves the row pending -- the pre-fix wedge window.
	eng := &fakeEngine{pending: []map[string]any{pendingRow("v1:platform:outboundRequest:r1")}}
	tr := &fakeTransport{}
	w := newTestWorker(eng, tr, ledger)
	w.cfg.ClaimTTL = 5 * time.Minute

	w.drainOnce(context.Background())
	if tr.count() != 0 {
		t.Fatal("within the lease the peer must lose the claim and not deliver")
	}
	if len(eng.stamped()) != 0 {
		t.Fatalf("within the lease the row must stay pending (no stamp), got %v", eng.stamped())
	}

	// The lease elapses (the dead claimant never returns). The next drain pass
	// re-wins the orphaned claim and recovers the row.
	ledger.advance(6 * time.Minute)
	w.drainOnce(context.Background())

	if tr.count() != 1 {
		t.Fatalf("after the lease expires the peer must re-claim and deliver, got %d deliveries", tr.count())
	}
	stamps := eng.stamped()
	if len(stamps) != 2 || !strings.Contains(stamps[0], `status: "sending"`) || !strings.Contains(stamps[1], `status: "sent"`) {
		t.Fatalf("recovery must stamp sending then sent, got %v", stamps)
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

func TestClaimTTLConfigDefaultsAndOverrides(t *testing.T) {
	if got := LoadConfig().ClaimTTL; got != defaultClaimTTLSeconds*time.Second {
		t.Fatalf("default ClaimTTL = %v, want %v", got, defaultClaimTTLSeconds*time.Second)
	}
	t.Setenv("MEMQL_OUTBOUND_CLAIM_TTL_SECONDS", "45")
	if got := LoadConfig().ClaimTTL; got != 45*time.Second {
		t.Fatalf("overridden ClaimTTL = %v, want 45s", got)
	}
}

// --- secret targets (memql#5480) -----------------------------------------------
//
// A Discord webhook carries its token in its URL's path, so for such a target
// the URL IS the credential. A row names the v1:platform:globalSecret holding
// it (targetSecret) and carries only the descriptor secret:<NAME> in target;
// the worker resolves the value at send time and holds it in memory for one
// attempt. Nothing it stamps and nothing it logs may carry it, which is why
// every test below reads the log as well as the stamps.

const (
	discordWebhookPrefix = "https://discord.com/api/webhooks/"
	discordSecretName    = "DISCORD_X"
)

// discordToken is the token half of a stand-in webhook URL. Distinctive, so an
// absence assertion cannot pass by accident (a token spelled "tok" is also a
// substring of "token"), and assembled rather than written out because the
// repository's secret scanner reads test files too.
var discordToken = "Zq" + strings.Repeat("x7", 30)

// secretRow is a pending webhook row as stageOutboundRequestToSecret stages it.
func secretRow(id string) map[string]any {
	row := pendingRow(id)
	row["target"] = "secret:" + discordSecretName
	row["targetSecret"] = discordSecretName
	row["body"] = `{"content":"run 42 passed"}`
	return row
}

// fakeSecrets stands in for the globalSecret resolver: it answers one value or
// one error and records every name it was asked for.
type fakeSecrets struct {
	mu    sync.Mutex
	value string
	err   error
	asked []string
}

func (s *fakeSecrets) Resolve(_ context.Context, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, name)
	return s.value, s.err
}

func (s *fakeSecrets) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

// newSecretTargetWorker wires a worker whose webhook allowlist admits Discord's
// webhook prefix only, and whose logger records every level into the returned
// buffer. secrets nil leaves the resolver unwired, as on a node nothing set it on.
func newSecretTargetWorker(eng *fakeEngine, transport Transport, secrets *fakeSecrets) (*Worker, *bytes.Buffer) {
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w := NewWorker(eng, nil, nil, map[string]Transport{"webhook": transport, "email": transport}, logger)
	w.cfg = Config{
		Enabled:          true,
		Poll:             time.Second,
		MaxAttempts:      3,
		EmailAllowlist:   []string{"example.com"},
		WebhookAllowlist: []string{discordWebhookPrefix},
		MaxPayloadBytes:  1024,
		HTTPTimeout:      time.Second,
		ClaimTTL:         5 * time.Minute,
	}
	if secrets != nil {
		w.Secrets = secrets.Resolve
	}
	return w, logs
}

// webhookStandIn answers for a webhook host the test does not own. Its client
// dials the httptest TLS server whatever host a URL names and verifies the
// server as example.com, a name httptest's certificate carries, so the real
// transport builds and sends the real request for the real URL and the bytes
// land here. Nothing leaves the machine.
func webhookStandIn(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	base, ok := srv.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httptest's client transport is %T, want *http.Transport", srv.Client().Transport)
	}
	tr := base.Clone()
	addr := srv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	tr.TLSClientConfig.ServerName = "example.com"
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}

// refusingClient dials a loopback port nothing listens on, whatever host a URL
// names: a real connection refusal from inside net/http, arriving wrapped in
// the *url.Error that names the request URL, exactly as a production dial
// failure does.
func refusingClient(t *testing.T) *http.Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		},
	}
}

// assertNoWebhookURL fails when text carries the resolved URL, its token, or
// the path that holds the token.
func assertNoWebhookURL(t *testing.T, where, text, resolved string) {
	t.Helper()
	for _, leak := range []string{resolved, discordToken, "/api/webhooks/"} {
		if strings.Contains(text, leak) {
			t.Errorf("%s carries %q, which is the webhook URL the secret holds or a piece of it. "+
				"That URL is a credential: it must reach the transport and nothing else.\n%s", where, leak, text)
		}
	}
}

// TestSecretTargetDeliversToTheResolvedURL: a row naming a globalSecret is
// POSTed to the URL the secret holds, through the allowlist and the transport
// every webhook row goes through, and the URL is in nothing the worker writes.
func TestSecretTargetDeliversToTheResolvedURL(t *testing.T) {
	const resolved = "https://discord.com/api/webhooks/1/tok"
	var mu sync.Mutex
	var gotMethod, gotHost, gotPath, gotBody string
	client := webhookStandIn(t, func(rw http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		mu.Lock()
		gotMethod, gotHost, gotPath, gotBody = r.Method, r.Host, r.URL.Path, buf.String()
		mu.Unlock()
		rw.WriteHeader(http.StatusNoContent)
	})
	eng := &fakeEngine{pending: []map[string]any{secretRow("v1:platform:outboundRequest:s1")}}
	secrets := &fakeSecrets{value: resolved}
	w, logs := newSecretTargetWorker(eng, &WebhookTransport{Client: client, Allowlist: []string{discordWebhookPrefix}}, secrets)

	w.drainOnce(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if gotMethod != http.MethodPost || gotHost != "discord.com" || gotPath != "/api/webhooks/1/tok" {
		t.Fatalf("the POST must go to the resolved URL; the stand-in saw %s %s%s (stamps: %v)", gotMethod, gotHost, gotPath, eng.stamped())
	}
	if gotBody != `{"content":"run 42 passed"}` {
		t.Fatalf("the row's body must POST verbatim, got %q", gotBody)
	}
	if names := secrets.names(); len(names) != 1 || names[0] != discordSecretName {
		t.Fatalf("the worker must resolve exactly the secret the row names, once; asked for %v", names)
	}
	stamps := eng.stamped()
	if len(stamps) != 2 || !strings.Contains(stamps[0], `status: "sending"`) || !strings.Contains(stamps[1], `status: "sent"`) {
		t.Fatalf("a delivered secret target stamps sending then sent, got %v", stamps)
	}
	for _, s := range stamps {
		if strings.Contains(s, "webhooks/1/tok") || strings.Contains(s, "discord.com") {
			t.Errorf("a stamp carries the resolved URL: %s", s)
		}
	}
	if strings.Contains(logs.String(), "webhooks/1/tok") {
		t.Errorf("the log carries the resolved URL:\n%s", logs.String())
	}
}

// TestSecretTargetThatDoesNotResolveFailsNamingTheSecret: a secret that cannot
// be read is not a transient condition the backoff can wait out. The row fails
// once, and its lastError names the secret, which is what an operator fixes.
func TestSecretTargetThatDoesNotResolveFailsNamingTheSecret(t *testing.T) {
	for _, tc := range []struct {
		name    string
		secrets *fakeSecrets // nil: no resolver is wired on this node
	}{
		{"the resolver errors", &fakeSecrets{err: errors.New(`secret "DISCORD_X" not found`)}},
		{"the secret is empty", &fakeSecrets{value: ""}},
		{"the secret is blank", &fakeSecrets{value: " \n"}},
		{"no resolver is wired", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &fakeEngine{pending: []map[string]any{secretRow("v1:platform:outboundRequest:s1")}}
			tr := &fakeTransport{}
			w, _ := newSecretTargetWorker(eng, tr, tc.secrets)

			w.drainOnce(context.Background())

			if tr.count() != 0 {
				t.Fatal("a secret that did not resolve must never reach the transport")
			}
			stamps := eng.stamped()
			if len(stamps) != 1 || !strings.Contains(stamps[0], `status: "failed"`) {
				t.Fatalf("an unresolvable secret is a permanent failure, stamped once as failed; got %v", stamps)
			}
			lastError := stringArg(t, stamps[0], "lastError")
			if !strings.Contains(lastError, "webhook: target secret DISCORD_X did not resolve") {
				t.Fatalf("lastError must name the secret that did not resolve, got %q", lastError)
			}
			if strings.Contains(lastError, "https://") {
				t.Fatalf("lastError must carry no URL, got %q", lastError)
			}
		})
	}
}

// TestSecretTargetOutsideTheAllowlistFailsWithoutNamingIt: the allowlist judges
// the resolved URL, and its refusal says so without quoting it. A plain row's
// refusal quotes its target; that quote here would publish the credential.
func TestSecretTargetOutsideTheAllowlistFailsWithoutNamingIt(t *testing.T) {
	resolved := "https://evil.example.net/api/webhooks/1/" + discordToken
	eng := &fakeEngine{pending: []map[string]any{secretRow("v1:platform:outboundRequest:s1")}}
	tr := &fakeTransport{}
	w, logs := newSecretTargetWorker(eng, tr, &fakeSecrets{value: resolved})

	w.drainOnce(context.Background())

	if tr.count() != 0 {
		t.Fatal("a resolved URL outside the allowlist must never reach the transport")
	}
	stamps := eng.stamped()
	if len(stamps) != 1 || !strings.Contains(stamps[0], `status: "failed"`) {
		t.Fatalf("an allowlist miss is a permanent failure, stamped once as failed; got %v", stamps)
	}
	if got := stringArg(t, stamps[0], "lastError"); got != "webhook: target not in allowlist" {
		t.Fatalf("lastError = %q, want %q", got, "webhook: target not in allowlist")
	}
	assertNoWebhookURL(t, "the log", logs.String(), resolved)
}

// TestSecretTargetDialFailureKeepsTheURLOutOfLastError: net/http wraps a
// transport failure in a *url.Error whose message embeds the request URL.
// Stamped as it arrives, a refused connection would write the token into
// lastError and the log. The cause has to survive, or lastError stops
// explaining anything.
func TestSecretTargetDialFailureKeepsTheURLOutOfLastError(t *testing.T) {
	resolved := discordWebhookPrefix + "123/" + discordToken
	eng := &fakeEngine{pending: []map[string]any{secretRow("v1:platform:outboundRequest:s1")}}
	transport := &WebhookTransport{Client: refusingClient(t), Allowlist: []string{discordWebhookPrefix}}
	w, logs := newSecretTargetWorker(eng, transport, &fakeSecrets{value: resolved})

	w.drainOnce(context.Background())

	stamps := eng.stamped()
	if len(stamps) == 0 {
		t.Fatal("the worker stamped nothing")
	}
	last := stamps[len(stamps)-1]
	if !strings.Contains(last, `status: "retrying"`) {
		t.Fatalf("a refused connection is retryable, got %q", last)
	}
	lastError := stringArg(t, last, "lastError")
	assertNoWebhookURL(t, "lastError", lastError, resolved)
	if !strings.Contains(lastError, "connection refused") {
		t.Fatalf("the redaction must keep the cause, got %q", lastError)
	}
	assertNoWebhookURL(t, "the log", logs.String(), resolved)
}

// TestSecretTargetThatCannotBeBuiltKeepsTheURLOutOfLastError: the second place
// net/http hands back the URL. A control byte in the query makes building the
// request fail with a parse *url.Error, and that error is stamped too.
func TestSecretTargetThatCannotBeBuiltKeepsTheURLOutOfLastError(t *testing.T) {
	resolved := discordWebhookPrefix + "123/" + discordToken + "?wait=true\x7f"
	eng := &fakeEngine{pending: []map[string]any{secretRow("v1:platform:outboundRequest:s1")}}
	transport := &WebhookTransport{Client: refusingClient(t), Allowlist: []string{discordWebhookPrefix}}
	w, logs := newSecretTargetWorker(eng, transport, &fakeSecrets{value: resolved})

	w.drainOnce(context.Background())

	stamps := eng.stamped()
	if len(stamps) == 0 {
		t.Fatal("the worker stamped nothing")
	}
	last := stamps[len(stamps)-1]
	if !strings.Contains(last, `status: "failed"`) {
		t.Fatalf("a request that cannot be built is a permanent failure, got %q", last)
	}
	lastError := stringArg(t, last, "lastError")
	assertNoWebhookURL(t, "lastError", lastError, resolved)
	if !strings.Contains(lastError, "invalid control character") {
		t.Fatalf("the redaction must keep the cause, got %q", lastError)
	}
	assertNoWebhookURL(t, "the log", logs.String(), resolved)
}

// TestSecretTargetOnAnEmailRowFailsWithoutResolving: a secret target names a
// webhook URL, so a row pairing one with medium "email" is malformed. It fails
// loudly, and the secret is never read for it.
func TestSecretTargetOnAnEmailRowFailsWithoutResolving(t *testing.T) {
	row := secretRow("v1:platform:outboundRequest:s1")
	row["medium"] = "email"
	row["target"] = "ops@example.com"
	eng := &fakeEngine{pending: []map[string]any{row}}
	tr := &fakeTransport{}
	secrets := &fakeSecrets{value: discordWebhookPrefix + "123/" + discordToken}
	w, _ := newSecretTargetWorker(eng, tr, secrets)

	w.drainOnce(context.Background())

	if tr.count() != 0 {
		t.Fatal("an email row carrying a secret target must never reach a transport")
	}
	if names := secrets.names(); len(names) != 0 {
		t.Fatalf("the secret must not be read for a row that cannot use it; asked for %v", names)
	}
	stamps := eng.stamped()
	if len(stamps) != 1 || !strings.Contains(stamps[0], `status: "failed"`) {
		t.Fatalf("a secret target on an email row is a permanent failure, stamped once as failed; got %v", stamps)
	}
	if lastError := stringArg(t, stamps[0], "lastError"); !strings.Contains(lastError, discordSecretName) {
		t.Fatalf("lastError must name the secret the row carries, got %q", lastError)
	}
}

// TestPlainWebhookRowNeverConsultsTheSecretResolver: a row with no targetSecret
// is delivered to its own target exactly as before the field existed, and the
// resolver is not asked anything.
func TestPlainWebhookRowNeverConsultsTheSecretResolver(t *testing.T) {
	eng := &fakeEngine{pending: []map[string]any{pendingRow("v1:platform:outboundRequest:r1")}}
	tr := &fakeTransport{}
	secrets := &fakeSecrets{value: discordWebhookPrefix + "123/" + discordToken}
	w := newTestWorker(eng, tr, nil)
	w.Secrets = secrets.Resolve

	w.drainOnce(context.Background())

	if names := secrets.names(); len(names) != 0 {
		t.Fatalf("a plain row must not consult the secret resolver; asked for %v", names)
	}
	if tr.count() != 1 || tr.delivered[0].Target != "https://hooks.internal.example/notify" {
		t.Fatalf("a plain row must deliver to its own target, got %+v", tr.delivered)
	}
}
