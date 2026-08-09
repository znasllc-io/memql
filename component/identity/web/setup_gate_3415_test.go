package web

// Handler-level regression test for the /setup ownership-wizard gate
// (memql#3415).
//
// The incident: a stray write blanked `v1:identity:clusterSettings.
// bootstrappedAt` on a live cluster. `CountUsers` had been deliberately
// reduced to `IsClusterBootstrapped` -- "so out-of-band user rows can't trip
// it" (app/integrations_identity.go) -- so that one blank field was the ONLY
// thing standing between the internet and the wizard that mints the cluster
// owner. `GET /setup` answered 200 on a cluster with an owner and hundreds of
// users.
//
// The original reasoning is sound as far as it goes: a stray USER row must not
// be able to seal /setup on a genuinely fresh cluster. But dropping the second
// signal entirely made the surface hinge on a single field, and this is what
// that cost. The fix restores the cross-check WITHOUT reintroducing a raw
// user-count: `ClusterClaimed` is wired to `Store.HasOwnerUser`, i.e. "an
// active user holding the cluster-OWNER role exists". An owner user is
// definitional proof the cluster was claimed (the same signal the auto-
// bootstrap claim-email guard already trusts, memql#1864) and is not something
// a stray row produces.
//
// Both signals now seal the wizard, and so does not being able to READ either
// one: the wizard mints the cluster owner, so "cannot prove this cluster is
// unclaimed" must refuse rather than serve.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
)

// newSetupGateTestServer builds the minimum *Server the /setup handlers need.
// No Store, no engine: the two gate signals are injected as funcs, which is
// exactly how the wiring layer supplies them.
func newSetupGateTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := NewServer(identity.Config{BaseURL: "http://localhost:8080"}, slog.Default(), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

func setupPostForm() *strings.Reader {
	form := url.Values{
		"domain":      {"example.com"},
		"owner_email": {"attacker@example.com"},
	}
	return strings.NewReader(form.Encode())
}

// TestSetupGate3415_ClaimedClusterSealsSetupEvenWhenBootstrappedAtIsBlank is
// the incident, at the handler. bootstrappedAt is blank (CountUsers reports 0,
// exactly as it did on the affected cluster) but an owner user exists. Before
// the fix both handlers answered 200 / persisted; after it they 404.
func TestSetupGate3415_ClaimedClusterSealsSetupEvenWhenBootstrappedAtIsBlank(t *testing.T) {
	s := newSetupGateTestServer(t)
	// Signal 1 (bootstrappedAt) says "not bootstrapped" -- the clobbered state.
	s.CountUsers = func(_ context.Context) (int, error) { return 0, nil }
	// Signal 2 says the cluster HAS an owner. It is claimed.
	s.ClusterClaimed = func(_ context.Context) (bool, error) { return true, nil }

	rec := httptest.NewRecorder()
	s.handleSetupGet(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /setup on a claimed cluster: status = %d, want 404 (memql#3415)", rec.Code)
	}

	persisted := false
	s.PersistClusterSettings = func(_ context.Context, _ ClusterSettingsInput) error {
		persisted = true
		return nil
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/setup", setupPostForm())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleSetupPost(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /setup on a claimed cluster: status = %d, want 404 (memql#3415)", rec.Code)
	}
	if persisted {
		t.Error("POST /setup on a claimed cluster must not persist cluster settings (memql#3415)")
	}
}

// TestSetupGate3415_UnreadableSignalSealsSetup pins the fail-closed direction.
// The handlers used to swallow a read error (`if n, err := CountUsers(); err ==
// nil && n > 0`) and serve the wizard, so a DB hiccup opened the ownership
// surface just as effectively as a blanked field. "Cannot determine" must
// refuse.
func TestSetupGate3415_UnreadableSignalSealsSetup(t *testing.T) {
	boom := errors.New("db unreachable")

	t.Run("bootstrap signal errors", func(t *testing.T) {
		s := newSetupGateTestServer(t)
		s.CountUsers = func(_ context.Context) (int, error) { return 0, boom }
		s.ClusterClaimed = func(_ context.Context) (bool, error) { return false, nil }
		rec := httptest.NewRecorder()
		s.handleSetupGet(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /setup with an unreadable bootstrap signal: status = %d, want 404 (memql#3415)", rec.Code)
		}
	})

	t.Run("claim signal errors", func(t *testing.T) {
		s := newSetupGateTestServer(t)
		s.CountUsers = func(_ context.Context) (int, error) { return 0, nil }
		s.ClusterClaimed = func(_ context.Context) (bool, error) { return false, boom }
		rec := httptest.NewRecorder()
		s.handleSetupGet(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /setup with an unreadable claim signal: status = %d, want 404 (memql#3415)", rec.Code)
		}
	})

	t.Run("claim signal unwired", func(t *testing.T) {
		// A binary that forgets to wire the second signal must not silently
		// fall back to the single-signal behaviour this issue is about.
		s := newSetupGateTestServer(t)
		s.CountUsers = func(_ context.Context) (int, error) { return 0, nil }
		s.ClusterClaimed = nil
		rec := httptest.NewRecorder()
		s.handleSetupGet(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /setup with no claim signal wired: status = %d, want 404 (memql#3415)", rec.Code)
		}
	})
}

// TestSetupGate3415_FreshClusterStillServesSetup is the counterweight. The
// guard must not brick first-run setup: with both signals readable and both
// saying "unclaimed", the wizard renders and the POST persists. A guard that
// sealed a genuinely fresh cluster would be a different outage.
func TestSetupGate3415_FreshClusterStillServesSetup(t *testing.T) {
	s := newSetupGateTestServer(t)
	s.CountUsers = func(_ context.Context) (int, error) { return 0, nil }
	s.ClusterClaimed = func(_ context.Context) (bool, error) { return false, nil }

	rec := httptest.NewRecorder()
	s.handleSetupGet(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /setup on a fresh cluster: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestSetupGate3415_PreBootstrapRedirectRespectsClaimSignal covers the other
// half of the incident: with bootstrappedAt blank, `/` and `/login` 302'd to
// /setup and nobody could sign in. The claim signal must stop that redirect
// too -- an owner exists, so the cluster is NOT pre-bootstrap no matter what
// the stamp says.
func TestSetupGate3415_PreBootstrapRedirectRespectsClaimSignal(t *testing.T) {
	s := newSetupGateTestServer(t)
	s.CountUsers = func(_ context.Context) (int, error) { return 0, nil }
	s.ClusterClaimed = func(_ context.Context) (bool, error) { return true, nil }

	if s.preBootstrap(httptest.NewRequest(http.MethodGet, "/login", nil)) {
		t.Error("preBootstrap must be false when an owner user exists, whatever bootstrappedAt says (memql#3415)")
	}
}
