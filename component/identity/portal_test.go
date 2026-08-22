package identity

import "testing"

func TestPortalHomeURLFromClusterDomain(t *testing.T) {
	got := PortalHomeURL("memql.localhost", "https://identity.example.test")
	if got != "https://portal.memql.localhost/" {
		t.Fatalf("got %q", got)
	}
}

func TestPortalHomeURLFromIdentityBaseURL(t *testing.T) {
	got := PortalHomeURL("", "https://identity.memql.localhost")
	if got != "https://portal.memql.localhost/" {
		t.Fatalf("got %q", got)
	}
}

func TestPortalHomeURLEmptyWhenUnnameable(t *testing.T) {
	if got := PortalHomeURL("", "https://auth.example.com"); got != "" {
		t.Fatalf("unnameable origin must not invent a portal host; got %q", got)
	}
	if got := PortalHomeURL("", ""); got != "" {
		t.Fatalf("empty inputs: got %q", got)
	}
}

func TestDefaultPostLoginLandingNeverAdmin(t *testing.T) {
	if got := DefaultPostLoginLanding("memql.localhost", ""); got == "/admin/" || got == "" {
		t.Fatalf("named cluster must land on the portal, got %q", got)
	}
	if got := DefaultPostLoginLanding("", "https://auth.example.com"); got != "/me" {
		t.Fatalf("unnameable portal falls back to /me, not /admin/; got %q", got)
	}
	if got := DefaultPostLoginLanding("", ""); got != "/me" {
		t.Fatalf("empty: got %q", got)
	}
}
