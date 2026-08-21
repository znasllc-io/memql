package web

// Handler-level test for the /setup domain prefill (memql#4216).
//
// The incident: deploy/k8s/base/identity.yaml pins
// MEMQL_IDENTITY_BOOTSTRAP_DOMAIN=example.com as a fail-closed placeholder.
// envregistry.ApplyDomainDerivations copies MEMQL_DOMAIN onto that name
// set-if-absent, so the pin wins. handleSetupGet then prefills from
// Bootstrap.Domain and the operator sees example.com (or an empty box)
// on a keep-it install that already patched MEMQL_DOMAIN.
//
// The running install domain is MEMQL_DOMAIN (envFrom memql-domain).
// /setup must prefer that over the placeholder. A local install still
// prefills memql.localhost. The product default stays fail-closed;
// this test never names a vendor hostname.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func setupPrefillServer(t *testing.T) *Server {
	t.Helper()
	s := newSetupGateTestServer(t)
	s.CountUsers = func(_ context.Context) (int, error) { return 0, nil }
	s.ClusterClaimed = func(_ context.Context) (bool, error) { return false, nil }
	return s
}

func getSetupBody(t *testing.T, s *Server) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleSetupGet(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))
	return rec.Code, rec.Body.String()
}

func domainInputValue(body string) string {
	const mark = `id="domain"`
	i := strings.Index(body, mark)
	if i < 0 {
		return ""
	}
	chunk := body[i:]
	if end := strings.Index(chunk, ">"); end >= 0 {
		chunk = chunk[:end]
	}
	const prefix = `value="`
	v := strings.Index(chunk, prefix)
	if v < 0 {
		return ""
	}
	rest := chunk[v+len(prefix):]
	if end := strings.Index(rest, `"`); end >= 0 {
		return rest[:end]
	}
	return rest
}

// TestSetupPrefillsDomainFromMEMQLDomain is the keep-it case: the base
// placeholder is still on Bootstrap.Domain, MEMQL_DOMAIN is the install
// patch. The form must show the install domain.
func TestSetupPrefillsDomainFromMEMQLDomain(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "keepit.example")
	s := setupPrefillServer(t)
	s.Cfg.Bootstrap.Domain = "example.com"

	code, body := getSetupBody(t, s)
	if code != http.StatusOK {
		t.Fatalf("GET /setup: status = %d, want 200; body=%s", code, body)
	}
	got := domainInputValue(body)
	if got != "keepit.example" {
		t.Errorf("domain input value = %q, want MEMQL_DOMAIN %q (memql#4216)", got, "keepit.example")
	}
}

// TestSetupPrefillsLocalhostFromMEMQLDomain is the local install: the
// committed default is memql.localhost and /setup must show it, not
// the example.com placeholder.
func TestSetupPrefillsLocalhostFromMEMQLDomain(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "memql.localhost")
	s := setupPrefillServer(t)
	s.Cfg.Bootstrap.Domain = "example.com"

	code, body := getSetupBody(t, s)
	if code != http.StatusOK {
		t.Fatalf("GET /setup: status = %d, want 200; body=%s", code, body)
	}
	got := domainInputValue(body)
	if got != "memql.localhost" {
		t.Errorf("domain input value = %q, want memql.localhost (memql#4216)", got)
	}
}

// TestSetupPrefillsExplicitBootstrapWhenMEMQLDomainUnset keeps the
// unattended-deploy path honest: an operator who set
// MEMQL_IDENTITY_BOOTSTRAP_DOMAIN and not MEMQL_DOMAIN still sees
// that value.
func TestSetupPrefillsExplicitBootstrapWhenMEMQLDomainUnset(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "")
	s := setupPrefillServer(t)
	s.Cfg.Bootstrap.Domain = "acme.com"

	code, body := getSetupBody(t, s)
	if code != http.StatusOK {
		t.Fatalf("GET /setup: status = %d, want 200; body=%s", code, body)
	}
	got := domainInputValue(body)
	if got != "acme.com" {
		t.Errorf("domain input value = %q, want explicit bootstrap %q", got, "acme.com")
	}
}

// TestSetupLeavesDomainEmptyWhenOnlyExamplePlaceholder is the
// fail-closed base: both MEMQL_DOMAIN and the bootstrap pin are
// absent or the RFC example.com placeholder. The box stays empty so
// the operator types a real domain (memql#4216).
func TestSetupLeavesDomainEmptyWhenOnlyExamplePlaceholder(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "")
	s := setupPrefillServer(t)
	s.Cfg.Bootstrap.Domain = "example.com"

	code, body := getSetupBody(t, s)
	if code != http.StatusOK {
		t.Fatalf("GET /setup: status = %d, want 200; body=%s", code, body)
	}
	got := domainInputValue(body)
	if got != "" {
		t.Errorf("domain input value = %q, want empty (example.com is unset for this field)", got)
	}
}

// TestSetupFallsBackPastExampleComMEMQLDomain treats a MEMQL_DOMAIN
// of example.com the same as unset and uses a real bootstrap domain.
func TestSetupFallsBackPastExampleComMEMQLDomain(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "example.com")
	s := setupPrefillServer(t)
	s.Cfg.Bootstrap.Domain = "acme.com"

	code, body := getSetupBody(t, s)
	if code != http.StatusOK {
		t.Fatalf("GET /setup: status = %d, want 200; body=%s", code, body)
	}
	got := domainInputValue(body)
	if got != "acme.com" {
		t.Errorf("domain input value = %q, want real bootstrap %q", got, "acme.com")
	}
}
