package clustere2e

// magic_link_replica_test.go -- the cross-replica gate for the device-bound
// magic-link flow (memql#4302, design section 9 item 12).
//
// # WHY THIS FILE CARRIES NO BUILD TAG
//
// The rest of this package is `//go:build clustere2e` and needs a live
// 2-replica k3d cluster. That is the right home for an end-to-end assertion,
// and it is SKIPPED everywhere a cluster is not running -- including on a
// developer's machine and on every CI lane that does not boot one. A gate
// skipped by default cannot be the thing standing between this flow and the
// mesh bug it was written to prevent.
//
// So the hop is gated here instead, deterministically and with no cluster, by
// running the flow across TWO INDEPENDENT web.Server instances that share
// nothing but a store. Two Servers is what two identity pods are: separate
// process memory, one database. If any part of the flow ever starts caching
// the binding, the approval or the request in memory, the second Server will
// not see what the first wrote and this test fails.
//
// # HOW TO CONFIRM IT IS LOAD-BEARING
//
// Make the flow remember anything locally -- a map of requestId to nonce on
// the Server, say -- and populate it only on the Server that issues. The
// approve on replica A then leaves replica B's poll answering `pending`
// forever, and TestApproveOnOneReplicaFinishOnTheOther fails at the poll. If
// it passes with a per-Server cache in place, it is measuring nothing.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/magiclink"
	identityweb "github.com/znasllc-io/memql/component/identity/web"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// sharedRow is the one piece of state two replicas have in common: a row in
// Postgres. Guarded by a mutex because two httptest servers in one process
// can touch it concurrently, exactly as two pods can.
type sharedRow struct {
	mu  sync.Mutex
	row identity.MagicLinkRow
}

func (s *sharedRow) get() identity.MagicLinkRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.row
}

func (s *sharedRow) approve(ip, ua string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.row.ApprovedAt.IsZero() || !s.row.ConsumedAt.IsZero() {
		return false
	}
	s.row.ApprovedAt = time.Now().UTC()
	s.row.ApprovedFromIP = ip
	s.row.ApprovedUserAgent = ua
	return true
}

func (s *sharedRow) consume() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.row.ConsumedAt.IsZero() {
		return false
	}
	s.row.ConsumedAt = time.Now().UTC()
	return true
}

// replicaVerifier is the flow's read/finish half over the shared row. It
// holds NO per-instance state, which is the property under test: every
// replica constructs one of these and they must be interchangeable.
type replicaVerifier struct {
	shared   *sharedRow
	token    string
	finishes *int32
	mu       *sync.Mutex
}

func (v *replicaVerifier) Inspect(_ context.Context, plainToken, _, _ string) (*identity.MagicLinkRow, error) {
	if strings.TrimSpace(plainToken) != v.token {
		return nil, magiclink.ErrInvalidToken
	}
	row := v.shared.get()
	if !row.ConsumedAt.IsZero() {
		return nil, magiclink.ErrTokenAlreadyUsed
	}
	return &row, nil
}

func (v *replicaVerifier) Finish(_ context.Context, _ magiclink.FinishInput) (*magiclink.VerifyResult, error) {
	if !v.shared.consume() {
		return nil, magiclink.ErrTokenAlreadyUsed
	}
	v.mu.Lock()
	*v.finishes++
	v.mu.Unlock()
	return &magiclink.VerifyResult{UserId: "u1", Email: "team@acme.test", AdminSession: true}, nil
}

// replicaEngine is the DATABASE both replicas talk to. Each replica gets its
// own identity.Store over this one engine, which is exactly the production
// shape: two pods, two Store values, one Postgres.
type replicaEngine struct{ shared *sharedRow }

func (e *replicaEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	switch {
	case strings.HasPrefix(q, "query magicLinkRequestById("):
		row := e.shared.get()
		fields := map[string]*structpb.Value{
			"id":          structpb.NewStringValue(row.ID),
			"email":       structpb.NewStringValue(row.Email),
			"bindingHash": structpb.NewStringValue(row.BindingHash),
		}
		if !row.ApprovedAt.IsZero() {
			fields["approvedAt"] = structpb.NewStringValue(row.ApprovedAt.Format(time.RFC3339Nano))
			fields["approvedFromIP"] = structpb.NewStringValue(row.ApprovedFromIP)
			fields["approvedUserAgent"] = structpb.NewStringValue(row.ApprovedUserAgent)
		}
		if !row.ConsumedAt.IsZero() {
			fields["consumedAt"] = structpb.NewStringValue(row.ConsumedAt.Format(time.RFC3339Nano))
		}
		if !row.ExpiresAt.IsZero() {
			fields["expiresAt"] = structpb.NewStringValue(row.ExpiresAt.Format(time.RFC3339Nano))
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{
			Nodes: []*memqlv1.MemoryNode{{Id: row.ID, Payload: &structpb.Struct{Fields: fields}}},
		}}, nil

	case strings.HasPrefix(q, "mutation approveMagicLinkRequest("):
		// The store has already re-read the row and decided; the engine's job
		// is the write. Values are pinned rather than parsed out of the
		// rendered call -- the single-replica tests cover the rendering.
		e.shared.approve("198.51.100.7", "ClickerBrowser/1.0")
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
	// Everything else (the post-login landing's cluster-settings read) answers
	// empty, which exercises the fallback.
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

// silentAudit swallows events; the single-replica tests in
// component/identity/web already assert on them.
type silentAudit struct{}

func (silentAudit) Log(context.Context, identity.AuditEvent) {}

func newReplica(t *testing.T, shared *sharedRow, finishes *int32, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	cfg := identity.Config{
		Enabled:      true,
		BaseURL:      "https://identity.test",
		JWTAudience:  "memql",
		MagicLinkTTL: 10 * time.Minute,
	}
	srv, err := identityweb.NewServer(cfg, slog.Default(), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// A SEPARATE Store per replica, over the one engine. Two pods, one
	// database -- and nothing else shared.
	srv.Store = &identity.Store{Engine: &replicaEngine{shared: shared}, Logger: slog.Default()}
	srv.SetMagicLinkFlow(
		&replicaVerifier{shared: shared, token: "plain-token", finishes: finishes, mu: mu},
		func(w http.ResponseWriter, _ *http.Request, _ identityweb.BrowserSessionInput) error {
			http.SetCookie(w, &http.Cookie{Name: "memql_admin", Value: "jwt", Path: "/"})
			return nil
		},
		silentAudit{},
	)
	mux := http.NewServeMux()
	srv.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// TestApproveOnOneReplicaFinishOnTheOther is the acceptance criterion.
func TestApproveOnOneReplicaFinishOnTheOther(t *testing.T) {
	shared := &sharedRow{row: identity.MagicLinkRow{
		ID:          "ml-req-1",
		Email:       "team@acme.test",
		BindingHash: magiclink.HashBindingNonce("the-nonce"),
		ExpiresAt:   time.Now().UTC().Add(10 * time.Minute),
	}}
	var finishes int32
	var mu sync.Mutex

	replicaA := newReplica(t, shared, &finishes, &mu)
	replicaB := newReplica(t, shared, &finishes, &mu)

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// --- B clicks on REPLICA A, with no binding cookie ---
	form := url.Values{"ml": {"plain-token"}}
	req, _ := http.NewRequest(http.MethodPost, replicaA.URL+"/auth/landing", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("approve on replica A: %v", err)
	}
	resp.Body.Close()
	if got := shared.get(); got.ApprovedAt.IsZero() {
		t.Fatal("the approval did not land on the shared row")
	}

	// --- A polls on REPLICA B, holding the binding cookie ---
	pollReq, _ := http.NewRequest(http.MethodGet, replicaB.URL+"/auth/magic-link/status?request=ml-req-1", nil)
	pollReq.AddCookie(&http.Cookie{Name: "memql_ml", Value: "the-nonce"})
	pollResp, err := noRedirect.Do(pollReq)
	if err != nil {
		t.Fatalf("poll on replica B: %v", err)
	}
	defer pollResp.Body.Close()
	if pollResp.StatusCode != http.StatusOK {
		t.Fatalf("poll on replica B returned %d, want 200", pollResp.StatusCode)
	}
	var state struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(pollResp.Body).Decode(&state); err != nil {
		t.Fatalf("decode poll reply: %v", err)
	}
	if state.State != "approved" {
		t.Fatalf("replica B reported %q, want approved.\n\n"+
			"Replica B never saw the request being issued and never saw the click. It answers "+
			"correctly only because every fact the flow needs is on the ROW: the approval, and the "+
			"digest of the cookie the caller is presenting. Anything cached in a replica's memory "+
			"breaks exactly here.", state.State)
	}

	// --- A finishes on REPLICA B ---
	//
	// The finish carries a CSRF token, exactly as the form on /check-email
	// does. Worth doing in full here rather than exempting the path: the CSRF
	// token is a DOUBLE-SUBMIT COOKIE, so it needs no shared secret between
	// replicas -- the check compares a cookie against a form field, both of
	// which the browser holds. That is why the flow can be served by whichever
	// pod the load balancer picks, and asserting it is part of the point.
	csrf := mintCSRF(t, noRedirect, replicaB.URL)

	finishForm := url.Values{"request": {"ml-req-1"}, identityweb.CSRFFormField: {csrf}}
	finishReq, _ := http.NewRequest(http.MethodPost, replicaB.URL+"/auth/magic-link/finish", strings.NewReader(finishForm.Encode()))
	finishReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	finishReq.AddCookie(&http.Cookie{Name: "memql_ml", Value: "the-nonce"})
	finishReq.AddCookie(&http.Cookie{Name: identityweb.CSRFCookieName, Value: csrf})
	finishResp, err := noRedirect.Do(finishReq)
	if err != nil {
		t.Fatalf("finish on replica B: %v", err)
	}
	defer finishResp.Body.Close()

	mu.Lock()
	got := finishes
	mu.Unlock()
	if got != 1 {
		t.Fatalf("finish ran %d time(s) across the two replicas, want 1", got)
	}
	if finishResp.StatusCode != http.StatusSeeOther {
		t.Errorf("finish on replica B returned %d, want 303", finishResp.StatusCode)
	}
	var admin bool
	for _, c := range finishResp.Cookies() {
		if c.Name == "memql_admin" && c.Value != "" {
			admin = true
		}
	}
	if !admin {
		t.Error("the session did not land on the replica that served the finish")
	}
}

// TestPollOnAnotherReplicaStillRequiresTheCookie pins that the cross-replica
// answer above is not simply "any caller gets the state".
//
// Without this, the test above would pass just as well against a status
// endpoint that had no gate at all -- and the gate is the only thing keeping
// the request id, which is rendered on a page, from being enough to watch
// somebody else's sign-in.
func TestPollOnAnotherReplicaStillRequiresTheCookie(t *testing.T) {
	shared := &sharedRow{row: identity.MagicLinkRow{
		ID:          "ml-req-2",
		Email:       "team@acme.test",
		BindingHash: magiclink.HashBindingNonce("the-nonce"),
		ExpiresAt:   time.Now().UTC().Add(10 * time.Minute),
	}}
	var finishes int32
	var mu sync.Mutex
	replica := newReplica(t, shared, &finishes, &mu)

	resp, err := http.Get(replica.URL + "/auth/magic-link/status?request=ml-req-2")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a replica answered a poll from a caller with no binding cookie")
	}
}

// mintCSRF does what a browser does before it can post: fetch a page and keep
// the double-submit cookie the middleware sets on the way out.
func mintCSRF(t *testing.T, client *http.Client, base string) string {
	t.Helper()
	resp, err := client.Get(base + "/check-email")
	if err != nil {
		t.Fatalf("mint csrf: %v", err)
	}
	defer resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == identityweb.CSRFCookieName && c.Value != "" {
			return c.Value
		}
	}
	t.Fatalf("no %s cookie was minted by a GET; the finish form could never be submitted",
		identityweb.CSRFCookieName)
	return ""
}
